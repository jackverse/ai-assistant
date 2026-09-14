package winapi

import (
	"fmt"
	"syscall"
	"unsafe"
)

var (
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procGetLogicalDrives         = kernel32.NewProc("GetLogicalDrives")
	procGetDriveTypeW            = kernel32.NewProc("GetDriveTypeW")
	procGetVolumeInformationW    = kernel32.NewProc("GetVolumeInformationW")
	procGetDiskFreeSpaceExW      = kernel32.NewProc("GetDiskFreeSpaceExW")
	procGetDiskFreeSpaceW        = kernel32.NewProc("GetDiskFreeSpaceW")
	procGetWindowsDirectoryW     = kernel32.NewProc("GetWindowsDirectoryW")
	procGetSystemDirectoryW      = kernel32.NewProc("GetSystemDirectoryW")
	procFindFirstFileExW         = kernel32.NewProc("FindFirstFileExW")
	procFindNextFileW            = kernel32.NewProc("FindNextFileW")
	procFindClose                = kernel32.NewProc("FindClose")
	procGetCompressedFileSizeW   = kernel32.NewProc("GetCompressedFileSizeW")
	procGetFileAttributesW       = kernel32.NewProc("GetFileAttributesW")
	procSetConsoleOutputCP       = kernel32.NewProc("SetConsoleOutputCP")
	procSetConsoleCP             = kernel32.NewProc("SetConsoleCP")
)

// SetConsoleUTF8 把控制台的输入/输出代码页切换为 UTF-8（65001）。
//
// 为什么必须做：Windows 中文环境下 cmd.exe 默认代码页是 936（GBK）。
// 直接输出 UTF-8 的中文路径（如 C:\Users\x\OneDrive\图片）会变成乱码。
// 实测已复现：不加这句时「图片」显示为「ͼƬ」。
// 对 Git Bash / Windows Terminal 等本就使用 UTF-8 的环境是幂等操作。
func SetConsoleUTF8() {
	procSetConsoleOutputCP.Call(65001)
	procSetConsoleCP.Call(65001)
}

// GetLogicalDrives 返回盘符位掩码（bit 0 = A:, bit 1 = B:, ...）。
func GetLogicalDrives() (uint32, error) {
	r1, _, err := procGetLogicalDrives.Call()
	if r1 == 0 {
		return 0, osError("GetLogicalDrives", err)
	}
	return uint32(r1), nil
}

// GetDriveType 返回驱动器类型（DRIVE_FIXED 等）。
func GetDriveType(root string) uint32 {
	p, err := syscall.UTF16PtrFromString(root)
	if err != nil {
		return DRIVE_UNKNOWN
	}
	r1, _, _ := procGetDriveTypeW.Call(uintptr(unsafe.Pointer(p)))
	return uint32(r1)
}

// VolumeInfo 是 GetVolumeInformationW 的解析结果。
type VolumeInfo struct {
	Label              string
	FileSystem         string
	SerialNumber       uint32
	MaxComponentLength uint32
	FileSystemFlags    uint32
}

// GetVolumeInformation 读取卷的标签、文件系统类型与序列号。
//
// 文件系统类型是迁移预检的必需项：FAT32/exFAT 不支持重解析点，
// 且 FAT32 单文件上限 4 GB（放不下 36 GB 的 docker_data.vhdx）。
func GetVolumeInformation(root string) (VolumeInfo, error) {
	rootp, err := syscall.UTF16PtrFromString(root)
	if err != nil {
		return VolumeInfo{}, err
	}

	var (
		labelBuf  [MaxPath + 1]uint16
		fsBuf     [MaxPath + 1]uint16
		serial    uint32
		maxComp   uint32
		fsFlags   uint32
	)

	r1, _, callErr := procGetVolumeInformationW.Call(
		uintptr(unsafe.Pointer(rootp)),
		uintptr(unsafe.Pointer(&labelBuf[0])),
		uintptr(len(labelBuf)),
		uintptr(unsafe.Pointer(&serial)),
		uintptr(unsafe.Pointer(&maxComp)),
		uintptr(unsafe.Pointer(&fsFlags)),
		uintptr(unsafe.Pointer(&fsBuf[0])),
		uintptr(len(fsBuf)),
	)
	if r1 == 0 {
		return VolumeInfo{}, osError("GetVolumeInformation", callErr)
	}

	return VolumeInfo{
		Label:              syscall.UTF16ToString(labelBuf[:]),
		FileSystem:         syscall.UTF16ToString(fsBuf[:]),
		SerialNumber:       serial,
		MaxComponentLength: maxComp,
		FileSystemFlags:    fsFlags,
	}, nil
}

