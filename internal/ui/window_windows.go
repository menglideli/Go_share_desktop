//go:build windows

package ui

import "log"

// moveWindow 用 Win32 SetWindowPos 把窗口挪到指定虚拟屏幕坐标（不改 Z 序、不改尺寸）。
//
// 用途：演示时把观看窗放到副屏，避免采集端把自己采进去（P0-1 自摄入）。
// 本机环境（Win10 LTSC 17763）没有 WDA_EXCLUDEFROMCAPTURE，只能靠物理隔离。
func moveWindow(title string, x, y int) bool {
	h := findWindow(title)
	if h == 0 {
		log.Printf("moveWindow: 未找到窗口 %q", title)
		return false
	}
	ok := moveOnly(h, x, y)
	log.Printf("moveWindow: %q -> (%d,%d) ok=%v", title, x, y, ok)
	return ok
}
