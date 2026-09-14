package winapi

import (
	"encoding/binary"
	"errors"
	"strings"
	"syscall"
)

// ReparseInfo 描述一个重解析点的类型与目标。
type ReparseInfo struct {
	Tag        uint32
	Substitute string // 内部目标（NT 命名空间，如 \??\D:\target）
	Print      string // 用户可见目标（如 D:\target）
}

// IsMountPoint 报告是否为目录联接（Junction）或卷挂载点。
func (r ReparseInfo) IsMountPoint() bool { return r.Tag == IO_REPARSE_TAG_MOUNT_POINT }

// IsSymlink 报告是否为符号链接。
func (r ReparseInfo) IsSymlink() bool { return r.Tag == IO_REPARSE_TAG_SYMLINK }

// IsCloud 报告是否为云占位（OneDrive 等）。
//
// 云占位文件在本地可能只有 0 字节，但逻辑大小很大；
// 若不区分，报告会严重虚报占用空间。
func (r ReparseInfo) IsCloud() bool {
	return r.Tag == IO_REPARSE_TAG_ONEDRIVE ||
		(r.Tag&IO_REPARSE_TAG_CLOUD_MASK) == IO_REPARSE_TAG_CLOUD_BASE_MASK
}

// TagName 返回可读的标记名，用于报告展示。
func (r ReparseInfo) TagName() string {
	switch {
	case r.IsMountPoint():
		return "junction"
	case r.IsSymlink():
		return "symlink"
	case r.IsCloud():
		return "cloud-placeholder"
	case r.Tag == IO_REPARSE_TAG_DEDUP:
		return "dedup"
	case r.Tag == IO_REPARSE_TAG_APPEXECLINK:
		return "appexeclink"
	case r.Tag == IO_REPARSE_TAG_WCI:
		return "wcifs"
	case r.Tag == IO_REPARSE_TAG_STORAGE_SYNC:
		return "sync-root"
	default:
		return "unknown"
	}
}

// Target 返回最适合展示的目标路径。
//
// 优先用 PrintName（用户可读形式）。若为空则退回 SubstituteName，
// 并去掉 NT 命名空间前缀（\??\ 或 \\?\），因为 SubstituteName 是给
// 内核看的内部形式。
func (r ReparseInfo) Target() string {
	if s := normalizeNTPrefix(r.Print); s != "" {
		return s
	}
	return normalizeNTPrefix(r.Substitute)
}

// normalizeNTPrefix 去掉 NT 命名空间前缀。
func normalizeNTPrefix(s string) string {
	for _, prefix := range []string{`\??\`, `\\?\UNC\`, `\\?\`} {
		if strings.HasPrefix(s, prefix) {
			return s[len(prefix):]
		}
	}
	return s
}

// ReadReparsePoint 读取指定路径的重解析点信息。
//
// 调用者应当先用文件属性判断 FILE_ATTRIBUTE_REPARSE_POINT，
// 避免对普通文件做无谓的 syscall。
//
// 实现要点：
//   - CreateFile 必须带 FILE_FLAG_OPEN_REPARSE_POINT，否则系统会跟随链接、
//     打开到目标上，读出的就是目标的属性而不是链接本身的。
//   - 目录还需要 FILE_FLAG_BACKUP_SEMANTICS。
func ReadReparsePoint(path string) (ReparseInfo, error) {
	const (
		openExisting          = 3
		fileFlagBackupSemantics = 0x02000000
		fileFlagOpenReparsePoint = 0x00200000
	)

	pathp, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return ReparseInfo{}, err
	}

	h, err := syscall.CreateFile(
		pathp,
		0, // 只需要读属性，不需要数据访问权限
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil,
		openExisting,
		fileFlagBackupSemantics|fileFlagOpenReparsePoint,
		0,
	)
	if err != nil {
		return ReparseInfo{}, &OpError{Op: "CreateFile(reparse) " + path, Errno: errnoOf(err)}
	}
	defer syscall.CloseHandle(h)

	// REPARSE_DATA_BUFFER 最大约 16 KB；用足够大的缓冲区以免截断长目标路径。
	buf := make([]byte, 16*1024)
	var returned uint32
	err = syscall.DeviceIoControl(
		h,
		FSCTL_GET_REPARSE_POINT,
		nil,
		0,
		&buf[0],
		uint32(len(buf)),
		&returned,
		nil,
	)
	if err != nil {
		return ReparseInfo{}, &OpError{Op: "DeviceIoControl(FSCTL_GET_REPARSE_POINT) " + path, Errno: errnoOf(err)}
	}
	if returned < 16 {
		return ReparseInfo{}, &OpError{Op: "reparse 缓冲区过短 " + path, Errno: syscall.Errno(0)}
	}

	info := ReparseInfo{
		Tag: binary.LittleEndian.Uint32(buf[0:4]),
	}

	// 只有 mount point / symlink 两种布局带 PathBuffer，但两者的偏移【不同】：
	//
	//   MOUNT_POINT (Junction):                          PathBuffer @ 16
	//     tag(4) dataLen(2) reserved(2) subOff(2) subLen(2) printOff(2) printLen(2)
	//
	//   SYMLINK:                                         PathBuffer @ 20
	//     tag(4) dataLen(2) reserved(2) subOff(2) subLen(2) printOff(2) printLen(2) flags(4)
	//
	// 符号链接多一个 DWORD Flags（表示相对/绝对），少算这 4 字节会读出错位的
	// 目标路径。实测：C:\Users\All Users 读成 "ta\??\C:\ProgramDa"（多出前缀
	// ta、尾部缺 ta），正是偏移少 4 字节造成的。
	//
	// 其余标记（云占位、去重等）没有可解析的目标路径。
	if info.IsMountPoint() || info.IsSymlink() {
		dataLen := binary.LittleEndian.Uint16(buf[4:6])
		if int(dataLen)+8 <= len(buf) {
			pathBase := 16
			if info.IsSymlink() {
				pathBase = 20
			}

			subOff := binary.LittleEndian.Uint16(buf[8:10])
			subLen := binary.LittleEndian.Uint16(buf[10:12])
			printOff := binary.LittleEndian.Uint16(buf[12:14])
			printLen := binary.LittleEndian.Uint16(buf[14:16])

			info.Substitute = utf16BytesToString(buf, pathBase+int(subOff), int(subLen))
			info.Print = utf16BytesToString(buf, pathBase+int(printOff), int(printLen))
		}
	}

	return info, nil
}

// utf16BytesToString 从字节切片中按 UTF-16LE 解码指定范围（长度为字节数）。
func utf16BytesToString(buf []byte, off, lengthBytes int) string {
	if lengthBytes <= 0 || off < 0 || off+lengthBytes > len(buf) {
		return ""
	}
	u16 := make([]uint16, 0, lengthBytes/2)
	for i := off; i+1 < off+lengthBytes; i += 2 {
		u16 = append(u16, binary.LittleEndian.Uint16(buf[i:i+2]))
	}
	// 去掉末尾的 NUL
	for len(u16) > 0 && u16[len(u16)-1] == 0 {
		u16 = u16[:len(u16)-1]
	}
	return syscall.UTF16ToString(u16)
}

// errnoOf 尽力把 error 转成 syscall.Errno。
//
// 用 errors.As 而非类型断言：错误可能被多层包装（例如 os 的 *PathError
// 在 Windows 上并不存在，但 os 函数仍可能包装出其他类型），
// errors.As 会沿 Unwrap 链查找，更稳妥。
func errnoOf(err error) syscall.Errno {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno
	}
	return syscall.Errno(0)
}