// DiskSpace 是卷的容量信息。
type DiskSpace struct {
	FreeToCaller uint64
	Total        uint64
	TotalFree    uint64
}

// GetDiskFreeSpace 读取卷容量。
//
// 使用 64 位的 Ex 版本，避免 GetDiskFreeSpace 在大于 2 TB 的卷上溢出。
func GetDiskFreeSpace(root string) (DiskSpace, error) {
	rootp, err := syscall.UTF16PtrFromString(root)
	if err != nil {
		return DiskSpace{}, err
	}
	var freeToCaller, total, totalFree uint64
	r1, _, callErr := procGetDiskFreeSpaceExW.Call(
		uintptr(unsafe.Pointer(rootp)),
		uintptr(unsafe.Pointer(&freeToCaller)),
		uintptr(unsafe.Pointer(&total)),
		uintptr(unsafe.Pointer(&totalFree)),
	)
	if r1 == 0 {
		return DiskSpace{}, osError("GetDiskFreeSpaceEx", callErr)
	}
	return DiskSpace{FreeToCaller: freeToCaller, Total: total, TotalFree: totalFree}, nil
}

// ClusterSize 返回卷的簇大小（字节）。
//
// 用于把文件逻辑大小折算成实际占用（簇对齐）。
// 注意：不要用 GetDiskFreeSpace 返回的空闲/总簇数——它在超过 2 TB 的卷上不可靠；
// 这里只用它来拿 sectorsPerCluster * bytesPerSector。
func ClusterSize(root string) (uint32, error) {
	rootp, err := syscall.UTF16PtrFromString(root)
	if err != nil {
		return 0, err
	}
	var sectorsPerCluster, bytesPerSector, freeClusters, totalClusters uint32
	r1, _, callErr := procGetDiskFreeSpaceW.Call(
		uintptr(unsafe.Pointer(rootp)),
		uintptr(unsafe.Pointer(&sectorsPerCluster)),
		uintptr(unsafe.Pointer(&bytesPerSector)),
		uintptr(unsafe.Pointer(&freeClusters)),
		uintptr(unsafe.Pointer(&totalClusters)),
	)
	if r1 == 0 {
		return 0, osError("GetDiskFreeSpace", callErr)
	}
	return sectorsPerCluster * bytesPerSector, nil
}

// GetWindowsDirectory 返回 Windows 安装目录（如 C:\Windows）。
func GetWindowsDirectory() (string, error) {
	return getDirectoryProc(procGetWindowsDirectoryW, "GetWindowsDirectory")
}

// GetSystemDirectory 返回系统目录（如 C:\Windows\System32）。
func GetSystemDirectory() (string, error) {
	return getDirectoryProc(procGetSystemDirectoryW, "GetSystemDirectory")
}

func getDirectoryProc(proc *syscall.LazyProc, name string) (string, error) {
	buf := make([]uint16, MaxPath+1)
	r1, _, callErr := proc.Call(
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
	)
	if r1 == 0 {
		return "", osError(name, callErr)
	}
	return syscall.UTF16ToString(buf[:]), nil
}

// FindHandle 是目录枚举句柄。零值表示无效。
type FindHandle uintptr

