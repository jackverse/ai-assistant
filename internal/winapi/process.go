package winapi

import (
	"fmt"
	"path/filepath"
	"syscall"
	"unsafe"
)

// 进程查询与终止（kernel32）。
//
// 只提供三件事：查进程的可执行文件路径（用户要看清「这个 PID 是什么」）、
// 判断进程是否还活着（外部启动的服务可能已被别人关掉）、按 PID 终止。
// 终止【整棵进程树】的编排放在业务包里——那是策略，不是 syscall 转发。

var (
	procOpenProcess                = kernel32.NewProc("OpenProcess")
	procQueryFullProcessImageNameW = kernel32.NewProc("QueryFullProcessImageNameW")
	procGetExitCodeProcess         = kernel32.NewProc("GetExitCodeProcess")
	procTerminateProcess           = kernel32.NewProc("TerminateProcess")
)

const (
	PROCESS_QUERY_LIMITED_INFORMATION = 0x1000
	PROCESS_TERMINATE                 = 0x0001
	PROCESS_QUERY_INFORMATION         = 0x0400

	STILL_ACTIVE = 259
)

// ProcessImagePath 返回进程的可执行文件完整路径。
//
// 权限说明：用 PROCESS_QUERY_LIMITED_INFORMATION 而不是 PROCESS_QUERY_INFORMATION，
// 前者对更高完整性级别（管理员）进程也能拿到路径，无需提权。
func ProcessImagePath(pid uint32) (string, error) {
	if pid == 0 {
		return "", fmt.Errorf("PID 0 是空闲进程，没有映像")
	}
	h, err := openProcess(PROCESS_QUERY_LIMITED_INFORMATION, pid)
	if err != nil {
		return "", err
	}
	defer procCloseHandle.Call(h)

	buf := make([]uint16, syscall.MAX_LONG_PATH)
	size := uint32(len(buf))
	r1, _, e := procQueryFullProcessImageNameW.Call(
		h, 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)))
	if r1 == 0 {
		return "", fmt.Errorf("读取进程 %d 的路径失败: %v", pid, e)
	}
	return syscall.UTF16ToString(buf[:size]), nil
}

// ProcessName 返回进程名（如 java.exe）。取不到路径时返回空串。
func ProcessName(pid uint32) string {
	p, err := ProcessImagePath(pid)
	if err != nil {
		return ""
	}
	return filepath.Base(p)
}

// ProcessAlive 判断进程是否仍在运行。
//
// 权限不足时（例如系统进程）保守地返回 true——把「查不到」当成「已退出」
// 会让界面误报服务已停止，那比多显示一个状态糟得多。
func ProcessAlive(pid uint32) bool {
	if pid == 0 {
		return false
	}
	h, err := openProcess(PROCESS_QUERY_LIMITED_INFORMATION, pid)
	if err != nil {
		return true
	}
	defer procCloseHandle.Call(h)

	var code uint32
	r1, _, _ := procGetExitCodeProcess.Call(h, uintptr(unsafe.Pointer(&code)))
	if r1 == 0 {
		return true
	}
	return code == STILL_ACTIVE
}

// TerminateProcessByPID 强制终止单个进程（不含子进程）。
//
// 需要对被终止进程有 PROCESS_TERMINATE 权限：终止别的用户或
// 更高完整性级别的进程会失败并返回「拒绝访问」，这是预期行为，
// 调用方应把错误原样呈现给用户，而不是尝试提权。
func TerminateProcessByPID(pid uint32, code uint32) error {
	if pid == 0 || pid == 4 {
		return fmt.Errorf("拒绝终止系统关键进程（PID %d）", pid)
	}
	h, err := openProcess(PROCESS_TERMINATE, pid)
	if err != nil {
		return err
	}
	defer procCloseHandle.Call(h)

	r1, _, e := procTerminateProcess.Call(h, uintptr(code))
	if r1 == 0 {
		return fmt.Errorf("终止进程 %d 失败: %v", pid, e)
	}
	return nil
}

func openProcess(access uint32, pid uint32) (uintptr, error) {
	h, _, e := procOpenProcess.Call(uintptr(access), 0, uintptr(pid))
	if h == 0 {
		if e == syscall.ERROR_ACCESS_DENIED {
			return 0, fmt.Errorf("无法访问进程 %d：拒绝访问（该进程以更高权限运行）", pid)
		}
		return 0, fmt.Errorf("无法打开进程 %d: %v", pid, e)
	}
	return h, nil
}
