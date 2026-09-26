//go:build windows

package ui

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// AlertError 弹一个模态错误框（MessageBoxW）。
//
// 为什么需要它：发布构建用 `-H windowsgui`（不分配控制台），
// 此时 `fmt.Fprintf(os.Stderr, ...)` 写出去的东西没有任何人看得到 ——
// 启动失败（显示器枚举失败、参数错误）会变成"双击后什么都没发生"。
// 关键失败路径统一走这里兜底，让用户至少知道发生了什么。
func AlertError(title, msg string) {
	t, err1 := syscall.UTF16PtrFromString(title)
	m, err2 := syscall.UTF16PtrFromString(msg)
	if err1 != nil || err2 != nil {
		return
	}
	const mbOK = 0x0
	const mbIconError = 0x10
	windows.NewLazySystemDLL("user32.dll").NewProc("MessageBoxW").Call(
		0, uintptr(unsafe.Pointer(m)), uintptr(unsafe.Pointer(t)),
		uintptr(mbOK|mbIconError))
}
