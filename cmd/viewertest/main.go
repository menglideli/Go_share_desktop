// viewertest — 隔离实验：用真实的 ui.RunViewer 显示一张自造亮色图，
// 完全不经过采集/编解码/网络。
//
// 判定标准很干脆：窗口里出现彩色渐变 = 渲染层没问题，
// 那么 sharedemo 的黑就只能来自"采集到的内容本身很暗"。
package main

import (
	"context"
	"flag"
	"image"
	"image/color"
	"log"
	"os"
	"time"

	"gioui.org/app"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"

	"goshare/internal/ui"
)

func makeFrame(w, h, tick int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	// 一条随时间移动的亮条，用来确认画面在"动"，不是一张静止图
	barX := (tick * 40) % (w - 200)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			p := img.PixOffset(x, y)
			c := color.NRGBA{
				R: uint8(x * 255 / w),
				G: uint8(y * 255 / h),
				B: 160,
				A: 255,
			}
			if x >= barX && x < barX+200 {
				c = color.NRGBA{R: 255, G: 255, B: 255, A: 255}
			}
			img.Pix[p+0], img.Pix[p+1], img.Pix[p+2], img.Pix[p+3] = c.R, c.G, c.B, c.A
		}
	}
	return img
}

// minimalLoop 是最小渲染循环：不用 ui.RunViewer，但数据源仍是同一条 channel。
// 用来二分：如果它能显示，问题就在 ui.RunViewer 内部；如果它也黑，问题在数据源。
func minimalLoop(frames <-chan *image.NRGBA, seconds int) {
	w := new(app.Window)
	w.Option(app.Title("ViewerTest-minimal"), app.Size(unit.Dp(1280), unit.Dp(760)))
	var ops op.Ops
	cur := (*image.NRGBA)(nil)
	start := time.Now()
	frames2 := 0
	for {
		e := w.Event()
		switch e := e.(type) {
		case app.DestroyEvent:
			log.Printf("minimal: destroy err=%v frames=%d", e.Err, frames2)
			os.Exit(0)
		case app.FrameEvent:
			gtx := app.NewContext(&ops, e)
			select {
			case img := <-frames:
				if img != nil {
					cur = img
				}
			default:
			}
			paint.ColorOp{Color: color.NRGBA{R: 15, G: 20, B: 25, A: 255}}.Add(gtx.Ops)
			paint.PaintOp{}.Add(gtx.Ops)
			if cur != nil {
				dims := widget.Image{
					Src:      paint.NewImageOp(cur),
					Fit:      widget.Contain,
					Position: layout.Center,
					Scale:    1 / gtx.Metric.PxPerDp,
				}.Layout(gtx)
				if frames2 < 3 {
					log.Printf("minimal: 图像 %v 约束 %v→%v 算出 %v",
						cur.Bounds().Size(), gtx.Constraints.Min, gtx.Constraints.Max, dims.Size)
				}
			}
			e.Frame(gtx.Ops)
			frames2++
			if time.Since(start) > time.Duration(seconds)*time.Second {
				log.Printf("minimal: 到时退出 frames=%d", frames2)
				os.Exit(0)
			}
			w.Invalidate()
		}
	}
}

func main() {
	minimal := flag.Bool("minimal", false, "用最小渲染循环替代 ui.RunViewer")
	flag.Parse()

	ctx := context.Background()
	frames := make(chan *image.NRGBA, 2)

	go func() {
		for tick := 0; ; tick++ {
			img := makeFrame(1920, 1080, tick)
			select {
			case frames <- img:
			default:
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()

	if *minimal {
		go minimalLoop(frames, 20)
		app.Main()
		return
	}

	err := ui.RunViewer(ctx, ui.ViewerConfig{
		Title:     "ViewerTest · 隔离渲染实验",
		Frames:    frames,
		ExitAfter: 25 * time.Second,
		Status:    func() string { return "viewertest 自造图 1920x1080" },
	})
	log.Printf("viewertest 结束 err=%v", err)
}
