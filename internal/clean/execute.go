package clean

import (
	"context"
	"fmt"
	"sort"
	"syscall"
	"time"

	"winclean/internal/safeio"
	"winclean/internal/sys"
	"winclean/internal/winapi"
)

// Selection 是用户对候选的最终决定。
type Selection struct {
	ID string
	// Files 只对 KindDownloads 生效：用户勾选的精确文件/目录列表。
	// 其余类型删除范围完全由候选目录本身决定，不接受外部路径——
	// 这保证「能删什么」永远出自内置白名单，而不是任意输入。
	Files []string
}

// Result 是一个候选的执行结果（给用户看的口径，docs/design/06 §7.3）。
type Result struct {
	ID            string   `json:"id"`
	OK            bool     `json:"ok"`
	FreedBytes    int64    `json:"freed_bytes"`
	DeletedFiles  int64    `json:"deleted_files"`
	SkippedInUse  int64    `json:"skipped_in_use"`
	SkippedRecent int64    `json:"skipped_recent"`
	SkippedReparse int64   `json:"skipped_reparse"`
	Errors        int64    `json:"errors"`
	Samples       []string `json:"samples,omitempty"`
}

// Progress 是执行过程的进度回调参数。
type Progress struct {
	CandidateID   string
	CandidateName string
	FreedBytes    int64
	DeletedFiles  int64
}

// Execute 执行一批已勾选的清理候选。
//
// 安全设计：
//   - 每个候选执行前重过 GuardCheck（禁止根护栏）；
//   - 文件删除前重新检查年龄与占用（TOCTOU：勾选到执行之间文件可能变化）；
//   - 重解析点（目录或文件）一律跳过并计数，绝不删除；
//   - 目录自底向上摘除，失败（仍有内容）保留不算错误；
//   - 单文件失败只累计不中止——缓存清理里被占用是常态。
func Execute(ctx context.Context, cands []Candidate, sel []Selection, prog func(Progress)) []Result {
	byID := make(map[string]Candidate, len(cands))
	for _, c := range cands {
		byID[c.ID] = c
	}

	var out []Result
	for _, s := range sel {
		c, ok := byID[s.ID]
		if !ok {
			out = append(out, Result{ID: s.ID, Samples: []string{"未知候选，已跳过"}})
			continue
		}
		res := executeOne(ctx, c, s, prog)
		out = append(out, res)
		if ctx.Err() != nil {
			break
		}
	}
	return out
}

func executeOne(ctx context.Context, c Candidate, s Selection, prog func(Progress)) Result {
	res := Result{ID: c.ID}

	// 护栏重查：内置白名单也要过这一关
	for _, p := range c.Paths {
		if err := GuardCheck(p); err != nil {
			res.Samples = append(res.Samples, err.Error())
			return res
		}
	}

	var totalFreed, totalFiles int64
	note := func(freedDelta, filesDelta int64) {
		totalFreed += freedDelta
		totalFiles += filesDelta
		if prog != nil {
			prog(Progress{
				CandidateID: c.ID, CandidateName: c.Title,
				FreedBytes: totalFreed, DeletedFiles: totalFiles,
			})
		}
	}

	fail := func(path string, err error) {
		res.Errors++
		if len(res.Samples) < 5 {
			res.Samples = append(res.Samples, path+": "+err.Error())
		}
	}

	switch c.Kind {
	case KindDownloads:
		// 只删用户显式勾选的精确路径，且必须仍在该候选目录之下
		for _, f := range s.Files {
			if ctx.Err() != nil {
				break
			}
			abs, err := sys.CleanAbsolute(f)
			if err != nil {
				fail(f, err)
				continue
			}
			inside := false
			for _, root := range c.Paths {
				if sys.IsUnder(abs, root) {
					inside = true
					break
				}
			}
			if !inside {
				fail(f, fmt.Errorf("路径不在候选目录内，拒绝删除"))
				continue
			}
			n, freed, err := deletePath(ctx, abs, &res)
			if err != nil {
				fail(abs, err)
				continue
			}
			note(freed, n)
		}

	case KindFile:
		// 精确单文件：删除前重新校验存在性与占用
		for _, p := range c.Paths {
			if ctx.Err() != nil {
				break
			}
			sz, err := osStat(p)
			if err != nil {
				continue // 已不存在：无事可做
			}
			if inUse, _ := winapi.IsFileInUse(sys.ToExtendedPath(p)); inUse {
				res.SkippedInUse++
				continue
			}
			if err := syscall.DeleteFile(syscall.StringToUTF16Ptr(sys.ToExtendedPath(p))); err != nil {
				fail(p, err)
				continue
			}
			note(sz, 1)
		}

	case KindFiles:
		for _, root := range c.Paths {
			entries, err := listMatchingFiles(root, c.Patterns)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if ctx.Err() != nil {
					break
				}
				if !ageOk(e.MtimeNano, c.MinAge) {
					res.SkippedRecent++
					continue
				}
				if inUse, _ := winapi.IsFileInUse(sys.ToExtendedPath(e.Path)); inUse {
					res.SkippedInUse++
					continue
				}
				if err := syscall.DeleteFile(syscall.StringToUTF16Ptr(sys.ToExtendedPath(e.Path))); err != nil {
					fail(e.Path, err)
					continue
				}
				note(e.Size, 1)
			}
		}

	default: // KindDir
		for _, root := range c.Paths {
			freed, files, recent, inuse, reparse, err := clearDirContents(ctx, root, c.MinAge, func(f, n int64) {
				note(f, n)
			}, &res)
			if err != nil {
				fail(root, err)
			}
			res.SkippedRecent += recent
			res.SkippedInUse += inuse
			res.SkippedReparse += reparse
			_ = freed
			_ = files
		}
	}

	res.FreedBytes = totalFreed
	res.DeletedFiles = totalFiles
	res.OK = res.Errors == 0
	return res
}

