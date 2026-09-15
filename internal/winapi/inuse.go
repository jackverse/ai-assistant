package winapi

import (
	"syscall"
	"unsafe"
)

// ProbeFileInUse 探测文件当前是否被其它进程占用。
//
// 原理：尝试以「独占」方式打开（不声明任何共享位）。只要有别的句柄
// 以任何方式打开着这个文件，就会得到共享冲突错误。
//
// 返回值语义（三态，调用方必须区分）：
//   - inUse=true   确认被占用
//   - inUse=false  确认空闲（成功以独占方式打开并立即关闭）
//   - unknown=true 无法判定（ACL 拒绝、路径消失等）——不能当成"空闲"用，
//     也不能当成"占用"，只能标注为未知
//
// 访问掩码用读写而共享位给 0：只声明读权限时，别的进程以写方式打开着
// 文件也会让我们打开成功，会误判成"空闲"。
func ProbeFileInUse(path string) (inUse, unknown bool) {
	const (
		genericRead  = 0x80000000
		genericWrite = 0x40000000
		openExisting = 3
		// 仅元数据不需要，这里要真正测共享冲突，需要数据访问位
		fileReadData  = 0x00000001
		fileWriteData = 0x00000002
	)

	pathp, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return false, true
	}

	h, err := syscall.CreateFile(
		pathp,
		fileReadData|fileWriteData|genericRead|genericWrite,
		0, // 不声明任何共享位
		nil,
		openExisting,
		0,
		0,
	)
	if err != nil {
		errno := errnoOf(err)
		switch {
		case errno == 32: // ERROR_SHARING_VIOLATION：被其它句柄占用
			return true, false
		case errno == 5: // ERROR_ACCESS_DENIED：无法区分 ACL 与独占占用
			return false, true
		case errno == 2 || errno == 3: // 文件/路径不存在
			return false, true // 按未知处理，不要当空闲
		default:
			return false, true
		}
	}
	procCloseHandle.Call(uintptr(h))
	return false, false
}

// 确保引用（CreateFile 参数里未直接用到 unsafe）。
var _ = unsafe.Pointer(nil)
