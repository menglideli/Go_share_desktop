// Package ui 提供基于 Gio 的界面。
//
// 阶段 1 的交付物是本地预览窗：把采集到的画面（含区域裁切与光标叠加）
// 实时渲染出来，并带一条诊断 HUD。
package ui

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"log"
	"os"
	"time"

	"gioui.org/app"
	"gioui.org/io/system"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"goshare/internal/capture"
)

// 配色（深色主题）
var (
	colBG      = color.NRGBA{R: 15, G: 20, B: 25, A: 255}
	colPanel   = color.NRGBA{R: 22, G: 28, B: 34, A: 255}
	colText    = color.NRGBA{R: 230, G: 237, B: 243, A: 255}
	colDim     = color.NRGBA{R: 139, G: 148, B: 158, A: 255}
	colAccent  = color.NRGBA{R: 74, G: 158, B: 255, A: 255}
	colWarn    = color.NRGBA{R: 240, G: 180, B: 41, A: 255}
	colGood    = color.NRGBA{R: 63, G: 185, B: 120, A: 255}
)

// PreviewConfig 描述预览窗的行为。
type PreviewConfig struct {
	Display capture.Display
	Region  capture.Rect
	FPS     int
	Cursor  bool
	Title   string
	// ExitAfter 非空时，窗口会在该时长后自动关闭（用于自动化验证）。
	ExitAfter time.Duration
}

// RunPreview 打开预览窗并阻塞，直到窗口关闭。
func RunPreview(ctx context.Context, cfg PreviewConfig) error {
	if cfg.Title == "" {
		cfg.Title = "GoShare · 本地预览"
	}
	if cfg.FPS <= 0 {
		cfg.FPS = 30
	}

	src, err := capture.NewSource(ctx, capture.Options{
		Display: cfg.Display,
		Region:  cfg.Region,
		FPS:     cfg.FPS,
		Cursor:  cfg.Cursor,
	})
	if err != nil {
		return err
	}
	defer src.Close()

	region := src.Region()
	view := NewView(region.W, region.H, FitScale(region.W, region.H, 1280, 720))

	stop := make(chan struct{})
	go captureLoop(ctx, src, view, stop)

	// 看门狗：窗口若始终不产生帧事件（锁屏 / 无头 / 远程会话下会这样），
	// 自动关闭逻辑就永远触发不了，自动化会挂死。这里兜底强制退出。
	if cfg.ExitAfter > 0 {
		go func() {
			time.Sleep(cfg.ExitAfter + 6*time.Second)
			log.Printf("看门狗: 超过 ExitAfter+6s 仍未退出，强制结束")
			log.Printf("        → 通常意味着窗口未产生帧事件（锁屏/无头环境）")
			os.Exit(3)
		}()
	}

	var runErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		runErr = loop(src, view, cfg)
	}()

	app.Main()
	close(stop)
	<-done
	return runErr
}

// captureLoop 持续取帧并交给视图。
func captureLoop(ctx context.Context, src *capture.Source, view *View, stop chan struct{}) {
	for {
		select {
		case <-stop:
			return
		default:
		}
		c, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		f, err := src.WaitFrame(c)
		cancel()
		if err == nil && !f.Empty() {
			view.Update(f)
		}
	}
}

func loop(src *capture.Source, view *View, cfg PreviewConfig) error {
	w := new(app.Window)
	w.Option(
		app.Title(cfg.Title),
		app.Size(unit.Dp(1100), unit.Dp(720)),
		app.MinSize(unit.Dp(560), unit.Dp(360)),
	)
	log.Printf("loop: 窗口已创建 title=%q", cfg.Title)

	th := material.NewTheme()
	th.Shaper = text.NewShaper(text.WithCollection(loadFontFaces()))
	// 深色主题：把默认的黑色前景改为浅色
	th.Palette.Fg = colText
	th.Palette.Bg = colBG

	var ops op.Ops
	start := time.Now()
	frames := 0
	closing := false
	lastStamp := uint64(0)
	newFrames := 0
	lastFpsAt := time.Now()
	fps := 0.0

	for {
		e := w.Event()
		switch e := e.(type) {
		case app.DestroyEvent:
			return e.Err
		case app.FrameEvent:
			gtx := app.NewContext(&ops, e)

			// 有新的采集帧才重新做像素转换
			if st := view.Stamp(); st != lastStamp {
				lastStamp = st
				newFrames++
			}
			view.Render()

			layoutPreview(gtx, th, view, src)

			e.Frame(gtx.Ops)
			frames++

			if now := time.Now(); now.Sub(lastFpsAt) >= 500*time.Millisecond {
				fps = float64(newFrames) / now.Sub(lastFpsAt).Seconds()
				lastFpsAt = now
				newFrames = 0
			}
			_ = fps

			// 第 1 帧时 Gio 通常还没完成 ShowWindow，第 10 帧再断言才准
			if frames == 1 || frames == 10 {
				log.Printf("loop: 已渲染 %d 帧", frames)
				LogWindowVisible(cfg.Title)
			} else if frames%60 == 0 {
				log.Printf("loop: 已渲染 %d 帧 · 采集 %.1f fps · 后端 %s",
					frames, src.Stats().FPS, src.Stats().Backend)
			}

			if cfg.ExitAfter > 0 && time.Since(start) > cfg.ExitAfter {
				if !closing {
					closing = true
					log.Printf("loop: ExitAfter 到时，关闭窗口（共渲染 %d 帧）", frames)
					w.Perform(system.ActionClose)
					// 兜底：实测 Windows 上关窗后进程不一定自行退出
					// （Gio 的消息循环仍在跑），自动化等不到 DestroyEvent。
					go func() {
						time.Sleep(2 * time.Second)
						log.Printf("loop: 关窗后进程未自行退出，兜底结束")
						os.Exit(0)
					}()
				}
			}
			// Gio 按需渲染；预览需要持续出帧
			w.Invalidate()
		}
	}
}

