// Package winapi 是对 Windows API 的薄封装。
//
// 设计约束（见 docs/design/03-扫描引擎.md 与 09-安全与风险控制.md）：
//   - 只使用 W 系列（UTF-16）API，绝不使用 A 系列（ANSI），
//     否则中文路径会被代码页破坏（实测到 GBK 控制台下「图片」显示为乱码）。
//   - 本包不含任何业务判断，只做 syscall 转发与结构体解析，便于单独测试。
//   - 不依赖 golang.org/x/sys，仅用标准库 syscall，保证离线可构建。
package winapi

// 文件属性
const (
	FILE_ATTRIBUTE_READONLY      = 0x00000001
	FILE_ATTRIBUTE_HIDDEN        = 0x00000002
	FILE_ATTRIBUTE_SYSTEM        = 0x00000004
	FILE_ATTRIBUTE_DIRECTORY     = 0x00000010
	FILE_ATTRIBUTE_ARCHIVE       = 0x00000020
	FILE_ATTRIBUTE_SPARSE_FILE   = 0x00000200
	FILE_ATTRIBUTE_REPARSE_POINT = 0x00000400
	FILE_ATTRIBUTE_COMPRESSED    = 0x00000800
)

// 驱动器类型（GetDriveTypeW 返回值）
const (
	DRIVE_UNKNOWN     = 0
	DRIVE_NO_ROOT_DIR = 1
	DRIVE_REMOVABLE   = 2
	DRIVE_FIXED       = 3
	DRIVE_REMOTE      = 4
	DRIVE_CDROM       = 5
	DRIVE_RAMDISK     = 6
)

// FindFirstFileExW 参数
const (
	FindExInfoStandard = 0
	// FindExInfoBasic 不查询 8.3 短名，显著提升大目录枚举性能。
	// 注意：它仍然返回 FileIndex（硬链接去重依赖此字段）。
	FindExInfoBasic        = 1
	FindExSearchNameMatch  = 0
	FIND_FIRST_EX_LARGE_FETCH = 0x00000002
)

// 重解析点标记（reparse tag）
//
// 遍历时必须识别且【不跟随】这些点，否则会出现两类事故：
//  1. Junction 指向 C:\Windows 时重复统计整棵子树；
//  2. 后续基于路径的删除操作被引导到预期之外的位置。
const (
	IO_REPARSE_TAG_MOUNT_POINT     = 0xA0000003 // 目录联接（Junction）/ 卷挂载点
	IO_REPARSE_TAG_SYMLINK         = 0xA000000C // 符号链接
	IO_REPARSE_TAG_DEDUP           = 0x80000013 // 数据去重
	IO_REPARSE_TAG_WCI             = 0x80000018 // Windows 容器层
	IO_REPARSE_TAG_CLOUD           = 0x9000001A // OneDrive 等云占位（基础值）
	IO_REPARSE_TAG_APPEXECLINK     = 0x8000001B // UWP/Store 应用执行链
	IO_REPARSE_TAG_STORAGE_SYNC    = 0x8000001E // 同步根
	IO_REPARSE_TAG_ONEDRIVE        = 0x80000021 // OneDrive 占位
	IO_REPARSE_TAG_CLOUD_MASK      = 0xFFFF0000 // 云标记的高位掩码（同族标记按此判定）
	IO_REPARSE_TAG_CLOUD_BASE_MASK = 0x90000000
)

// FSCTL_GET_REPARSE_POINT 用于读取重解析点数据。
const FSCTL_GET_REPARSE_POINT = 0x000900A8

// 访问掩码
const (
	GENERIC_READ  = 0x80000000
	FILE_READ_DATA = 0x00000001
	TOKEN_QUERY   = 0x00000008
)

// TokenElevation 是 GetTokenInformation 的信息类别。
const TokenElevation = 20

// INVALID_HANDLE_VALUE 在 uintptr 上的表示。
const invalidHandleValue = ^uintptr(0)

// MaxPath 是 Win32 传统路径长度上限。超过时需使用 \\?\ 前缀。
const MaxPath = 260

// Filetime 是 Windows 的 64 位时间戳（自 1601-01-01 起的 100 纳秒数）。
type Filetime struct {
	LowDateTime  uint32
	HighDateTime uint32
}

// Nanoseconds 返回 100 纳秒单位的总数。
func (ft Filetime) Nanoseconds() int64 {
	return int64(ft.HighDateTime)<<32 | int64(ft.LowDateTime)
}

// unixEpochOffset 是 1601-01-01 到 1970-01-01 之间的 100 纳秒数。
const unixEpochOffset = 116444736000000000

// Win32FindData 对应 WIN32_FIND_DATAW（592 字节）。
//
// 字段顺序与大小必须与 Win32 定义严格一致，否则 size/index 字段会串位。
// 注意：dwReserved0/dwReserved1 在 Win32 头文件中就是文件索引的高低 32 位，
// 这里直接命名为 FileIndexHigh/FileIndexLow 以便使用。
type Win32FindData struct {
	FileAttributes    uint32
	CreationTime      Filetime
	LastAccessTime    Filetime
	LastWriteTime     Filetime
	FileSizeHigh      uint32
	FileSizeLow       uint32
	FileIndexHigh     uint32
	FileIndexLow      uint32
	FileName          [MaxPath]uint16
	AlternateFileName [14]uint16
}

// Size 返回文件逻辑大小（字节）。
func (d *Win32FindData) Size() int64 {
	return int64(d.FileSizeHigh)<<32 | int64(d.FileSizeLow)
}

// FileIndex 返回 dwReserved0/dwReserved1 组成的值。
//
// ⚠️ 不要用它做硬链接去重。这两个 DWORD 在 Win32 定义里是【保留字段】，
// 实测无论用 FindExInfoStandard 还是 FindExInfoBasic 都恒为 0
// （可用 `winclean doctor` 复核）。可靠的文件索引只能通过
// GetFileInformationByHandle 获取，见 winapi.GetFileID。
func (d *Win32FindData) FileIndex() uint64 {
	return uint64(d.FileIndexHigh)<<32 | uint64(d.FileIndexLow)
}

// IsDir 报告该项是否为目录。
func (d *Win32FindData) IsDir() bool {
	return d.FileAttributes&FILE_ATTRIBUTE_DIRECTORY != 0
}

// IsReparsePoint 报告该项是否为重解析点。
func (d *Win32FindData) IsReparsePoint() bool {
	return d.FileAttributes&FILE_ATTRIBUTE_REPARSE_POINT != 0
}

// IsSparseOrCompressed 报告文件是否可能是稀疏/压缩的（实际占用可能小于逻辑大小）。
func (d *Win32FindData) IsSparseOrCompressed() bool {
	return d.FileAttributes&(FILE_ATTRIBUTE_SPARSE_FILE|FILE_ATTRIBUTE_COMPRESSED) != 0
}

// TokenElevationInfo 对应 TOKEN_ELEVATION。
type TokenElevationInfo struct {
	TokenIsElevated uint32
}
