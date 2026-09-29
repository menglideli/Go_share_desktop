// 本文件实现分享中的置顶悬浮条（HUD）。
//
// 为什么需要它：分享一开始主窗就会最小化（防自摄入），用户切去演示别的
// 窗口后，"谁在看我 / 怎么停止"就完全看不见了 —— 只能去托盘翻菜单。
// 悬浮条把"状态 + 人数 + 停止"钉在屏幕上，和主流会议软件一致。
//
// 架构注意（都是踩坑区）：
//   - 它是进程内**第二个** Gio 窗口，自己跑一套事件循环 goroutine（Gio 官方
//     支持多窗口，但每个窗口的 Event() 循环必须在固定 goroutine 里跑）。
//     主壳当年选单窗口是怕关窗顺序/焦点问题，HUD 生命周期简单（随分享
//     开始/结束），风险可控。
//   - 置顶只能走 Win32 SetWindowPos(HWND_TOPMOST)，且必须 SWP_NOMOVE ——
//     外部**移动** Gio 窗口会让它不再产生帧事件（实测踩过，见 PLAN.md
//     约束 5）。
//   - 初始位置由我们用同一次 SetWindowPos 摆好（SWP_NOSIZE，只挪位置并置顶）：
//     系统给的 CW_USEDEFAULT 级联位置每次分享都不一样。这条路径**确实碰了
//     位置**，所以位置只摆一次（首帧后），且由 placeAndVerifyHUD 复核
//     "摆完还能不能刷新"（见该函数注释）。
//   - 拖动用 Gio 官方的 system.ActionMove（无边框窗口的标准做法），
//     拖到哪里会被记住，下次分享复用。
//   - HUD 本身会被采集进分享画面（本机 Win10 LTSC 没有
//     WDA_EXCLUDEFROMCAPTURE）。这是可接受的：会议软件的悬浮条也会出现在
//     共享画面里；且它只在状态变化时重绘，不会触发持续全量帧。
package ui

