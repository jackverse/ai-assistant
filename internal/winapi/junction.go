package winapi

import (
	"encoding/binary"
	"errors"
	"fmt"
	"syscall"
	"unsafe"
)

// FSCTL_SET_REPARSE_POINT 用于在目录上写入重解析数据（建 Junction）。
const FSCTL_SET_REPARSE_POINT = 0x000900A4

// SetJunction 在 dir 上创建指向 target 的目录联接（Junction）。
//
// 为什么必须走 DeviceIoControl 而不是 os.Symlink（见 docs/design/07 §2.2）：
//   - Go 标准库在 Windows 上创建的是符号链接，需要管理员权限或开发者模式；
//   - Junction 本地卷内创建【不需要提权】，这是迁移工具可用性的关键。
//
// 调用前提（由调用方保证，本函数会再校验一次）：
//   - dir 已存在且为空（Junction 的挂载点必须存在且为空）；
//   - target 是本机绝对目录路径（Junction 不支持 UNC）。
//
// 布局细节（错一字节就会建出不可用的链接）：
//   - SubstituteName 必须是 NT 命名空间形式 \??\D:\...（内核解析用）；
//   - PrintName 用 Win32 形式（给用户看）；
//   - 所有字符串 UTF-16LE，长度以【字节】计且不含结尾 NUL；
//     PathBuffer 里则各带一个 NUL；
//   - ReparseDataLength 不含最前面的 8 字节头（tag+长度+保留）。
func SetJunction(dir, target string) error {
	if dir == "" || target == "" {
		return errors.New("SetJunction: 目录或目标为空")
	}

	// 挂载点必须存在且为空；顺带确认它不是已有的重解析点，
	// 避免把别人创建的链接覆盖掉。
	attrs, err := GetFileAttributes(dir)
	if err != nil {
		return fmt.Errorf("SetJunction: 挂载点不存在 %s: %w", dir, err)
	}
	if attrs&FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("SetJunction: %s 已是重解析点，拒绝覆盖", dir)
	}
	empty, err := dirIsEmpty(dir)
	if err != nil {
		return fmt.Errorf("SetJunction: 无法确认挂载点为空 %s: %w", dir, err)
	}
	if !empty {
		return fmt.Errorf("SetJunction: 挂载点 %s 非空，拒绝在其上创建 Junction", dir)
	}

	sub, err := syscall.UTF16FromString(`\??\` + target)
	if err != nil {
		return fmt.Errorf("SetJunction: 目标路径含 NUL: %w", err)
	}
	print, err := syscall.UTF16FromString(target)
	if err != nil {
		return fmt.Errorf("SetJunction: 目标路径含 NUL: %w", err)
	}

	subBytes := (len(sub) - 1) * 2     // 不含 NUL
	printBytes := (len(print) - 1) * 2 // 不含 NUL
	dataLen := 8 + len(sub)*2 + len(print)*2

	// 完整输入缓冲：头 8 字节（tag+dataLen+reserved）+ 挂载点缓冲。
	buf := make([]byte, 8+dataLen)
	binary.LittleEndian.PutUint32(buf[0:4], IO_REPARSE_TAG_MOUNT_POINT)
	binary.LittleEndian.PutUint16(buf[4:6], uint16(dataLen))
	// buf[6:8] Reserved = 0
	// 挂载点缓冲的偏移/长度均相对其自身起始（buf[8:]）
	binary.LittleEndian.PutUint16(buf[8:10], 0)                                // SubstituteNameOffset
	binary.LittleEndian.PutUint16(buf[10:12], uint16(subBytes))                // SubstituteNameLength
	binary.LittleEndian.PutUint16(buf[12:14], uint16(len(sub)*2))              // PrintNameOffset（sub 含 NUL 之后）
	binary.LittleEndian.PutUint16(buf[14:16], uint16(printBytes))              // PrintNameLength
	copy(buf[16:], utf16LE(sub))
	copy(buf[16+len(sub)*2:], utf16LE(print))

	dirp, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return fmt.Errorf("SetJunction: %w", err)
	}
	h, err := syscall.CreateFile(
		dirp,
		syscall.GENERIC_WRITE,
		0, // 独占：写入重解析数据期间不允许共享
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_FLAG_BACKUP_SEMANTICS|syscall.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return fmt.Errorf("SetJunction: 打开 %s 失败（挂载点需已存在）: %w", dir, err)
	}
	defer syscall.CloseHandle(h)

	var returned uint32
	err = syscall.DeviceIoControl(
		h,
		FSCTL_SET_REPARSE_POINT,
		&buf[0],
		uint32(len(buf)),
		nil,
		0,
		&returned,
		nil,
	)
	if err != nil {
		return fmt.Errorf("SetJunction: 写入重解析数据失败 %s -> %s: %w", dir, target, errnoOf(err))
	}
	return nil
}

// utf16LE 把 UTF-16 字符串编码为小端字节流。
func utf16LE(s []uint16) []byte {
	out := make([]byte, len(s)*2)
	for i, v := range s {
		binary.LittleEndian.PutUint16(out[i*2:], v)
	}
	return out
}

// dirIsEmpty 报告目录是否为空（不含 . 与 ..）。
func dirIsEmpty(dir string) (bool, error) {
	pattern := dir
	if pattern[len(pattern)-1] != '\\' {
		pattern += `\`
	}
	pattern += "*"
	h, data, err := FindFirstFileEx(pattern)
	if err != nil {
		// 空目录上 FindFirstFileEx 返回 ERROR_FILE_NOT_FOUND，即「空」。
		if errors.Is(err, syscall.ERROR_FILE_NOT_FOUND) {
			return true, nil
		}
		return false, err
	}
	defer FindClose(h)
	for {
		name := syscall.UTF16ToString(data.FileName[:])
		if name != "." && name != ".." {
			return false, nil
		}
		if err := FindNextFile(h, data); err != nil {
			return true, nil
		}
	}
}

// IsReparsePointRaw 报告路径是否为重解析点，并返回其标记。
//
// 与 ReadReparsePoint 的区别：不打开句柄，仅读属性 + 必要时读标记，
// 用于删除前的最后确认（docs/design/09 §4.2 的 TOCTOU 重校验）。
func IsReparsePointRaw(path string) (bool, uint32, error) {
	attrs, err := GetFileAttributes(path)
	if err != nil {
		return false, 0, err
	}
	if attrs&FILE_ATTRIBUTE_REPARSE_POINT == 0 {
		return false, 0, nil
	}
	info, err := ReadReparsePoint(path)
	if err != nil {
		// 属性说是重解析点但读不出标记：按「是重解析点」处理（保守方向）。
		return true, 0, nil
	}
	return true, info.Tag, nil
}

// RemoveDirectoryOnly 删除一个目录，但【只删目录本身】。
//
// 这是删除 Junction 的唯一安全原语：与 os.RemoveAll 不同，
// RemoveDirectoryW 遇到重解析点时只摘除链接、绝不递归进目标
// （docs/design/07 §4.4 把误删目标列为整个工具最危险的单个操作）。
//
// 防护：删除前重新读取重解析标记。若该路径【不是】重解析点，
// 说明它可能是真实目录，直接拒绝——宁可漏删，不可误删。
func RemoveDirectoryOnly(dir string) error {
	isReparse, tag, err := IsReparsePointRaw(dir)
	if err != nil {
		return fmt.Errorf("RemoveDirectoryOnly: 无法读取属性 %s: %w", dir, err)
	}
	if !isReparse {
		return fmt.Errorf("拒绝删除：%s 不是重解析点，可能是真实目录", dir)
	}
	_ = tag

	dirp, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	r1, _, callErr := procRemoveDirectoryW.Call(uintptr(unsafe.Pointer(dirp)))
	if r1 == 0 {
		return osError("RemoveDirectory "+dir, callErr)
	}
	return nil
}
