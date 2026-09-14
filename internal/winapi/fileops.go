package winapi

import (
	"errors"
	"syscall"
	"unsafe"
)

// 本文件是清理与迁移共用的文件级原语：占用检测、复制、删目录。

// errSharingViolation 是 ERROR_SHARING_VIOLATION（32）。
// 标准 syscall 包未定义该常量，这里按 Win32 文档补齐。
const errSharingViolation = syscall.Errno(32)

var (
	procCopyFileW        = kernel32.NewProc("CopyFileW")
	procRemoveDirectoryW = kernel32.NewProc("RemoveDirectoryW")
)

// IsFileInUse 检测文件是否被进程占用（共享冲突）。
//
// 原理：尝试以【零共享模式】打开文件；若得到 ERROR_SHARING_VIOLATION，
// 说明有进程正以与本调用不兼容的方式持有句柄。
// 注意局限（docs/design/06 §7.2）：以 FILE_SHARE_DELETE 打开的文件
// 这里测不出来，此时删除会成功但进程可能行为异常——
// 初期实现以共享冲突检测为界，够用且方向安全（宁可放过）。
func IsFileInUse(path string) (bool, error) {
	pathp, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return false, err
	}
	h, err := syscall.CreateFile(
		pathp,
		syscall.GENERIC_READ,
		0, // 不共享：能打开说明没人占用
		nil,
		syscall.OPEN_EXISTING,
		0,
		0,
	)
	if err == nil {
		syscall.CloseHandle(h)
		return false, nil
	}
	if errors.Is(err, errSharingViolation) {
		return true, nil
	}
	// 打不开但不是共享冲突（权限、不存在等）：不能断言「被占用」，
	// 交由上层按普通错误处理。
	return false, errnoOf(err)
}

// CopyFileW 复制单个文件。
//
// CopyFile 会把源文件的属性一并带到目标（含只读、时间戳语义由系统保证），
// 因此不需要额外的属性拷贝步骤。failIfExists 为 true 时目标已存在即失败，
// 迁移用它保证「绝不覆盖既有数据」。
func CopyFileW(src, dst string, failIfExists bool) error {
	srcp, err := syscall.UTF16PtrFromString(src)
	if err != nil {
		return err
	}
	dstp, err := syscall.UTF16PtrFromString(dst)
	if err != nil {
		return err
	}
	var fail uint32
	if failIfExists {
		fail = 1
	}
	r1, _, callErr := procCopyFileW.Call(
		uintptr(unsafe.Pointer(srcp)),
		uintptr(unsafe.Pointer(dstp)),
		uintptr(fail),
	)
	if r1 == 0 {
		return osError("CopyFile "+src+" -> "+dst, callErr)
	}
	return nil
}
