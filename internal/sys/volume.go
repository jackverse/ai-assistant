// Package sys 是「Windows 系统事实」的唯一入口，屏蔽 syscall 细节。
// 本层不含业务判断——那是 analyze/* 的职责。
package sys

import (
	"fmt"
	"path/filepath"
	"strings"
	"syscall"

	"winclean/internal/model"
	"winclean/internal/winapi"
)

// EnumerateVolumes 枚举本机所有固定磁盘。
//
// 只返回固定磁盘（DRIVE_FIXED）：光驱、网络盘、可移动盘、未挂载卷一律排除。
// 排除它们的原因不是「不能扫」，而是扫了没有意义且慢，且网络盘可能触发意外的 IO。
func EnumerateVolumes() ([]model.Volume, error) {
	mask, err := winapi.GetLogicalDrives()
	if err != nil {
		return nil, err
	}

	systemRoot, _ := SystemVolumeRoot()

	var vols []model.Volume
	for i := 0; i < 26; i++ {
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		letter := string(rune('A' + i))
		root := letter + `:\`

		if winapi.GetDriveType(root) != winapi.DRIVE_FIXED {
			continue
		}

		v := model.Volume{
			Root:   root,
			Letter: letter,
		}

		if info, err := winapi.GetVolumeInformation(root); err == nil {
			v.Label = info.Label
			v.FileSystem = info.FileSystem
			v.SerialNumber = info.SerialNumber
			v.IsNTFS = strings.EqualFold(info.FileSystem, "NTFS")
		} else {
			// 卷信息读不到（例如 BitLocker 锁定、权限不足）时保留占位，
			// 但不把该卷标记为 NTFS —— 迁移预检会因此拒绝它，这是安全的方向。
			v.FileSystem = "unknown"
		}

		if space, err := winapi.GetDiskFreeSpace(root); err == nil {
			v.TotalBytes = space.Total
			v.FreeBytes = space.TotalFree
			v.UsedBytes = space.Total - space.TotalFree
		}

		if cluster, err := winapi.ClusterSize(root); err == nil {
			v.BytesPerCluster = cluster
		}
		if v.BytesPerCluster == 0 {
			v.BytesPerCluster = 4096 // 保守兜底，仅影响 OnDisk 估算
		}

		if systemRoot != "" && strings.EqualFold(root, systemRoot) {
			v.IsSystem = true
			v.IsBoot = true
		}

		vols = append(vols, v)
	}

	return vols, nil
}

// SystemVolumeRoot 返回 Windows 安装目录所在卷的根（如 "C:\"）。
func SystemVolumeRoot() (string, error) {
	dir, err := winapi.GetWindowsDirectory()
	if err != nil {
		return "", err
	}
	return VolumeRootOf(dir), nil
}

// VolumeRootOf 从任意路径提取卷根（"C:\"）。
// 支持 UNC 路径（返回 "\\server\share\"）。
func VolumeRootOf(path string) string {
	if len(path) >= 2 && path[1] == ':' {
		return strings.ToUpper(path[:1]) + `:\`
	}
	if strings.HasPrefix(path, `\\`) {
		parts := strings.SplitN(strings.TrimPrefix(path, `\\`), `\`, 3)
		if len(parts) >= 2 {
			root := `\\` + parts[0] + `\` + parts[1] + `\`
			return root
		}
	}
	return ""
}

// VolumeOf 返回路径所在的盘符字母（"C"）。
func VolumeOf(path string) string {
	root := VolumeRootOf(path)
	if len(root) >= 2 && root[1] == ':' {
		return strings.ToUpper(root[:1])
	}
	return ""
}

// ToExtendedPath 把路径转换为 \\?\ 扩展形式。
//
// 为什么必须这样做（见 docs/design/03-扫描引擎.md §2.2）：
//   - 绕过 MAX_PATH=260 限制，否则深层 node_modules 之类的路径会直接失败
//   - \\?\ 前缀会【禁用】路径规范化，因此必须先用 CleanAbsolute 处理干净，
//     绝不把未清理的输入直接加前缀——那只是掩盖问题
func ToExtendedPath(abs string) string {
	if strings.HasPrefix(abs, `\\?\`) {
		return abs
	}
	if strings.HasPrefix(abs, `\\`) {
		return `\\?\UNC\` + strings.TrimPrefix(abs, `\\`)
	}
	return `\\?\` + abs
}

// FromExtendedPath 去掉 \\?\ 前缀，还原为常规路径以便展示。
func FromExtendedPath(p string) string {
	if strings.HasPrefix(p, `\\?\UNC\`) {
		return `\\` + strings.TrimPrefix(p, `\\?\UNC\`)
	}
	return strings.TrimPrefix(p, `\\?\`)
}

// CleanAbsolute 把路径清理为不含相对成分的绝对路径。
//
// 这是安全校验的第一步：任何来自用户、规则库或扫描结果的路径，
// 在参与比较或 API 调用前都必须经过它，否则 ".." 可以逃出预期范围。
func CleanAbsolute(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("路径为空")
	}
	if strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("路径含 NUL 字节")
	}

	// 统一分隔符：接受用户输入的 / 与 \
	p = strings.ReplaceAll(p, "/", `\`)

	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("无法解析为绝对路径 %q: %w", p, err)
	}
	abs = filepath.Clean(abs)

	if VolumeRootOf(abs) == "" {
		return "", fmt.Errorf("路径没有卷根 %q", p)
	}
	return abs, nil
}

// IsUnder 报告 child 是否位于 parent 之下（含相等）。
//
// 比较前双方都应已 CleanAbsolute，且比较是大小写不敏感的
// （Windows 文件系统不区分大小写）。
//
// 实现上特意在 parent 后补分隔符再比较，避免 "C:\WindowsOld" 被误判为
// "C:\Windows" 的子路径——这是路径前缀校验的经典漏洞。
func IsUnder(child, parent string) bool {
	c := strings.ToLower(strings.TrimRight(child, `\`))
	p := strings.ToLower(strings.TrimRight(parent, `\`))
	if c == p {
		return true
	}
	return strings.HasPrefix(c, p+`\`)
}

// RelPath 返回 child 相对 parent 的子路径（child 必须在 parent 之下）。
// 仅用于同一遍历过程内由拼接产生的路径，大小写保证一致。
func RelPath(parent, child string) (string, error) {
	p := strings.TrimRight(parent, `\`)
	c := strings.TrimRight(child, `\`)
	if c == p {
		return "", nil
	}
	if !strings.HasPrefix(c, p+`\`) {
		return "", fmt.Errorf("路径 %s 不在 %s 之下", child, parent)
	}
	return c[len(p)+1:], nil
}

// JoinPath 是 filepath.Join 的包装，额外保证结果通过 CleanAbsolute。
func JoinPath(base, elem string) (string, error) {
	return CleanAbsolute(filepath.Join(base, elem))
}

// IsReparsePoint 报告路径是否为重解析点（不跟随链接本身）。
func IsReparsePoint(path string) (bool, uint32, error) {
	attrs, err := winapi.GetFileAttributes(ToExtendedPath(path))
	if err != nil {
		return false, 0, err
	}
	if attrs&winapi.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
		return false, 0, nil
	}
	info, err := winapi.ReadReparsePoint(ToExtendedPath(path))
	if err != nil {
		// 属性说是重解析点但读不出标记：保守地当作重解析点处理（不递归进去）。
		return true, 0, nil
	}
	return true, info.Tag, nil
}

// ReadReparseTarget 读取重解析点目标，用于报告展示。
func ReadReparseTarget(path string) string {
	info, err := winapi.ReadReparsePoint(ToExtendedPath(path))
	if err != nil {
		return ""
	}
	return info.Target()
}

// 便于上层判断特定 Win32 错误。
var (
	ErrAccessDenied = syscall.ERROR_ACCESS_DENIED
	ErrFileNotFound = syscall.ERROR_FILE_NOT_FOUND
	ErrPathNotFound = syscall.ERROR_PATH_NOT_FOUND
	// ErrCantAccessFile 常见于云占位文件未下载。
	ErrCantAccessFile = syscall.Errno(1920)
)