import (
	"image"
	"image/color"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"gioui.org/app"
	"gioui.org/io/event"
	"gioui.org/io/pointer"
	"gioui.org/io/system"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

// hudWindowTitle 是悬浮条窗口的标题，也是"认主窗时必须排除"的判据。
//
// 后一个用途是本次事故的教训：防自摄入靠"找出本进程主窗再最小化"，
// 而悬浮条是本进程的第二个顶层窗，标题不排除的话它会被当成主窗 ——
// 于是被最小化、被停靠到 (-32000,-32000)，用户再也看不到它，
// 而日志还打着"主窗已最小化"（见 winmin_windows.go 的 ownMainWindow）。
const hudWindowTitle = "goshare-hud"

// HUDWindowTitle 供 cmd/goshare 创建窗口时复用，避免两处各写一份字符串。
const HUDWindowTitle = hudWindowTitle

// colBad 是停止/危险色。只在 HUD 用，放这里不污染主配色表。
var colBad = color.NRGBA{R: 224, G: 82, B: 82, A: 255}

// hudLastPos 记住用户把悬浮条拖到过哪里（内存级，不写盘），下次分享复用。
// 没有它时每次开始分享都落在系统的级联位置，每次都不一样。
var hudLastPos atomic.Pointer[image.Point]

// HUDConfig 是悬浮条的构建参数。
type HUDConfig struct {
	// Title 是窗口标题，Win32 置顶按它找窗口，必须全局唯一。
	Title string
	// OnStop 是点「停止」的回调，在 HUD 窗口 goroutine 里触发 ——
	// 调用方要自己做线程切换（通常是发消息给主流程）。
	OnStop func()
}

// HUD 是分享中的置顶悬浮条。所有方法都可在任意 goroutine 调用。
type HUD struct {
	cfg HUDConfig

	mu      sync.Mutex
	viewers int
	quality string // 质量显示，如 "最大" 或 "自适应·q59"；空表示不显示
	paused  bool

	// win 是窗口指针，run goroutine 装填；Stop 跨 goroutine 调用，用原子。
	win atomic.Pointer[app.Window]
	// running 标记事件循环是否活着（Start 幂等用）。
	running atomic.Bool
	// wantClose 标记"窗口还没建出来就被要求关闭"：
	// Stop 赶在 run 装填 win 之前调用时，靠它在首帧补救，避免窗口漏关。
	wantClose atomic.Bool
	done      chan struct{} // 事件循环退出时关闭
	stopOne   sync.Once

	btnStop widget.Clickable
	dragTag bool // 拖拽移动的 event.Tag，需要取地址
}

// NewHUD 创建悬浮条（尚未显示）。
func NewHUD(cfg HUDConfig) *HUD {
	return &HUD{cfg: cfg, done: make(chan struct{})}
}

// Start 显示悬浮条（幂等：已显示时直接返回）。
func (h *HUD) Start() {
	if !h.running.CompareAndSwap(false, true) {
		return
	}
	// 上一轮的 Once 已经用掉了：不重置的话第二次 Stop 会静默失效，
	// 悬浮条就再也关不掉（停分享后它还钉在屏幕上）。
	h.stopOne = sync.Once{}
	h.wantClose.Store(false)
	h.done = make(chan struct{})
	go h.run()
}

// Stop 关闭悬浮条（幂等）。事件循环退出后才可以再次 Start。
func (h *HUD) Stop() {
	h.stopOne.Do(func() {
		h.wantClose.Store(true)
		// 关窗之前先记下用户把它拖到了哪里：窗口一销毁就再也读不到位置了。
		// 这是在非 Gio 事件循环的 goroutine 里调用，Win32 查询是安全的。
		if w := h.win.Load(); w != nil {
			rememberHUDPos(h.cfg.Title)
			w.Perform(system.ActionClose)
		}
	})
}

// Wait 等事件循环退出（测试/清理用）。
func (h *HUD) Wait() { <-h.done }

// SetState 更新显示内容并触发重绘。
// Gio 的 Invalidate 官方允许跨 goroutine 调用。
//
// 内容没变就不重绘：HUD 会被采集进分享画面，每次无谓重绘都会
// 让那一小块 dirty tile 跟着变，白烧带宽。
func (h *HUD) SetState(viewers int, quality string, paused bool) {
	h.mu.Lock()
	if h.viewers == viewers && h.quality == quality && h.paused == paused {
		h.mu.Unlock()
		return
	}
	h.viewers, h.quality, h.paused = viewers, quality, paused
	h.mu.Unlock()
	// 内容真的变了才记一笔：这是"悬浮条到底有没有在刷新"的唯一日志线索
	// （本轮排查里，屏幕上像素不变时无法区分"状态没变"与"根本没重绘"）。
	log.Printf("hud: 内容更新 观众=%d 质量=%q 暂停=%v", viewers, quality, paused)
	if w := h.win.Load(); w != nil {
		w.Invalidate()
	}
}

// run 是悬浮条窗口的事件循环（Gio 要求一个窗口一套固定 goroutine）。
func (h *HUD) run() {
	defer h.running.Store(false)
	defer close(h.done)

	w := new(app.Window)
	h.win.Store(w)
	w.Option(
		app.Title(h.cfg.Title),
		app.Size(unit.Dp(320), unit.Dp(46)),
		app.Decorated(false),
	)
	// Stop 可能抢在窗口建出来之前就到了：补关。
	if h.wantClose.Load() {
		w.Perform(system.ActionClose)
	}

	th := NewTheme()
	var ops op.Ops
	// frameN 必须跨 goroutine 读：下面的自检用独立定时器，不能靠渲染帧驱动
	// （最小化/遮挡时 Gio 不再产生 FrameEvent，用帧计数当时间轴会被彻底饿死 —— R22）。
	var frameN atomic.Int64

	for {
		e := w.Event()
		switch e := e.(type) {
		case app.DestroyEvent:
			return
		case app.FrameEvent:
			gtx := app.NewContext(&ops, e)
			n := frameN.Add(1)
			if n == 1 {
				// 首帧报一次真实尺寸与缩放：这是唯一能回答"悬浮条到底多大"的地方
				// （尺寸由 Gio 按 DPI 换算出来，日志里没有它就只能靠猜）。
				log.Printf("hud: 首帧 尺寸=%v PxPerDp=%.3f（尺寸即物理像素）", e.Size, gtx.Metric.PxPerDp)
				placeHUD(h.cfg.Title, e.Size, w, &frameN)
			}
			h.handleEvents(gtx)
			h.Layout(gtx, th)
			e.Frame(gtx.Ops)
		}
	}
}

// placeHUD 在窗口出现后做三件事，全部走**独立 goroutine / 独立定时器**：
//
//  1. 摆放 + 置顶（Win32 不能在 FrameEvent 处理里直接调，见下）；
//  2. 断言"它真的显示出来了"（可见 / 未最小化 / 置顶）—— 「渲染了帧」不等于
//     「窗口真的显示出来了」，本轮事故就是日志全绿而窗口被停靠在 -32000；
//  3. 自检"挪过位置之后还能不能刷新"（PLAN 约束 5 的风险点）。
//
// ⚠️ 第 2 步**不能**等第 10 帧：悬浮条只在内容变化时重绘（SetState → Invalidate），
// 没有观众时可能永远到不了 10 帧 —— 实测踩过（等了整场只渲染 1 帧）。
// 所以时序一律用独立定时器。
func placeHUD(title string, size image.Point, w *app.Window, frameN *atomic.Int64) {
	go placeAndVerifyHUD(title, size)

	// 第 2 步：窗口出来 2 秒后断言状态。
	time.AfterFunc(2*time.Second, func() {
		if !LogWindowState(title) {
			log.Printf("hud: ⚠ 悬浮条状态异常 —— 用户可能看不到它（详见上一行 wincheck 输出）")
		}
	})

	// 第 3 步：摆放完成后再过 2 秒，主动请求一帧，看帧事件还会不会来。
	time.AfterFunc(2*time.Second, func() {
		before := frameN.Load()
		w.Invalidate()
		time.AfterFunc(1500*time.Millisecond, func() {
			if frameN.Load() <= before {
				log.Printf("hud: ⚠ 摆放后帧事件不再产生（PLAN 约束 5）—— 悬浮条会停在旧画面，位置改动需要回退")
			} else {
				log.Printf("hud: 摆放后帧事件正常（+%d 帧）", frameN.Load()-before)
			}
		})
	})
}

// handleEvents 处理点击与拖拽。
func (h *HUD) handleEvents(gtx layout.Context) {
	if h.btnStop.Clicked(gtx) {
		// 必须留痕：悬浮条是置顶的，用户点别的东西时可能正好点在它上面 ——
		// 本轮验证时分享被莫名停掉过一次，只有这条日志能区分"真被点了"
		// 与"程序自己走了停止路径"。
		log.Printf("hud: 「停止」被点击")
		if h.cfg.OnStop != nil {
			h.cfg.OnStop()
		}
	}
	// 按住条体拖动 = 移动窗口（Gio 官方做法，等价于系统标题栏拖动）。
	for {
		ev, ok := gtx.Event(pointer.Filter{Target: &h.dragTag, Kinds: pointer.Press})
		if !ok {
			break
		}
		if e, ok := ev.(pointer.Event); ok && e.Kind == pointer.Press {
			if w := h.win.Load(); w != nil {
				w.Perform(system.ActionMove)
			}
		}
	}
}

// Layout 绘制悬浮条内容。
//
// 导出以便离屏渲染校验（cmd/uicheck）：HUD 不进主壳路由，
// 但布局函数本身可以脱离窗口独立渲染。
func (h *HUD) Layout(gtx layout.Context, th *material.Theme) layout.Dimensions {
	h.mu.Lock()
	viewers, quality, paused := h.viewers, h.quality, h.paused
	h.mu.Unlock()

	dot := colGood
	status := "正在分享"
	if paused {
		dot = colWarn
		status = "已暂停"
	}
	text := status + " · " + itoa(viewers) + " 人在看"
	if quality != "" {
		text += " · " + quality
	}

	return layout.Stack{}.Layout(gtx,
		// 底层：圆角背景 + 整面拖拽区（按钮区域后画，点击优先级更高）
		layout.Expanded(func(gtx layout.Context) layout.Dimensions {
			defer clip.UniformRRect(image.Rectangle{Max: gtx.Constraints.Min}, 10).Push(gtx.Ops).Pop()
			paint.Fill(gtx.Ops, color.NRGBA{R: 22, G: 28, B: 34, A: 242})
			return layout.Dimensions{Size: gtx.Constraints.Min}
		}),
		layout.Expanded(func(gtx layout.Context) layout.Dimensions {
			// 整面注册拖拽 Tag。按钮在更上层后注册，点击优先级更高。
			area := clip.Rect(image.Rectangle{Max: gtx.Constraints.Min}).Push(gtx.Ops)
			event.Op(gtx.Ops, &h.dragTag)
			area.Pop()
			return layout.Dimensions{Size: gtx.Constraints.Min}
		}),
		// 上层：内容行
		layout.Stacked(func(gtx layout.Context) layout.Dimensions {
			return layout.UniformInset(unit.Dp(10)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				// ⚠️ 必须把 Min 撑到 Max：layout.Stack 会把子项的 Min 约束清零，
				// 而 Flex 分配 SpaceBetween 的富余空间用的是 mainMin（= Min 约束），
				// Min=0 时富余恒为 0 —— 表现就是「停止」按钮紧跟在文字后面、
				// 条体右侧留下一大片空白（默认 SpaceBetween 形同失效）。
				gtx.Constraints.Min = gtx.Constraints.Max
				return layout.Flex{Alignment: layout.Middle, Spacing: layout.SpaceBetween}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								sz := gtx.Dp(10)
								paint.FillShape(gtx.Ops, dot, clip.Ellipse(image.Rectangle{Max: image.Pt(sz, sz)}).Op(gtx.Ops))
								return layout.Dimensions{Size: image.Pt(sz, sz)}
							}),
							layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								l := material.Body2(th, text)
								l.Color = colText
								return l.Layout(gtx)
							}),
						)
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						b := material.Button(th, &h.btnStop, "停止")
						b.Background = colBad
						b.Color = colText
						b.Inset = layout.UniformInset(unit.Dp(6))
						return b.Layout(gtx)
					}),
				)
			})
		}),
	)
}

// itoa 避免为此单点引入 strconv 的格式化开销（HUD 数字都很小）。
func itoa(n int) string {
	if n <= 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
