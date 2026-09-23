// uiprobe — 阶段 0 · S0-5
// 验证 Gio 能否在 CGO_ENABLED=0 下构建出可运行的 Windows 窗口，并渲染：
//   ① 纯色背景（基础渲染管线）
//   ② 一张 image.NRGBA 位图（视频帧渲染路径的前置验证）
//   ③ 文本（UI 可用性）
// 窗口渲染满若干帧后自行退出并打印 GIO_OK，便于自动化测试判定。
package main

import (
	"fmt"
	"image"
	"image/color"
	"log"
	"os"
	"syscall"
	"time"
	"unsafe"

	"gioui.org/app"
	"gioui.org/font/gofont"
	"gioui.org/io/system"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget/material"
)

// makeFrame 模拟一帧视频画面（渐变 + 色块）
func makeFrame(w, h int) image.Image {
	m := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			m.SetNRGBA(x, y, color.NRGBA{
				R: uint8(x * 255 / w),
				G: uint8(y * 255 / h),
				B: 160,
				A: 255,
			})
		}
	}
	for y := h / 4; y < h*3/4; y++ {
		for x := w / 4; x < w*3/4; x++ {
			m.SetNRGBA(x, y, color.NRGBA{R: 240, G: 240, B: 240, A: 255})
		}
	}
	return m
}

func main() {
	logf, err := os.Create("uiprobe.log")
	if err == nil {
		defer logf.Close()
		log.SetOutput(logf)
	}
	log.Println("main: start")

	go func() {
		msg := run()
		log.Println("run returned:", msg)
	}()

	log.Println("main: entering app.Main()")
	app.Main()
	log.Println("main: app.Main() returned")
}

const winTitle = "GoShare · S0-5 Gio 验证"

// checkWindowVisible 用 Win32 FindWindowW/IsWindowVisible 断言窗口真的存在于系统窗口表中
func checkWindowVisible(title string) {
	u := syscall.NewLazyDLL("user32.dll")
	fw := u.NewProc("FindWindowW")
	iv := u.NewProc("IsWindowVisible")
	p, err := syscall.UTF16PtrFromString(title)
	if err != nil {
		log.Printf("check: UTF16 转换失败 %v", err)
		return
	}
	hwnd, _, _ := fw.Call(0, uintptr(unsafe.Pointer(p)))
	if hwnd == 0 {
		log.Printf("check: 未找到窗口 hwnd=0 ❌")
		return
	}
	v, _, _ := iv.Call(hwnd)
	log.Printf("check: 找到窗口 hwnd=0x%x visible=%d %s", hwnd, v,
		map[bool]string{true: "✅ 窗口真实可见", false: "⚠️ 不可见"}[v != 0])
}

func run() string {
	w := new(app.Window)
	w.Option(app.Title(winTitle), app.Size(unit.Dp(520), unit.Dp(360)))
	log.Println("run: window created")

	th := material.NewTheme()
	th.Shaper = text.NewShaper(text.WithCollection(gofont.Collection()))

	frameImg := makeFrame(320, 180)
	imgOp := paint.NewImageOp(frameImg)

	var ops op.Ops
	frames := 0
	start := time.Now()

	for {
		e := w.Event()
		switch e := e.(type) {
		case app.DestroyEvent:
			return fmt.Sprintf("GIO_OK frames=%d err=%v", frames, e.Err)
		case app.FrameEvent:
			gtx := app.NewContext(&ops, e)

			// ① 背景
			paint.ColorOp{Color: color.NRGBA{R: 18, G: 22, B: 30, A: 255}}.Add(gtx.Ops)
			paint.PaintOp{}.Add(gtx.Ops)

			// ② 位图（视频帧渲染路径）
			st := clip.Rect{Max: image.Pt(320, 180)}.Push(gtx.Ops)
			imgOp.Add(gtx.Ops)
			paint.PaintOp{}.Add(gtx.Ops)
			st.Pop()

			// ③ 文本
			title := material.H4(th, "Gio 渲染正常")
			title.Color = color.NRGBA{R: 235, G: 240, B: 255, A: 255}
			title.Layout(gtx)

			e.Frame(gtx.Ops)
			frames++
			if frames == 1 {
				log.Printf("run: rendered frame %d", frames)
				checkWindowVisible(winTitle)
			} else if frames == 10 {
				// 等窗口完成 ShowWindow 后再断言一次
				checkWindowVisible(winTitle)
			} else if frames%20 == 0 {
				log.Printf("run: rendered frame %d", frames)
			}

			// Gio 是按需渲染，强制持续出帧以便观察稳定性
			if frames < 60 {
				w.Invalidate()
			}

			// 渲染满 60 帧后自行关闭，便于自动化判定
			if frames >= 60 || time.Since(start) > 10*time.Second {
				log.Printf("run: done, frames=%d, elapsed=%v", frames, time.Since(start))
				w.Perform(system.ActionClose)
			}
		}
	}
}
