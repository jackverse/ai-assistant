package clean

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"winclean/internal/safeio"
	"winclean/internal/sys"
	"winclean/internal/winapi"
)

// Stats 是一个候选测量后的完整画像。
type Stats struct {
	Exists bool `json:"exists"`

	// 范围内全部文件（逻辑大小与个数）
	Files int64 `json:"files"`
	Size  int64 `json:"size"`

	// 可安全回收 = 通过年龄过滤且未被占用的部分
	ReclaimableFiles int64 `json:"reclaimable_files"`
	Reclaimable      int64 `json:"reclaimable"`

	// 因年龄/占用被排除的部分（报告必须体现过滤结果，docs/design/06 §7.3）
	RecentFiles int64 `json:"recent_files"`
	RecentBytes int64 `json:"recent_bytes"`
	InUseFiles  int64 `json:"in_use_files"`
	InUseBytes  int64 `json:"in_use_bytes"`

	ReparseSkipped int64    `json:"reparse_skipped"`
	DownloadFiles  []FilePP `json:"download_files,omitempty"`
}

// FilePP 是下载目录列表里的一项（pp = present to user）。
type FilePP struct {
	Path      string `json:"path"`
	Name      string `json:"name"`
	IsDir     bool   `json:"is_dir"`
	Size      int64  `json:"size"`
	AgeDays   int64  `json:"age_days"`
	Suggested bool   `json:"suggested"` // 仅作建议标记，勾选权在用户
}

// Measure 测量一个候选：范围内的总量与「当前实际可回收量」。
//
// 占用检测逐文件做：以零共享模式尝试打开，共享冲突即判定被占用。
// 这个检查同时服务测量（可回收量）与执行（删除前再查一次），
// 两处结论一致，不会出现「测量说能删、执行却删不掉」的意外。
func Measure(ctx context.Context, c Candidate) (Stats, error) {
	st := Stats{Exists: false}
	for _, root := range c.Paths {
		if root == "" {
			continue
		}
		if _, err := winapi.GetFileAttributes(sys.ToExtendedPath(root)); err != nil {
			continue
		}
		st.Exists = true

		switch c.Kind {
		case KindDownloads:
			list, err := listDownloads(ctx, root)
			if err != nil {
				return st, err
			}
			st.DownloadFiles = append(st.DownloadFiles, list...)
			for _, f := range st.DownloadFiles {
				st.Files++
				st.Size += f.Size
			}
			// 下载目录没有「默认可回收」概念——全部待用户勾选
		case KindFile:
			// Paths 是精确的单个文件（如 MEMORY.DMP）
			for _, p := range c.Paths {
				fi, err := statFile(p)
				if err != nil {
					continue // 不存在：候选已在 Catalog 阶段按存在性过滤
				}
				st.Exists = true
				st.Files++
				st.Size += fi
				if inUse, _ := winapi.IsFileInUse(sys.ToExtendedPath(p)); inUse {
					st.InUseFiles++
					st.InUseBytes += fi
					continue
				}
				st.ReclaimableFiles++
				st.Reclaimable += fi
			}
		case KindFiles:
			entries, err := listMatchingFiles(root, c.Patterns)
			if err != nil {
				continue
			}
			for _, e := range entries {
				st.Files++
				st.Size += e.Size
				if !ageOk(e.MtimeNano, c.MinAge) {
					st.RecentFiles++
					st.RecentBytes += e.Size
					continue
				}
				inUse, _ := winapi.IsFileInUse(sys.ToExtendedPath(e.Path))
				if inUse {
					st.InUseFiles++
					st.InUseBytes += e.Size
					continue
				}
				st.ReclaimableFiles++
				st.Reclaimable += e.Size
			}
		default: // KindDir
			_, err := safeio.Walk(ctx, root, safeio.WalkerConfig{}, func(e safeio.Entry) error {
				if e.IsDir {
					return nil
				}
				st.Files++
				st.Size += e.Size
				if !ageOk(e.MtimeNano, c.MinAge) {
					st.RecentFiles++
					st.RecentBytes += e.Size
					return nil
				}
				inUse, _ := winapi.IsFileInUse(sys.ToExtendedPath(e.Path))
				if inUse {
					st.InUseFiles++
					st.InUseBytes += e.Size
					return nil
				}
				st.ReclaimableFiles++
				st.Reclaimable += e.Size
				return nil
			})
			if err != nil {
				return st, err
			}
		}
	}
	return st, nil
}

