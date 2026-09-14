package winapi

import (
	"syscall"
	"unsafe"
)

var (
	// 注意：GetFileInformationByHandle / GetFileInformationByHandleEx
	// 都没有 A/W 变体（它们不是字符串函数）。写成 ...W 会让 LazyProc
	// 在首次调用时直接 panic。
	procGetFileInformationByHandle   = kernel32.NewProc("GetFileInformationByHandle")
	procGetFileInformationByHandleEx = kernel32.NewProc("GetFileInformationByHandleEx")
)

// FileStandardInfoClass 是 FILE_INFO_BY_HANDLE_CLASS 中的 FileStandardInfo。
const FileStandardInfoClass = 1

// ByHandleFileInformation 对应 BY_HANDLE_FILE_INFORMATION。
type ByHandleFileInformation struct {
	FileAttributes     uint32
	CreationTime       Filetime
	LastAccessTime     Filetime
	LastWriteTime      Filetime
	VolumeSerialNumber uint32
	FileSizeHigh       uint32
	FileSizeLow        uint32
	NumberOfLinks      uint32
	FileIndexHigh      uint32
	FileIndexLow       uint32
}

// FileStandardInfo 对应 FILE_STANDARD_INFO。
// 末尾的填充是必要的：结构体按 8 字节对齐，实际大小是 24 字节。
type FileStandardInfo struct {
	AllocationSize int64
	EndOfFile      int64
	NumberOfLinks  uint32
	DeletePending  uint8
	Directory      uint8
	_              [2]uint8
}

// FileID 唯一标识一个文件（同一卷内）。
type FileID struct {
	VolumeSerial uint32
	Index        uint64
}

// FileMetadata 是一次句柄查询能拿到的全部有用信息。
type FileMetadata struct {
	ID         FileID
	Links      uint32
	Allocation int64 // 真实磁盘占用（含稀疏/压缩/驻留文件的影响）
	EndOfFile  int64 // 逻辑大小
}

// GetFileMetadata 通过一次 CreateFile 同时取得文件索引与真实磁盘占用。
//
// 为什么必须走句柄：
//   - 文件索引：WIN32_FIND_DATA 里紧跟文件大小的两个 DWORD 是【保留字段】，
//     实测恒为 0（见 doctor 的「硬链接去重能力」检查），不是文件索引。
//   - 真实占用：目录枚举给出的只是逻辑大小。簇对齐估算对小文件会高估，
//     对稀疏/压缩/云占位文件会严重高估。
//     AllocationSize 才是真实的分配字节数。
//
// 两个查询共用同一个句柄，因此只需一次 CreateFile 的开销。
// 只申请 FILE_READ_ATTRIBUTES 权限（不需要读数据），
// 因此对只读文件、他人文件通常也能成功。
func GetFileMetadata(path string) (FileMetadata, error) {
	const (
		fileReadAttributes      = 0x00000080
		openExisting            = 3
		fileFlagBackupSemantics = 0x02000000
	)

	pathp, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return FileMetadata{}, err
	}

	h, err := syscall.CreateFile(
		pathp,
		fileReadAttributes,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil,
		openExisting,
		fileFlagBackupSemantics,
		0,
	)
	if err != nil {
		return FileMetadata{}, &OpError{Op: "CreateFile(属性) " + path, Errno: errnoOf(err)}
	}
	defer procCloseHandle.Call(uintptr(h))

	if err := procGetFileInformationByHandle.Find(); err != nil {
		return FileMetadata{}, &OpError{Op: "GetFileInformationByHandle 不可用", Errno: syscall.Errno(0)}
	}

	var basic ByHandleFileInformation
	r1, _, callErr := procGetFileInformationByHandle.Call(
		uintptr(h),
		uintptr(unsafe.Pointer(&basic)),
	)
	if r1 == 0 {
		return FileMetadata{}, osError("GetFileInformationByHandle "+path, callErr)
	}

	md := FileMetadata{
		ID: FileID{
			VolumeSerial: basic.VolumeSerialNumber,
			Index:        uint64(basic.FileIndexHigh)<<32 | uint64(basic.FileIndexLow),
		},
		Links:     basic.NumberOfLinks,
		EndOfFile: int64(basic.FileSizeHigh)<<32 | int64(basic.FileSizeLow),
	}

	// AllocationSize 需要另一个查询类别。失败时不致命——退化为逻辑大小，
	// 由调用方决定是否回退到簇对齐估算。
	if err := procGetFileInformationByHandleEx.Find(); err == nil {
		var std FileStandardInfo
		r1, _, _ := procGetFileInformationByHandleEx.Call(
			uintptr(h),
			uintptr(FileStandardInfoClass),
			uintptr(unsafe.Pointer(&std)),
			uintptr(unsafe.Sizeof(std)),
		)
		if r1 != 0 {
			md.Allocation = std.AllocationSize
			if std.NumberOfLinks != 0 {
				md.Links = std.NumberOfLinks
			}
		} else {
			md.Allocation = -1
		}
	} else {
		md.Allocation = -1
	}

	return md, nil
}