// FindFirstFileEx 打开目录枚举。
//
// pattern 必须是绝对路径且以 \* 结尾，并且【必须】已经加上 \\?\ 前缀
// （见 sys.ToExtendedPath），否则超过 260 字符的路径会失败。
//
// 关于 FindExInfoStandard 而非 FindExInfoBasic（重要）：
// FindExInfoBasic 不查询 8.3 短名，性能更好，但实测（见 doctor 命令的
// 「文件索引」检查）它【不填充 FileIndex】。而 FileIndex 是硬链接去重的
// 唯一依据——WinSxS 等目录大量使用硬链接，缺了它体积会被严重高估。
// 因此这里用 FindExInfoStandard 换取正确性；性能损失通过
// FIND_FIRST_EX_LARGE_FETCH 部分弥补。
func FindFirstFileEx(pattern string) (FindHandle, *Win32FindData, error) {
	patternp, err := syscall.UTF16PtrFromString(pattern)
	if err != nil {
		return 0, nil, err
	}
	data := &Win32FindData{}
	r1, _, callErr := procFindFirstFileExW.Call(
		uintptr(unsafe.Pointer(patternp)),
		uintptr(FindExInfoStandard),
		uintptr(unsafe.Pointer(data)),
		uintptr(FindExSearchNameMatch),
		0,
		uintptr(FIND_FIRST_EX_LARGE_FETCH),
	)
	if r1 == invalidHandleValue {
		return 0, nil, osError("FindFirstFileEx "+pattern, callErr)
	}
	return FindHandle(r1), data, nil
}

// FindNextFile 取下一个目录项。枚举结束时返回 io.EOF 语义的错误。
func FindNextFile(h FindHandle, data *Win32FindData) error {
	r1, _, callErr := procFindNextFileW.Call(
		uintptr(h),
		uintptr(unsafe.Pointer(data)),
	)
	if r1 == 0 {
		return osError("FindNextFile", callErr)
	}
	return nil
}

// FindClose 关闭目录枚举句柄。
func FindClose(h FindHandle) {
	if h != 0 {
		procFindClose.Call(uintptr(h))
	}
}

// GetCompressedFileSize 返回文件在磁盘上的实际占用字节数。
//
// 与逻辑大小不同，它会计入稀疏、压缩与去重，因此能正确反映
// 动态扩展的 VHDX 内部空洞、以及 OneDrive 云占位文件（本地占用可能为 0）。
// 代价是一次额外 syscall，所以只在 --deep 模式或大文件上调用。
func GetCompressedFileSize(path string) (uint64, error) {
	pathp, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var high uint32
	r1, _, callErr := procGetCompressedFileSizeW.Call(
		uintptr(unsafe.Pointer(pathp)),
		uintptr(unsafe.Pointer(&high)),
	)
	low := uint32(r1)
	if low == 0xFFFFFFFF {
		// 需要通过 GetLastError 区分「真错误」与「文件确实有 0xFFFFFFFF 低 32 位」。
		if callErr != nil && callErr != syscall.Errno(0) {
			if errno, ok := callErr.(syscall.Errno); ok && errno != 0 {
				return 0, osError("GetCompressedFileSize "+path, callErr)
			}
		}
	}
	return uint64(high)<<32 | uint64(low), nil
}

// GetFileAttributes 返回路径属性。若路径不存在返回错误。
func GetFileAttributes(path string) (uint32, error) {
	pathp, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	r1, _, callErr := procGetFileAttributesW.Call(uintptr(unsafe.Pointer(pathp)))
	if uint32(r1) == 0xFFFFFFFF {
		return 0, osError("GetFileAttributes "+path, callErr)
	}
	return uint32(r1), nil
}

// osError 把 syscall 返回的错误转成带操作名的错误。
// LazyProc.Call 在成功时也会返回一个非 nil 的 "Errno 0"，因此需要过滤。
func osError(op string, err error) error {
	if err == nil {
		return fmt.Errorf("%s: 未知错误", op)
	}
	if errno, ok := err.(syscall.Errno); ok {
		if errno == 0 {
			return fmt.Errorf("%s: 未知错误", op)
		}
		return &OpError{Op: op, Errno: errno}
	}
	return fmt.Errorf("%s: %w", op, err)
}

// OpError 携带 Win32 错误码与操作名，便于上层区分「权限不足」等情形。
type OpError struct {
	Op    string
	Errno syscall.Errno
}

func (e *OpError) Error() string {
	return fmt.Sprintf("%s: %v (Win32 错误码 %d)", e.Op, e.Errno, uintptr(e.Errno))
}

// Unwrap 支持 errors.Is(err, syscall.ERROR_ACCESS_DENIED) 之类的判断。
func (e *OpError) Unwrap() error { return e.Errno }
