package winapi

import (
	"syscall"
	"unsafe"
)

var (
	advapi32 = syscall.NewLazyDLL("advapi32.dll")

	procOpenProcessToken     = advapi32.NewProc("OpenProcessToken")
	procGetTokenInformation  = advapi32.NewProc("GetTokenInformation")
	procCloseHandle          = kernel32.NewProc("CloseHandle")
	procGetCurrentProcess    = kernel32.NewProc("GetCurrentProcess")
)

// IsElevated 报告当前进程是否以管理员权限运行（UAC 已提权）。
//
// 判定方式：打开自身进程令牌并查询 TokenElevation。
// 这比 "IsUserAnAdmin"（已弃用）可靠，且能正确处理 UAC 受限令牌的情形。
func IsElevated() bool {
	procHandle, _, _ := procGetCurrentProcess.Call()

	var token syscall.Handle
	r1, _, _ := procOpenProcessToken.Call(
		procHandle,
		uintptr(TOKEN_QUERY),
		uintptr(unsafe.Pointer(&token)),
	)
	if r1 == 0 {
		return false
	}
	defer procCloseHandle.Call(uintptr(token))

	var info TokenElevationInfo
	var returned uint32
	r1, _, _ = procGetTokenInformation.Call(
		uintptr(token),
		uintptr(TokenElevation),
		uintptr(unsafe.Pointer(&info)),
		uintptr(unsafe.Sizeof(info)),
		uintptr(unsafe.Pointer(&returned)),
	)
	if r1 == 0 {
		return false
	}
	return info.TokenIsElevated != 0
}
