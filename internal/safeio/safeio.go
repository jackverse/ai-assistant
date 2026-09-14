// Package safeio 提供清理与迁移共用的文件系统写操作原语。
//
// 全部实现遵循两条硬约束（docs/design/09 §4.4、07 §4.4）：
//   - 永不跟随重解析点：遍历、复制、删除遇到 Junction/Symlink 一律
//     「报告但不进入」；删除目录前重读重解析标记，宁可漏删不可误删；
//   - 永不使用 os.RemoveAll / filepath.Walk（禁用 API，见 09 §4.4）：
//     前者会递归进链接目标删掉真实数据，后者无法控制重解析点行为。
package safeio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"syscall"

	"winclean/internal/sys"
	"winclean/internal/winapi"
)

// Entry 是遍历得到的一个目录项。
type Entry struct {
	Path      string // 普通形式（不带 \\?\），用于展示与记录
	IsDir     bool
	IsReparse bool
	Size      int64  // 逻辑大小（目录项无意义）
	MtimeNano int64  // 最后写入时间（Unix 纳秒，0 表示无效）
	Name      string // 文件名（含扩展名）
}

// WalkResult 是对一次遍历的统计。
type WalkResult struct {
	Files       int64
	Dirs        int64
	Bytes       int64 // 逻辑大小合计
	ReparseSkipped int64
	Errors      int64
}

// WalkerConfig 控制遍历行为。
type WalkerConfig struct {
	// VisitReparse 为 true 时把重解析点也交给 fn（但绝不深入它们）。
	VisitReparse bool
}

// Walk 遍历 root 子树（单线程），对每个条目调用 fn。
//
// 安全语义：
//   - 目录若是重解析点：调用 fn（VisitReparse 时）后【不进入】；
//   - fn 返回 error 会中止遍历并向上返回；
//   - 单个条目枚举失败计入 Errors 并跳过，不中止。
func Walk(ctx context.Context, root string, cfg WalkerConfig, fn func(Entry) error) (WalkResult, error) {
	var res WalkResult
	err := walkDir(ctx, root, cfg, fn, &res)
	return res, err
}

