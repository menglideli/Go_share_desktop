// redwin 在屏幕上铺一块纯红 (255,0,0) 窗口，供"真实链路颜色保真"e2e 使用：
// host 采集本屏 → 观看端 dump 收到的帧 → 断言红像素还是红的。
//
// 用法：redwin -hold 90s
package main

import (
	"flag"
	"image"
	"image/color"
	"os"
	"time"

	"gioui.org/app"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"

	"syscall"
	"unsafe"
)

// 用户屏幕上可能有别的最大化窗口盖住本探针 —— 置顶自己。
// 只改 z-order（SWP_NOMOVE|SWP_NOSIZE），不移动窗口，避免触发
// "外部 SetWindowPos 移动 Gio 窗口后不再产生帧事件"的坑。
var (
	user32         = syscall.NewLazyDLL("user32.dll")
	procFindWindow = user32.NewProc("FindWindowW")
	procSetPos     = user32.NewProc("SetWindowPos")
)

func topmost(title string) {
	t, _ := syscall.UTF16PtrFromString(title)
	hwnd, _, _ := procFindWindow.Call(0, uintptr(unsafe.Pointer(t)))
	if hwnd == 0 {
		return
	}
	const (
		hwndTopmost = ^uintptr(0) // -1
		swpNoMove   = 0x0002
		swpNoSize   = 0x0001
	)
	procSetPos.Call(hwnd, hwndTopmost, 0, 0, 0, 0, swpNoMove|swpNoSize)
}

func main() {
	hold := flag.Duration("hold", 90*time.Second, "保持时长")
	flag.Parse()

	go func() {
		w := new(app.Window)
		w.Option(app.Title("redwin-color-probe"), app.Size(unit.Dp(1280), unit.Dp(800)))
		go func() {
			time.Sleep(1500 * time.Millisecond) // 等窗口建出来再置顶
			topmost("redwin-color-probe")
		}()
		var ops op.Ops
		red := color.NRGBA{R: 255, G: 0, B: 0, A: 255}
		for {
			e := w.Event()
			switch e := e.(type) {
			case app.DestroyEvent:
				os.Exit(0)
			case app.FrameEvent:
				gtx := app.NewContext(&ops, e)
				stack := clip.Rect(image.Rectangle{Max: gtx.Constraints.Max}).Push(gtx.Ops)
				paint.Fill(gtx.Ops, red)
				stack.Pop()
				e.Frame(gtx.Ops)
			}
		}
	}()
	// 定时自杀（app.Main 永不返回，必须从另一个 goroutine 退出）
	go func() {
		time.Sleep(*hold)
		os.Exit(0)
	}()
	app.Main()
	_ = layout.Center
}
