//go:build windows

// 本文件集中存放 Win32 的窗口查询 / 摆放封装。
//
// 为什么集中：这类调用原先散在 4 个文件里各自 NewLazyDLL，而 LazyProc 挂错 DLL
// 是 mustFind **panic**（崩在 goroutine 里，build/vet 全绿也发现不了）。
// 一处声明、一处可核对。所有调用都只碰 Z 序/位置，不碰 Gio 的窗口过程。
package ui

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32API = windows.NewLazySystemDLL("user32.dll")

	procFindWindowW              = user32API.NewProc("FindWindowW")
	procSetWindowPos             = user32API.NewProc("SetWindowPos")
	procGetWindowRect            = user32API.NewProc("GetWindowRect")
	procGetWindowTextW           = user32API.NewProc("GetWindowTextW")
	procIsWindowVisible          = user32API.NewProc("IsWindowVisible")
	procIsIconic                 = user32API.NewProc("IsIconic")
	procGetWindow                = user32API.NewProc("GetWindow")
	procGetWindowLongW           = user32API.NewProc("GetWindowLongW")
	procShowWindow               = user32API.NewProc("ShowWindow")
	procSetForegroundWindow      = user32API.NewProc("SetForegroundWindow")
	procSetWindowDisplayAffinity = user32API.NewProc("SetWindowDisplayAffinity")
	procSystemParametersInfoW    = user32API.NewProc("SystemParametersInfoW")
)

const (
	swMinimize = 6 // SW_MINIMIZE
	swRestore  = 9 // SW_RESTORE
	wdaMonitor = 1 // WDA_MONITOR

	gwOwner    = 4 // GW_OWNER
	gwlExStyle = ^uintptr(19)

	wsExTopmost = 0x00000008

	hwndTopmost   = ^uintptr(0) // HWND_TOPMOST = -1
	swpNoSize     = 0x0001
	swpNoMove     = 0x0002
	swpNoZorder   = 0x0004
	swpNoActivate = 0x0010
	swpShowWindow = 0x0040

	spiGetWorkArea = 0x0030 // SystemParametersInfo(SPI_GETWORKAREA)
)

// winRect 是 Win32 RECT。
type winRect struct{ Left, Top, Right, Bottom int32 }

func (r winRect) Dx() int { return int(r.Right - r.Left) }
func (r winRect) Dy() int { return int(r.Bottom - r.Top) }

// findWindow 按标题找顶层窗口，找不到返回 0。
func findWindow(title string) windows.HWND {
	p, err := syscall.UTF16PtrFromString(title)
	if err != nil {
		return 0
	}
	h, _, _ := procFindWindowW.Call(0, uintptr(unsafe.Pointer(p)))
	return windows.HWND(h)
}

// windowRect 读窗口矩形（物理像素；本进程已 PerMonitorV2，不会被虚拟化）。
func windowRect(h windows.HWND) winRect {
	var r winRect
	procGetWindowRect.Call(uintptr(h), uintptr(unsafe.Pointer(&r)))
	return r
}

// windowTitle 读窗口标题（同进程调用，不涉及跨进程读取）。
func windowTitle(h windows.HWND) string {
	var buf [512]uint16
	n, _, _ := procGetWindowTextW.Call(uintptr(h), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf[:n])
}

func windowOwner(h windows.HWND) windows.HWND {
	o, _, _ := procGetWindow.Call(uintptr(h), gwOwner)
	return windows.HWND(o)
}

func isVisible(h windows.HWND) bool {
	v, _, _ := procIsWindowVisible.Call(uintptr(h))
	return v != 0
}

// isIconic 问的是"有没有真的变成图标/最小化"。
//
// ⚠️ 复核最小化**不能**用 IsWindowVisible：最小化的窗口仍然带 WS_VISIBLE 样式位，
// IsWindowVisible 照样返回 true（实测踩过）。判据必须是 IsIconic。
func isIconic(h windows.HWND) bool {
	v, _, _ := procIsIconic.Call(uintptr(h))
	return v != 0
}

func isTopmost(h windows.HWND) bool {
	return exStyleOf(h)&wsExTopmost != 0
}

// exStyleOf 读窗口扩展样式（诊断用：置顶位 WS_EX_TOPMOST = 0x8）。
func exStyleOf(h windows.HWND) uint32 {
	ex, _, _ := procGetWindowLongW.Call(uintptr(h), gwlExStyle)
	return uint32(ex)
}

// topmost 只把窗口提到 Z 序顶端，不动位置尺寸。
//
// ⚠️ SWP_NOMOVE 不能省：外部**移动** Gio 窗口会让它不再产生帧事件（PLAN 约束 5）。
// SWP_NOACTIVATE 是顺手加的：置顶不该抢走用户正在输入的窗口焦点。
func topmost(h windows.HWND) bool {
	r, _, _ := procSetWindowPos.Call(uintptr(h), hwndTopmost, 0, 0, 0, 0,
		uintptr(swpNoSize|swpNoMove|swpNoActivate))
	return r != 0
}

// moveAndTopmost 把窗口挪到 (x,y) 并置顶（尺寸不变）。
//
// ⚠️ 这条路径会触发 PLAN 约束 5 的风险（外部移动后帧事件可能停摆），
// 调用方必须验证"移动后悬浮条内容仍能刷新"，见 internal/ui/hud.go。
func moveAndTopmost(h windows.HWND, x, y int) bool {
	r, _, _ := procSetWindowPos.Call(uintptr(h), hwndTopmost, uintptr(int32(x)), uintptr(int32(y)), 0, 0,
		uintptr(swpNoSize|swpNoActivate|swpShowWindow))
	return r != 0
}

// moveOnly 只挪位置：不动 Z 序、不动尺寸（把窗口搬到副屏用）。
func moveOnly(h windows.HWND, x, y int) bool {
	r, _, _ := procSetWindowPos.Call(uintptr(h), 0, uintptr(int32(x)), uintptr(int32(y)), 0, 0,
		uintptr(swpNoSize|swpNoZorder|swpShowWindow))
	return r != 0
}

// primaryWorkArea 返回主屏工作区（排除任务栏），物理像素。
func primaryWorkArea() (winRect, bool) {
	var r winRect
	ok, _, _ := procSystemParametersInfoW.Call(spiGetWorkArea, 0, uintptr(unsafe.Pointer(&r)), 0)
	return r, ok != 0
}
