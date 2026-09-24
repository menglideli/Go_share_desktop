//go:build windows

package ui

import (
	"log"
	"syscall"
	"unsafe"
)

// moveWindow 用 Win32 SetWindowPos 把窗口挪到指定虚拟屏幕坐标。
//
// 用途：演示时把观看窗放到副屏，避免采集端把自己采进去（P0-1 自摄入）。
// 本机环境（Win10 LTSC 17763）没有 WDA_EXCLUDEFROMCAPTURE，只能靠物理隔离。
func moveWindow(title string, x, y int) bool {
	u := syscall.NewLazyDLL("user32.dll")
	fw := u.NewProc("FindWindowW")
	swp := u.NewProc("SetWindowPos")

	p, err := syscall.UTF16PtrFromString(title)
	if err != nil {
		log.Printf("moveWindow: 标题转换失败 %v", err)
		return false
	}
	hwnd, _, _ := fw.Call(0, uintptr(unsafe.Pointer(p)))
	if hwnd == 0 {
		log.Printf("moveWindow: 未找到窗口 %q", title)
		return false
	}
	// SWP_NOSIZE = 0x0001, SWP_NOZORDER = 0x0004, SWP_SHOWWINDOW = 0x0040
	const swpNoSize, swpNoZorder, swpShow = 0x0001, 0x0004, 0x0040
	r, _, _ := swp.Call(hwnd, 0, uintptr(x), uintptr(y), 0, 0,
		uintptr(swpNoSize|swpNoZorder|swpShow))
	ok := r != 0
	log.Printf("moveWindow: %q -> (%d,%d) ok=%v", title, x, y, ok)
	return ok
}
