package ui

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"log"
	"os"
	"sync"
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
)

// ViewerConfig 是观看端窗口的配置。
type ViewerConfig struct {
	Title string
	// Frames 是待显示的画面。渲染方用完会通过 Recycle 归还，
	// 供给端拿到后才敢覆写那块内存 —— 避免解码线程与渲染线程抢同一张画布。
	Frames <-chan *image.NRGBA
	// Recycle 是画布归还通道（可为 nil，表示不复用）。
	Recycle chan<- *image.NRGBA
	// Status 返回状态栏文本（可为 nil）。
	Status func() string
	// ExitAfter 大于 0 时，到时自动关闭（自动化验证用）。
	ExitAfter time.Duration
	// OnReady 窗口渲染出第一帧后调用（例如把窗口挪到副屏，避免采集到自己）。
	OnReady func()
}

// RunViewer 打开观看端窗口并持续渲染收到的画面。
func RunViewer(ctx context.Context, cfg ViewerConfig) error {
	stop := make(chan struct{})
	var closeErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		closeErr = viewerLoop(ctx, cfg, stop)
	}()

	// 看门狗：到时直接退出进程。
	//
	// ⚠️ 不能用 channel 通知事件循环 —— 循环阻塞在 w.Event() 上，
	// 窗口一旦不再产生事件（锁屏/遮挡/后端暂停），close(stop) 永远没人接收，
	// 进程就会静默挂死。实测踩过：看门狗日志打了，进程还在。
	if cfg.ExitAfter > 0 {
		go func() {
			time.Sleep(cfg.ExitAfter + 20*time.Second)
			select {
			case <-done:
			default:
				log.Printf("viewer: 看门狗触发，硬退出（帧数=%d）", atomicFrames())
				os.Exit(3)
			}
		}()
	}

	app.Main()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		close(stop)
		<-done
	}
	return closeErr
}

func viewerLoop(ctx context.Context, cfg ViewerConfig, stop <-chan struct{}) (err error) {
	// 把 panic 转成错误：Gio 后端一旦在渲染中 panic，进程会静默崩溃且没有 stderr，
	// 只留下一个看不懂的退出码（实测 0xCFFFFFFF）。这里必须兜住，否则排查成本极高。
	defer func() {
		if r := recover(); r != nil {
			log.Printf("viewer: PANIC %v", r)
			err = fmt.Errorf("viewer panic: %v", r)
		}
	}()
	return viewerLoopInner(ctx, cfg, stop)
}