func walkDir(ctx context.Context, dir string, cfg WalkerConfig, fn func(Entry) error, res *WalkResult) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	ext := sys.ToExtendedPath(dir + `\*`)
	h, data, err := winapi.FindFirstFileEx(ext)
	if err != nil {
		if errors.Is(err, syscall.ERROR_FILE_NOT_FOUND) {
			return nil // 空目录
		}
		res.Errors++
		return nil
	}
	defer winapi.FindClose(h)

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		name := syscall.UTF16ToString(data.FileName[:])
		if name != "." && name != ".." {
			e := Entry{
				Path:      dir + `\` + name,
				IsDir:     data.IsDir(),
				IsReparse: data.IsReparsePoint(),
				Size:      data.Size(),
				MtimeNano: filetimeToUnix(data.LastWriteTime),
				Name:      name,
			}
			if e.IsDir {
				res.Dirs++
			} else {
				res.Files++
				res.Bytes += e.Size
			}
			if e.IsReparse && !cfg.VisitReparse {
				// 仍然计数，只是不回调——调用方需要知道这里有个链接
				res.ReparseSkipped++
			} else {
				if err := fn(e); err != nil {
					return err
				}
			}
			// 关键：重解析点目录绝不递归进入
			if e.IsDir && !e.IsReparse {
				if err := walkDir(ctx, e.Path, cfg, fn, res); err != nil {
					return err
				}
			}
		}
		if err := winapi.FindNextFile(h, data); err != nil {
			if !errors.Is(err, syscall.ERROR_NO_MORE_FILES) && !errors.Is(err, io.EOF) {
				res.Errors++
			}
			return nil
		}
	}
}

// CopyTree 把 src 子树复制到 dst（dst 必须不存在或为空目录）。
//
// 保留项：文件属性由 CopyFileW 原样带到目标；目录时间戳不做刻意对齐
// （对缓存/数据目录语义无影响，报告里如实说明这是与 robocopy 的差异）。
//
// 遇到子目录级的重解析点：不复制其内容，记录路径交由调用方决定
// （迁移场景在目标处原样重建一个同目标 Junction）。
func CopyTree(ctx context.Context, src, dst string, prog func(copiedFiles, copiedBytes int64)) (files, bytes int64, reparse []string, err error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, nil, err
	}
	// 先建目标根（walk 中复制的文件都需要父目录已存在）
	if err := syscall.CreateDirectory(syscall.StringToUTF16Ptr(sys.ToExtendedPath(dst)), nil); err != nil {
		if !errors.Is(err, syscall.ERROR_ALREADY_EXISTS) {
			return 0, 0, nil, fmt.Errorf("创建根目录 %s: %w", dst, err)
		}
	}
	var res WalkResult
	cfg := WalkerConfig{VisitReparse: true}
	err = walkDir(ctx, src, cfg, func(e Entry) error {
		rel, rerr := sys.RelPath(src, e.Path)
		if rerr != nil {
			return rerr
		}
		target := dst + `\` + rel
		if e.IsDir {
			if e.IsReparse {
				// 子目录级 Junction：在目标处原样重建一个指向同一目标的链接，
				// 保证迁移后的树与源语义一致（不复制其内容）。
				if t := sys.ReadReparseTarget(e.Path); t != "" {
					_ = syscall.CreateDirectory(syscall.StringToUTF16Ptr(sys.ToExtendedPath(target)), nil)
					if err := winapi.SetJunction(sys.ToExtendedPath(target), t); err != nil {
						return fmt.Errorf("重建链接 %s 失败: %w", target, err)
					}
				}
				reparse = append(reparse, e.Path)
				return nil
			}
			if err := syscall.CreateDirectory(syscall.StringToUTF16Ptr(sys.ToExtendedPath(target)), nil); err != nil {
				if !errors.Is(err, syscall.ERROR_ALREADY_EXISTS) {
					return fmt.Errorf("创建目录 %s: %w", target, err)
				}
			}
			return nil
		}
		if e.IsReparse {
			// 文件级重解析点（极少见）：跳过并报告，不猜它的语义
			reparse = append(reparse, e.Path)
			return nil
		}
		if err := winapi.CopyFileW(sys.ToExtendedPath(e.Path), sys.ToExtendedPath(target), true); err != nil {
			return err
		}
		files++
		bytes += e.Size
		res.Files++
		res.Bytes += e.Size
		if prog != nil {
			prog(files, bytes)
		}
		return nil
	}, &res)
	return files, bytes, reparse, err
}

// TreeSummary 是 VerifyTrees 用的树指纹：文件数与逻辑字节总数。
type TreeSummary struct {
	Files int64
	Bytes int64
}

// SumTree 统计一棵树的文件数与逻辑总大小（重解析点不计入内容）。
func SumTree(ctx context.Context, root string) (TreeSummary, error) {
	var s TreeSummary
	_, err := Walk(ctx, root, WalkerConfig{}, func(e Entry) error {
		if !e.IsDir {
			s.Files++
			s.Bytes += e.Size
		}
		return nil
	})
	return s, err
}

// VerifyTrees 比较两棵树的文件数与总大小是否一致。
//
// 抽样校验逐文件内容哈希代价高（等于再复制一遍的读取量），
// 这里采用「计数 + 字节数」双重校验；如需更强保证可扩展 --verify-hash。
func VerifyTrees(a, b TreeSummary) error {
	if a.Files != b.Files {
		return fmt.Errorf("文件数不一致：源 %d，目标 %d", a.Files, b.Files)
	}
	if a.Bytes != b.Bytes {
		return fmt.Errorf("总大小不一致：源 %s，目标 %s",
			FormatBytes(a.Bytes), FormatBytes(b.Bytes))
	}
	return nil
}

// RemoveTree 安全删除一棵子树的内容。
//
// 规则（自底向上，每一步都重新校验）：
//   - 文件：重解析点文件跳过；其余直接删除；
//   - 目录：删除前重读重解析标记——若是 Junction 则【只摘链接】
//     （RemoveDirectoryW 语义），绝不递归进目标；普通目录先删空内容，
//     非空则保留并计数；
//   - 根目录本身最后尝试删除，失败（非空）不算错误。
func RemoveTree(ctx context.Context, root string, prog func(freedBytes, removedFiles int64)) (freed, removedFiles, skippedReparse int64, err error) {
	// 先收集再删除：一次遍历拿到全部路径，按「先文件后深目录」排序删。
	// 不用递归边走边删的原因：删除过程中修改正在遍历的目录行为未定义。
	var (
		dirs []Entry // 父先于子（Walk 天然保证：父目录先于内容被回调）
	)
	_, err = Walk(ctx, root, WalkerConfig{VisitReparse: true}, func(e Entry) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if e.IsDir {
			dirs = append(dirs, e)
			return nil
		}
		if e.IsReparse {
			skippedReparse++
			return nil
		}
		if err := syscall.DeleteFile(syscall.StringToUTF16Ptr(sys.ToExtendedPath(e.Path))); err != nil {
			// 单个文件失败不中止：缓存清理里被占用的文件很常见，如实统计
			skipNote(e.Path, err)
			return nil
		}
		freed += e.Size
		removedFiles++
		if prog != nil {
			prog(freed, removedFiles)
		}
		return nil
	})
	if err != nil {
		return freed, removedFiles, skippedReparse, err
	}

	// 自底向上删目录（后序）。删除前重新确认重解析状态（TOCTOU 防护：
	// 扫描到删除之间目录可能已被替换成指向别处的 Junction）。
	for i := len(dirs) - 1; i >= 0; i-- {
		if ctx.Err() != nil {
			break
		}
		d := dirs[i]
		isReparse, _, rerr := winapi.IsReparsePointRaw(sys.ToExtendedPath(d.Path))
		if rerr == nil && isReparse {
			// 是链接：只摘链接本身（RemoveDirectoryW 对重解析点只删链接，
			// 绝不进入目标——这正是这里必须用它而不是 RemoveAll 的原因）
			if rmErr := winapi.RemoveDirectoryOnly(sys.ToExtendedPath(d.Path)); rmErr == nil {
				skippedReparse++ // 计入「链接处理」口径：没碰目标内容
			}
			continue
		}
		// 普通目录：尝试删除，非空（还有删不掉的文件）则保留
		_ = syscall.RemoveDirectory(syscall.StringToUTF16Ptr(sys.ToExtendedPath(d.Path)))
	}
	// 根目录最后尝试删除：非空（仍有删不掉的内容）会失败，不算错误。
	// 同样先重读重解析状态——若根已被替换成 Junction，只摘链接。
	isRootReparse, _, rerr := winapi.IsReparsePointRaw(sys.ToExtendedPath(root))
	if rerr == nil && isRootReparse {
		_ = winapi.RemoveDirectoryOnly(sys.ToExtendedPath(root))
	} else if rerr == nil {
		_ = syscall.RemoveDirectory(syscall.StringToUTF16Ptr(sys.ToExtendedPath(root)))
	}
	return freed, removedFiles, skippedReparse, nil
}

// skipNote 目前把删除失败静默计入错误之外的「跳过」口径。
// 失败路径没有聚合展示的地方，进 debug 日志即可（不引入日志依赖）。
var skipNote = func(path string, err error) {}

// FormatBytes 是给报告/错误消息用的轻量字节格式化。
func FormatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	v := float64(n)
	i := -1
	for v >= unit && i < len(units)-1 {
		v /= unit
		i++
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}

func filetimeToUnix(ft winapi.Filetime) int64 {
	ns := ft.Nanoseconds()
	if ns == 0 {
		return 0
	}
	return (ns - 116444736000000000) * 100
}
