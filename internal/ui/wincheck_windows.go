//go:build windows

package ui

import (
	"log"
	"syscall"
	"unsafe"
)

// LogWindowVisible 用 Win32 FindWindowW / IsWindowVisible 断言窗口真实存在。
//
// 「渲染了帧」不等于「窗口真的显示出来了」——阶段 0 就差点在这里误判：
// 第一帧时 IsWindowVisible 返回 0，实际是 Gio 尚未完成 ShowWindow，
// 持续渲染到第 10 帧再断言才是 1。
func LogWindowVisible(title string) {
	u := syscall.NewLazyDLL("user32.dll")
	fw := u.NewProc("FindWindowW")
	iv := u.NewProc("IsWindowVisible")

	p, err := syscall.UTF16PtrFromString(title)
	if err != nil {
		log.Printf("wincheck: UTF16 转换失败 %v", err)
		return
	}
	hwnd, _, _ := fw.Call(0, uintptr(unsafe.Pointer(p)))
	if hwnd == 0 {
		log.Printf("wincheck: 未找到窗口 %q", title)
		return
	}
	v, _, _ := iv.Call(hwnd)
	log.Printf("wincheck: 窗口 %q hwnd=0x%x visible=%d", title, hwnd, v)
}
