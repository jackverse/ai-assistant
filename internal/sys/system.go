package sys

import (
	"path/filepath"
	"strings"

	"winclean/internal/winapi"
)

// SystemRoot 返回 Windows 安装目录（如 "C:\Windows"）。
func SystemRoot() (string, error) {
	dir, err := winapi.GetWindowsDirectory()
	if err != nil {
		return "", err
	}
	return filepath.Clean(dir), nil
}

// IsElevated 报告当前进程是否具有管理员权限。
func IsElevated() bool {
	return winapi.IsElevated()
}

// DriveTypeName 把 Win32 驱动器类型编号转成可读名称。
func DriveTypeName(t uint32) string {
	switch t {
	case winapi.DRIVE_FIXED:
		return "fixed"
	case winapi.DRIVE_REMOVABLE:
		return "removable"
	case winapi.DRIVE_REMOTE:
		return "remote"
	case winapi.DRIVE_CDROM:
		return "cdrom"
	case winapi.DRIVE_RAMDISK:
		return "ramdisk"
	case winapi.DRIVE_NO_ROOT_DIR:
		return "no-root-dir"
	default:
		return "unknown"
	}
}

// IsSystemCriticalPath 报告路径是否属于「永不接受写操作」的系统区域。
//
// 与 docs/design/09-安全与风险控制.md §3 的禁止清单一致。
// M1 阶段只读，此函数用于在扫描时给出提示与为后续里程碑预留；
// 例外清单必须是显式枚举的具体路径，绝不允许通配。
func IsSystemCriticalPath(path string) bool {
	root, err := SystemRoot()
	if err != nil {
		return false
	}
	abs, err := CleanAbsolute(path)
	if err != nil {
		return false
	}

	forbidden := []string{
		root,
		`C:\Program Files`,
		`C:\Program Files (x86)`,
		`C:\Recovery`,
		`C:\$Recycle.Bin`,
		`C:\System Volume Information`,
	}
	// 显式枚举的例外（具体路径，无通配）
	allowed := []string{
		root + `\Temp`,
		root + `\Logs\CBS`,
		root + `\Logs\DISM`,
		root + `\Logs\WindowsUpdate`,
		root + `\SoftwareDistribution\Download`,
		root + `\LiveKernelReports`,
		root + `\Minidump`,
	}

	for _, a := range allowed {
		if IsUnder(abs, a) {
			return false
		}
	}
	for _, f := range forbidden {
		if IsUnder(abs, f) {
			return true
		}
	}
	return false
}

// IsRootPath 报告路径是否为卷根（"C:\"）。
func IsRootPath(p string) bool {
	trimmed := strings.TrimRight(p, `\`)
	return len(trimmed) == 2 && trimmed[1] == ':'
}