// ageOk 报告文件最后修改时间是否早于最小年龄。
// MinAge 为 0 时全部通过。mtime 无效（0）视为过老（可删方向保守吗？
// 无效时间戳的文件几乎只存在于损坏的文件系统上，选择「可删」）。
func ageOk(mtimeNano int64, minAge time.Duration) bool {
	if minAge <= 0 {
		return true
	}
	if mtimeNano == 0 {
		return true
	}
	return time.Since(time.Unix(0, mtimeNano)) >= minAge
}

// installerExts 会给出删除建议的安装包扩展名（小写）。
var installerExts = map[string]bool{
	".exe": true, ".msi": true, ".zip": true, ".rar": true, ".7z": true, ".iso": true,
}

// docExts 明确不给删除建议的文档/媒体类型（docs/design/06 §6.1）。
// 注意：只是「不给建议」，用户仍可显式勾选删除。
var docExts = map[string]bool{
	".doc": true, ".docx": true, ".xls": true, ".xlsx": true, ".ppt": true, ".pptx": true,
	".pdf": true, ".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".heic": true,
}

// suggestAge 安装包给出建议的最小年龄。
const suggestAge = 90 * 24 * time.Hour

// listDownloads 列出下载目录的第一层内容，按大小降序，上限 200 条。
//
// 子目录只列聚合大小、不展开：下载目录里可能有一个解压出来的几 GB 文件夹，
// 用户需要知道它多大才能做决定；但展开它的内部既慢也没必要。
func listDownloads(ctx context.Context, root string) ([]FilePP, error) {
	ext := sys.ToExtendedPath(root + `\*`)
	h, data, err := winapi.FindFirstFileEx(ext)
	if err != nil {
		return nil, err
	}
	defer winapi.FindClose(h)

	var out []FilePP
	for {
		name := syscall.UTF16ToString(data.FileName[:])
		if name != "." && name != ".." {
			p := root + `\` + name
			if data.IsDir() {
				var sz int64
				if sum, err := safeio.SumTree(ctx, p); err == nil {
					sz = sum.Bytes
				}
				out = append(out, FilePP{
					Path: p, Name: name, IsDir: true,
					Size:    sz,
					AgeDays: ageDays(mtimeOf(data)),
				})
			} else if !data.IsReparsePoint() {
				age := ageDays(mtimeOf(data))
				e := strings.ToLower(filepath.Ext(name))
				suggested := installerExts[e] && !docExts[e] && age >= 90
				out = append(out, FilePP{
					Path: p, Name: name,
					Size:      data.Size(),
					AgeDays:   age,
					Suggested: suggested,
				})
			}
		}
		if err := winapi.FindNextFile(h, data); err != nil {
			break
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Size > out[j].Size })
	if len(out) > 200 {
		out = out[:200]
	}
	return out, nil
}

// listMatchingFiles 列出目录内匹配任一模式的文件（不递归）。
func listMatchingFiles(dir string, patterns []string) ([]safeio.Entry, error) {
	var out []safeio.Entry
	ext := sys.ToExtendedPath(dir + `\*`)
	h, data, err := winapi.FindFirstFileEx(ext)
	if err != nil {
		return nil, err
	}
	defer winapi.FindClose(h)
	for {
		name := syscall.UTF16ToString(data.FileName[:])
		if name != "." && name != ".." && !data.IsDir() {
			for _, p := range patterns {
				if ok, _ := filepath.Match(strings.ToLower(p), strings.ToLower(name)); ok {
					out = append(out, safeio.Entry{
						Path:      dir + `\` + name,
						Name:      name,
						Size:      data.Size(),
						MtimeNano: mtimeOf(data),
					})
					break
				}
			}
		}
		if err := winapi.FindNextFile(h, data); err != nil {
			break
		}
	}
	return out, nil
}

// statFile 返回单个文件的逻辑大小；不存在返回错误。
func statFile(p string) (int64, error) {
	h, data, err := winapi.FindFirstFileEx(sys.ToExtendedPath(p))
	if err != nil {
		return 0, err
	}
	defer winapi.FindClose(h)
	return data.Size(), nil
}

func ageDays(mtimeNano int64) int64 {
	if mtimeNano == 0 {
		return -1
	}
	return int64(time.Since(time.Unix(0, mtimeNano)).Hours() / 24)
}

func mtimeOf(data *winapi.Win32FindData) int64 {
	ns := data.LastWriteTime.Nanoseconds()
	if ns == 0 {
		return 0
	}
	return (ns - 116444736000000000) * 100
}
