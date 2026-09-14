package winapi

import (
	"syscall"
	"unsafe"
)

var user32 = syscall.NewLazyDLL("user32.dll")

var procSendMessageTimeoutW = user32.NewProc("SendMessageTimeoutW")

const (
	hwndBroadcast    = 0xFFFF
	wmSettingChange  = 0x001A
	smtoAbortIfHung  = 0x0002
)

// BroadcastEnvironmentChange 广播 WM_SETTINGCHANGE，让 Explorer 等进程
// 重新读取环境变量（写完 HKCU\Environment 后必须调用）。
//
// 已运行的进程不会因此自动更新自己的环境块——对新进程才生效，
// 这一点必须在迁移结果里明确提示用户。
func BroadcastEnvironmentChange() {
	env, err := syscall.UTF16PtrFromString("Environment")
	if err != nil {
		return
	}
	procSendMessageTimeoutW.Call(
		uintptr(hwndBroadcast),
		uintptr(wmSettingChange),
		0,
		uintptr(unsafe.Pointer(env)),
		uintptr(smtoAbortIfHung),
		5000, // 每个窗口最多等 5 秒，不无限卡死
		0,
	)
}
