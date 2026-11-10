package capture

import (
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// 顶层窗口枚举（P1-2 跟随窗口分享）。
//
// 为什么需要它：演示代码时真正要分享的是"一个 IDE 窗口"，而不是整块屏幕。
// 整屏 2K 是 5.2M 像素，一个 1280×800 的窗口只有 1M —— 带宽和 CPU 都差 3~6 倍，
// 而且不会把别的窗口（聊天、邮件）播出去。
//
// 过滤规则都是实测踩出来的：
//   - 只能看**可见且非最小化**的顶层窗（最小化的窗口 rect 在屏幕外，采出来是黑的）；
//   - 排除本进程自己的窗口（悬浮条、主窗），否则会把自己播给自己；
//   - 排除 WS_EX_TOOLWINDOW（输入法候选、浮动提示）与 DWM cloak（UWP 的隐藏壳）。

var (
	user32dll            = windows.NewLazySystemDLL("user32.dll")
	dwmapi               = windows.NewLazySystemDLL("dwmapi.dll")
	pEnumWindows         = user32dll.NewProc("EnumWindows")
	pIsWindowVisible     = user32dll.NewProc("IsWindowVisible")
	pIsIconic            = user32dll.NewProc("IsIconic")
	pGetWindowTextW      = user32dll.NewProc("GetWindowTextW")
	pGetWindowTextLength = user32dll.NewProc("GetWindowTextLengthW")
	pGetWindowRect       = user32dll.NewProc("GetWindowRect")
	pGetWindowLongW      = user32dll.NewProc("GetWindowLongW")
	pGetWindowThreadPID  = user32dll.NewProc("GetWindowThreadProcessId")
	pDwmGetWindowAttr    = dwmapi.NewProc("DwmGetWindowAttribute")
)

const (
	wsExToolWindow = 0x00000080
	dwmwaCloaked   = 14
)

// GWL_EXSTYLE = -20：GetWindowLongW 的参数是 32 位 int，x64 下要按补码传。
var gwlExStyle = ^uintptr(19)

// WindowInfo 是一个可选来分享的顶层窗口。
type WindowInfo struct {
	Handle    uintptr
	Title     string
	X, Y      int // 虚拟屏幕坐标（物理像素）
	W, H      int
	Minimized bool
}

// ListWindows 枚举当前可分享的顶层窗口（按标题去重、按面积从大到小）。
func ListWindows() []WindowInfo {
	self := uint32(windows.GetCurrentProcessId())
	var out []WindowInfo
	cb := syscall.NewCallback(func(hwnd uintptr) uintptr {
		if w, ok := windowInfo(hwnd, self); ok {
			out = append(out, w)
		}
		return 1 // 继续枚举
	})
	_, _, _ = pEnumWindows.Call(cb, 0)
	// 面积大的排前面：用户多半想分享 IDE/浏览器，而不是一个小对话框。
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].W*out[j].H > out[j-1].W*out[j-1].H; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// FindWindowByTitle 按标题子串找一个窗口（不区分大小写）。
func FindWindowByTitle(sub string) (WindowInfo, bool) {
	sub = strings.ToLower(strings.TrimSpace(sub))
	if sub == "" {
		return WindowInfo{}, false
	}
	for _, w := range ListWindows() {
		if strings.Contains(strings.ToLower(w.Title), sub) {
			return w, true
		}
	}
	return WindowInfo{}, false
}

// WindowRect 读一个窗口当前的矩形（跟随用）。ok=false 表示窗口已消失。
func WindowRect(hwnd uintptr) (r Rect, minimized bool, ok bool) {
	if hwnd == 0 {
		return Rect{}, false, false
	}
	if v, _, _ := pIsWindowVisible.Call(hwnd); v == 0 {
		return Rect{}, false, false
	}
	min := false
	if v, _, _ := pIsIconic.Call(hwnd); v != 0 {
		min = true
	}
	var wr struct{ Left, Top, Right, Bottom int32 }
	if v, _, _ := pGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&wr))); v == 0 {
		return Rect{}, false, false
	}
	return Rect{X: int(wr.Left), Y: int(wr.Top), W: int(wr.Right - wr.Left), H: int(wr.Bottom - wr.Top)}, min, true
}

func windowInfo(hwnd uintptr, selfPID uint32) (WindowInfo, bool) {
	if v, _, _ := pIsWindowVisible.Call(hwnd); v == 0 {
		return WindowInfo{}, false
	}
	// 本进程的窗口不列（主窗/悬浮条会形成自摄入）。
	var pid uint32
	_, _, _ = pGetWindowThreadPID.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	if pid == selfPID {
		return WindowInfo{}, false
	}
	// 工具窗口 / 被 DWM 隐藏的壳（UWP）不列。
	if ex, _, _ := pGetWindowLongW.Call(hwnd, gwlExStyle); ex&wsExToolWindow != 0 {
		return WindowInfo{}, false
	}
	var cloaked uint32
	if v, _, _ := pDwmGetWindowAttr.Call(hwnd, dwmwaCloaked, uintptr(unsafe.Pointer(&cloaked)), unsafe.Sizeof(cloaked)); v == 0 && cloaked != 0 {
		return WindowInfo{}, false
	}
	n, _, _ := pGetWindowTextLength.Call(hwnd)
	if n == 0 {
		return WindowInfo{}, false
	}
	buf := make([]uint16, n+1)
	_, _, _ = pGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	title := windows.UTF16ToString(buf)
	if strings.TrimSpace(title) == "" {
		return WindowInfo{}, false
	}
	r, min, ok := WindowRect(hwnd)
	if !ok || r.W < 120 || r.H < 80 {
		return WindowInfo{}, false
	}
	return WindowInfo{Handle: hwnd, Title: title, X: r.X, Y: r.Y, W: r.W, H: r.H, Minimized: min}, true
}
