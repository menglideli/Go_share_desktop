// 探针：验证"无边框 Gio 窗口 + Win32 SetWindowPos(HWND_TOPMOST) 只改 Z 序"
// 不会掐死帧事件。
//
// 背景：PLAN.md 约束 5 实测过"外部 SetWindowPos **移动** Gio 窗口后不再产生
// 帧事件（停在 10 帧）"。悬浮条需要置顶，置顶用的也是 SetWindowPos 但带
// SWP_NOMOVE —— 必须实测确认这条路径安全，不能拿"应该没事"当结论。
//
// 判定：置顶前后各统计 2 秒帧数，置顶后帧数塌掉一半以上即 FAIL。
// 探针自带 Win32 调用副本（与 internal/ui/hud_windows.go 同一套参数），
// 若以后改置顶参数要同步改这里。
package main

import (
	"image/color"
	"log"
	"os"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"gioui.org/app"
	"gioui.org/io/system"
	"gioui.org/op"
	"gioui.org/op/paint"
	"gioui.org/unit"
)

func makeTopmost(title string) bool {
	u := syscall.NewLazyDLL("user32.dll")
	fw := u.NewProc("FindWindowW")
	swp := u.NewProc("SetWindowPos")
	p, err := syscall.UTF16PtrFromString(title)
	if err != nil {
		return false
	}
	hwnd, _, _ := fw.Call(0, uintptr(unsafe.Pointer(p)))
	if hwnd == 0 {
		return false
	}
	const (
		hwndTopmost   = ^uintptr(0) // -1
		swpNoSize     = 0x0001
		swpNoMove     = 0x0002
		swpNoActivate = 0x0010
	)
	r, _, _ := swp.Call(hwnd, hwndTopmost, 0, 0, 0, 0,
		uintptr(swpNoSize|swpNoMove|swpNoActivate))
	return r != 0
}

func main() {
	log.SetOutput(os.Stdout)
	var framesBefore, framesAfter atomic.Int64
	var firstEvent atomic.Bool

	// 看门狗：任何环节卡死都要留下证据再死，不能无声挂着。
	time.AfterFunc(12*time.Second, func() {
		log.Printf("probe: 看门狗超时 —— 首事件=%v 置顶前帧=%d 置顶后帧=%d",
			firstEvent.Load(), framesBefore.Load(), framesAfter.Load())
		os.Exit(3)
	})
	log.Printf("probe: 启动")

	go func() {
		w := new(app.Window)
		w.Option(app.Title("hudtop-probe"), app.Size(unit.Dp(320), unit.Dp(46)), app.Decorated(false))
		var ops op.Ops
		start := time.Now()
		didTopmost := false
		didClose := false
		log.Printf("probe: 进入事件循环")
		for {
			e := w.Event()
			if firstEvent.CompareAndSwap(false, true) {
				log.Printf("probe: 收到首个窗口事件 %T", e)
			}
			switch e := e.(type) {
			case app.DestroyEvent:
				log.Printf("probe: 收到 DestroyEvent（关窗路径有效）")
				return
			case app.FrameEvent:
				gtx := app.NewContext(&ops, e)
				paint.Fill(gtx.Ops, color.NRGBA{R: 22, G: 28, B: 34, A: 255})
				e.Frame(gtx.Ops)
				if time.Since(start) < 2*time.Second {
					framesBefore.Add(1)
				} else {
					if !didTopmost {
						didTopmost = true
						// ⚠️ 必须在**独立 goroutine** 里调 Win32：
						// 在 FrameEvent 处理里直接调 FindWindowW/SetWindowPos
						// 会卡死事件循环（实测：帧停、日志都打不出来）——
						// 与 Gio 正在处理本窗消息时重入窗口过程有关。
						// internal/ui/hud.go 的置顶同样是独立 goroutine。
						go func() {
							ok := false
							for i := 0; i < 30; i++ {
								if makeTopmost("hudtop-probe") {
									ok = true
									break
								}
								time.Sleep(50 * time.Millisecond)
							}
							log.Printf("probe: 置顶设置 ok=%v", ok)
							if !ok {
								os.Exit(2)
							}
						}()
					}
					framesAfter.Add(1)
				}
				if time.Since(start) > 4*time.Second && !didClose {
					didClose = true
					log.Printf("probe: 请求关窗（ActionClose）")
					w.Perform(system.ActionClose)
				}
				w.Invalidate()
			}
		}
	}()

	app.Main()
	b, a := framesBefore.Load(), framesAfter.Load()
	log.Printf("probe: 置顶前 2s 帧数=%d，置顶后 2s 帧数=%d", b, a)
	if b < 10 {
		log.Printf("probe: FAIL —— 置顶前帧数就异常（%d），探针本身有问题", b)
		os.Exit(2)
	}
	if a < b/2 {
		log.Printf("probe: FAIL —— 置顶后帧事件被掐死（%d → %d），HUD 不能走 SetWindowPos 置顶", b, a)
		os.Exit(1)
	}
	log.Printf("probe: PASS —— 只改 Z 序不影响帧事件")
}