// clearDirContents 清空目录内「通过年龄与占用检查」的文件，
// 然后自底向上摘除空目录。返回 (累计释放字节, 累计文件数, 跳过口径计数)。
func clearDirContents(ctx context.Context, root string, minAge time.Duration, note func(freed, files int64), res *Result) (freed, files, recent, inuse, reparse int64, err error) {
	var (
		filesToDelete []safeio.Entry
		dirs          []safeio.Entry // 父先于子
	)
	_, werr := safeio.Walk(ctx, root, safeio.WalkerConfig{VisitReparse: true}, func(e safeio.Entry) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if e.IsDir {
			dirs = append(dirs, e)
			if e.IsReparse {
				reparse++
			}
			return nil
		}
		if e.IsReparse {
			reparse++
			return nil
		}
		if !ageOk(e.MtimeNano, minAge) {
			recent++
			return nil
		}
		filesToDelete = append(filesToDelete, e)
		return nil
	})
	if werr != nil {
		return 0, 0, recent, inuse, reparse, werr
	}

	for _, e := range filesToDelete {
		if ctx.Err() != nil {
			break
		}
		if inU, _ := winapi.IsFileInUse(sys.ToExtendedPath(e.Path)); inU {
			inuse++
			continue
		}
		if err := syscall.DeleteFile(syscall.StringToUTF16Ptr(sys.ToExtendedPath(e.Path))); err != nil {
			res.Errors++
			if len(res.Samples) < 5 {
				res.Samples = append(res.Samples, e.Path+": "+err.Error())
			}
			continue
		}
		freed += e.Size
		files++
		note(freed, files)
	}

	// 自底向上摘目录（后序）；失败说明仍有内容，保留即可。
	for i := len(dirs) - 1; i >= 0; i-- {
		if ctx.Err() != nil {
			break
		}
		d := dirs[i]
		if d.IsReparse {
			continue // 别人的链接不动
		}
		_ = syscall.RemoveDirectory(syscall.StringToUTF16Ptr(sys.ToExtendedPath(d.Path)))
	}
	return freed, files, recent, inuse, reparse, nil
}

// deletePath 删除一个精确路径（文件或目录），返回 (删除文件数, 释放字节)。
func deletePath(ctx context.Context, abs string, res *Result) (int64, int64, error) {
	isReparse, _, err := winapi.IsReparsePointRaw(sys.ToExtendedPath(abs))
	if err != nil {
		return 0, 0, err
	}
	if isReparse {
		res.SkippedReparse++
		return 0, 0, fmt.Errorf("路径是重解析点，已跳过")
	}

	// 是目录还是文件？先按文件删；报「拒绝访问/是目录」再按树删。
	if err := syscall.DeleteFile(syscall.StringToUTF16Ptr(sys.ToExtendedPath(abs))); err == nil {
		if fi, serr := osStat(abs); serr == nil {
			return 1, fi, nil
		}
		return 1, 0, nil
	}

	// 目录：走安全树删除（删内容 + 空目录，删除中逐项重校验重解析状态）
	freed, files, _, err := safeio.RemoveTree(ctx, abs, nil)
	return files, freed, err
}

// osStat 返回文件逻辑大小（FindFirstFile 精确查询以支持长路径）。
func osStat(p string) (int64, error) {
	h, data, err := winapi.FindFirstFileEx(sys.ToExtendedPath(p))
	if err != nil {
		return 0, err
	}
	defer winapi.FindClose(h)
	return data.Size(), nil
}

// SortCandidates 把候选拼成稳定的展示顺序：L2 在前、按 ID。
func SortCandidates(cands []Candidate) {
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].Level != cands[j].Level {
			return cands[i].Level == LevelSafe
		}
		return cands[i].ID < cands[j].ID
	})
}

// BlockedReason 返回候选因环境不满足（如未提权）而被阻断的原因；空串表示可用。
func BlockedReason(c Candidate, elevated bool) string {
	if c.AdminRequired && !elevated {
		return "需要管理员权限（当前程序未提权）"
	}
	return ""
}