func viewerLoopInner(ctx context.Context, cfg ViewerConfig, stop <-chan struct{}) error {
	th := material.NewTheme()
	th.Shaper = text.NewShaper(text.WithCollection(loadFontFaces()))
	th.Palette.Bg = colBG
	th.Palette.Fg = color.NRGBA{R: 230, G: 237, B: 243, A: 255}

	w := new(app.Window)
	w.Option(
		app.Title(cfg.Title),
		app.Size(unit.Dp(1280), unit.Dp(760)),
		app.MinSize(unit.Dp(480), unit.Dp(320)),
	)
	log.Printf("viewer: 窗口已创建 title=%q", cfg.Title)

	var ops op.Ops
	cur := (*image.NRGBA)(nil)
	frames := 0
	start := time.Now()
	ready := false
	closing := false
	status := ""

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-stop:
			w.Perform(system.ActionClose)
			return nil
		default:
		}

		e := w.Event()
		switch e := e.(type) {
		case app.DestroyEvent:
			log.Printf("viewer: DestroyEvent err=%v frames=%d", e.Err, frames)
			return e.Err
		case app.FrameEvent:
			gtx := app.NewContext(&ops, e)

			// 非阻塞取最新一帧；取到新的就把上一张还回去复用
			select {
			case img := <-cfg.Frames:
				if img != nil {
					if cur != nil && cfg.Recycle != nil {
						select {
						case cfg.Recycle <- cur:
						default:
						}
					}
					cur = img
				}
			default:
			}

			paint.ColorOp{Color: colBG}.Add(gtx.Ops)
			paint.PaintOp{}.Add(gtx.Ops)

			layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					ins := layout.UniformInset(unit.Dp(10))
					return ins.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						if cur == nil || cur.Bounds().Empty() {
							l := material.Body1(th, "等待画面…")
							l.Color = colDim
							return l.Layout(gtx)
						}
						// contain：保持宽高比，绝不拉伸（PLAN 层 1）
						dims := widget.Image{
							Src:      paint.NewImageOp(cur),
							Fit:      widget.Contain,
							Position: layout.Center,
							Scale:    1 / gtx.Metric.PxPerDp,
						}.Layout(gtx)
						// 只在"尺寸算成 0"这种真异常时报警：正常路径不该有日志噪音。
						// 画面不显示时，这行能立刻区分"布局算成 0"与"画了但被盖住"。
						if dims.Size.X == 0 || dims.Size.Y == 0 {
							log.Printf("viewer: 警告 图像布局尺寸为 0，画面不会显示 图像=%v 约束=%v",
								cur.Bounds().Size(), gtx.Constraints.Max)
						}
						return layout.Dimensions{Size: gtx.Constraints.Max}
					})
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					if cfg.Status != nil {
						if s := cfg.Status(); s != "" {
							status = s
						}
					}
					return layoutStatusBar2(gtx, th, status)
				}),
			)
			e.Frame(gtx.Ops)
			frames++
			atomicStoreFrames(frames)

			if frames == 1 || frames%60 == 0 {
				log.Printf("viewer: 渲染第 %d 帧（画面 %v）", frames, cur != nil)
			}
			if !ready && frames == 10 {
				ready = true
				LogWindowVisible(cfg.Title)
				if cfg.OnReady != nil {
					cfg.OnReady()
				}
			}
			if cfg.ExitAfter > 0 && time.Since(start) > cfg.ExitAfter && !closing {
				closing = true
				if cfg.Status != nil {
					log.Printf("viewer: 结束时统计 %s", cfg.Status())
				}
				log.Printf("viewer: ExitAfter 到时，关闭（共渲染 %d 帧）", frames)
				w.Perform(system.ActionClose)
				// 兜底：Windows 上关窗后 Gio 的消息循环不一定自行结束，
				// 进程会留下一个无意义的异常退出码（实测 0xCFFFFFFF）。
				go func() {
					time.Sleep(2 * time.Second)
					log.Printf("viewer: 关窗后未自行退出，兜底结束")
					os.Exit(0)
				}()
			}
			// 有新帧时请求下一轮，保证画面流畅
			w.Invalidate()
		}
	}
}

// layoutStatusBar2 画底部状态栏。
//
// ⚠️⚠️ 这里必须把背景的填充裁剪到状态栏自己的高度。
// Gio 的 paint.PaintOp 填充的是"当前裁剪区域"，而 layout.Rigid 不会帮你设裁剪 ——
// 少一个 clip，面板色就会铺满整个窗口，把上面刚画好的视频画面整块盖掉。
// 表现就是"画面全黑，只有状态栏文字看得见"，极难往 PaintOp 上怀疑。
// 实测踩过一次：阶段 1 的 layoutStatusBar 有这个 clip，阶段 2 重写观看端时漏掉了。
func layoutStatusBar2(gtx layout.Context, th *material.Theme, s string) layout.Dimensions {
	// 先量内容，拿到状态栏的真实高度
	macro := op.Record(gtx.Ops)
	gtx.Constraints.Min = image.Point{}
	inner := layout.UniformInset(unit.Dp(10)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		if s == "" {
			s = "观看中"
		}
		l := material.Body1(th, s)
		l.Color = colDim
		return l.Layout(gtx)
	})
	call := macro.Stop()

	// 背景只填这一条：高度用刚量出来的 inner.Size.Y，绝不用 gtx.Constraints
	// （Rigid 里的约束高度是无界的，拿它当裁剪会把画面一起盖掉）
	area := clip.Rect{Max: image.Pt(gtx.Constraints.Max.X, inner.Size.Y)}
	st := area.Push(gtx.Ops)
	paint.ColorOp{Color: colPanel}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	st.Pop()

	call.Add(gtx.Ops)
	return inner
}

// 渲染帧数用原子量记录，供看门狗在硬退出时打印（它拿不到事件循环里的局部变量）。
var (
	frameCountMu sync.Mutex
	frameCount   int
)

func atomicStoreFrames(n int) {
	frameCountMu.Lock()
	frameCount = n
	frameCountMu.Unlock()
}

func atomicFrames() int {
	frameCountMu.Lock()
	defer frameCountMu.Unlock()
	return frameCount
}

// MoveWindowTo 用 Win32 把窗口挪到指定屏幕坐标。
// 演示时把观看窗放到副屏，才能避免采集到自己的窗口（P0-1 自摄入）。
func MoveWindowTo(title string, x, y int) bool { return moveWindow(title, x, y) }

var _ = fmt.Sprintf