func layoutPreview(gtx layout.Context, th *material.Theme, view *View, src *capture.Source) layout.Dimensions {
	// 背景
	paint.ColorOp{Color: colBG}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		// 视频区
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			ins := layout.UniformInset(unit.Dp(12))
			return ins.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				img := view.Image()
				if img == nil || img.Bounds().Empty() {
					return layout.Dimensions{Size: gtx.Constraints.Min}
				}
				// contain：保持宽高比，绝不拉伸（PLAN 层 1）
				widget.Image{
					Src:      paint.NewImageOp(img),
					Fit:      widget.Contain,
					Position: layout.Center,
					// 1 图片像素 = 1 物理像素，避免高 DPI 下被 dp 放大
					Scale: 1 / gtx.Metric.PxPerDp,
				}.Layout(gtx)
				return layout.Dimensions{Size: gtx.Constraints.Max}
			})
		}),
		// 状态栏
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layoutStatusBar(gtx, th, view, src)
		}),
	)
}

func layoutStatusBar(gtx layout.Context, th *material.Theme, view *View, src *capture.Source) layout.Dimensions {
	st := src.Stats()
	srcW, srcH := view.SrcSize()
	img := view.Image()
	showW, showH := 0, 0
	if img != nil {
		showW, showH = img.Bounds().Dx(), img.Bounds().Dy()
	}

	// ⚠️ 背景必须"先量高度、再按高度裁剪"。
	// 曾经写成 clip.Rect{Max: gtx.Constraints.Max}，但在 layout.Rigid 里
	// 这个 Max 是整个窗口尺寸（Flex 不会给 Rigid 设高度上界），
	// 结果面板色铺满整窗、把视频画面整块盖掉 —— 症状是"画面全黑只剩状态栏文字"。
	// 同一个坑在 internal/ui/viewer.go 的 layoutStatusBar2 也踩过。
	macro := op.Record(gtx.Ops)
	gtx.Constraints.Min = image.Point{}
	inner := layout.UniformInset(unit.Dp(10)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		label := func(s string, c color.NRGBA) layout.Widget {
			l := material.Body2(th, s)
			l.Color = c
			return l.Layout
		}
		gap := layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Dimensions{Size: image.Pt(gtx.Dp(unit.Dp(18)), 0)}
		})

		backendTxt := fmt.Sprintf("采集后端 %s", st.Backend)
		if src.Note() != "" && st.Backend != "duplication" {
			backendTxt += "（已降级）"
		}

		return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
			layout.Rigid(label(backendTxt, statusColor(st.Backend))),
			gap,
			layout.Rigid(label(fmt.Sprintf("采集 %.1f fps", st.FPS), colText)),
			gap,
			layout.Rigid(label(fmt.Sprintf("单帧 %.2f ms", float64(st.MeanCapture.Microseconds())/1000.0), colText)),
			gap,
			layout.Rigid(label(fmt.Sprintf("源 %d×%d", srcW, srcH), colDim)),
			gap,
			layout.Rigid(label(fmt.Sprintf("显示 %d×%d", showW, showH), colDim)),
			gap,
			layout.Rigid(label(fmt.Sprintf("已采集 %d 帧", st.Frames), colDim)),
		)
	})
	call := macro.Stop()

	// 背景只填状态栏这一条，高度用刚量出来的 inner.Size.Y
	area := clip.Rect{Max: image.Pt(gtx.Constraints.Max.X, inner.Size.Y)}
	stack := area.Push(gtx.Ops)
	paint.ColorOp{Color: colPanel}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	stack.Pop()

	call.Add(gtx.Ops)
	return inner
}

// InfoLine 返回一行诊断文本，供 HUD 使用。
func InfoLine(src *capture.Source) string {
	st := src.Stats()
	return fmt.Sprintf("后端 %s · %.1f fps · 采集均值 %.2f ms",
		st.Backend, st.FPS, float64(st.MeanCapture.Microseconds())/1000.0)
}

// statusColor 按后端给出颜色：duplication 为正常色，gdi 为警告色。
func statusColor(backend string) color.NRGBA {
	if backend == "duplication" {
		return colGood
	}
	return colWarn
}

var _ = colAccent
