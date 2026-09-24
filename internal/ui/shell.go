// 本文件实现阶段 4 的应用外壳：单窗口 + 路由状态机。
//
// 为什么是单窗口：
// Gio 的 app.Main() 是全局事件循环，同一进程里开第二个 app.Window 需要再起一套
// 循环，实测风险高（关窗顺序、焦点、事件分发都容易出问题）。把首页/设置/分享中/
// 接入/观看/框选做成同一个窗口的不同路由，行为最可预期。
//
// 职责边界：
//   - 这里只管画和收集点击，业务（采集、编码、信令）全在 cmd/goshare 里；
//   - 业务侧通过 SetShareState / SetJoinState 把状态推过来，界面每帧读快照。
package ui

import (
	"context"
	"image"
	"image/color"
	"log"
	"os"
	"sync"
	"sync/atomic"
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

// Route 是界面路由。
type Route int

const (
	RouteHome Route = iota
	RouteSetup
	RouteSharing
	RouteJoin
	RouteViewing
	RouteRegion
)

// ViewerRow 是观众列表的一行。
type ViewerRow struct {
	Name string
	IP   string
	// Since 是加入时刻，界面显示"已观看 xx:xx"。
	Since time.Time
}

// FoundRow 是局域网发现列表的一行。
type FoundRow struct {
	Name string
	Addr string
}

// ShareState 是分享端的界面可见状态（业务侧写入，界面只读）。
type ShareState struct {
	Active     bool
	Code       string
	Addrs      []string
	Port       int
	PresetName string
	Paused     bool
	Viewers    []ViewerRow
	HUD        string
	// RegionText 描述当前共享区域，如「整屏 1920×1080」。
	RegionText string
	// SrcText 描述采集源。
	SrcText string
}

// JoinState 是观看端的界面可见状态。
type JoinState struct {
	Dialing  bool
	Err      string
	Status   string
	Found    []FoundRow
	Scanning bool
	// Notice 是**叠在画面上**的醒目提示（如"对方已停止分享"）。
	// 空表示不显示。
	//
	// 为什么要有它：断流时画面会**冻结在最后一帧**（lastFrame 不会被清），
	// 用户看到的是"一张静止的图"，分不清是对方停了、自己卡了、
	// 还是画面本来就不动。光看 fps 归零没有任何提示。
	Notice string
	// NoticeWarn 为 true 时用警告色（断开），false 用常规色（暂停等可恢复情况）。
	NoticeWarn bool
}

// ShellConfig 是应用外壳的构建参数。回调全部可选（nil 表示无动作）。
type ShellConfig struct {
	Title    string
	Displays []capture.Display
	Presets  []string

	// OnStartShare 开始分享。opts 里是用户在设置页选定的内容。
	OnStartShare func(opts ShareOptions) error
	// OnStopShare 停止分享。
	OnStopShare func()
	// OnPause 暂停/恢复推送画面。
	OnPause func(on bool)
	// OnPreset 切换质量档位（传档位名）。
	OnPreset func(name string)
	// OnKick 踢掉某位观众（按 IP）。
	OnKick func(ip string)
	// OnRotate 轮换授权码，返回新码。
	OnRotate func() string
	// OnPickRegion 请求抓取一张该显示器的快照供框选用。
	OnPickRegion func(d capture.Display) *image.NRGBA
	// OnRegionDone 框选完成（ok=false 表示用户取消）。
	OnRegionDone func(rect capture.Rect, ok bool)
	// OnJoin 请求接入某个分享。异步执行，结果用 SetJoinState 回传。
	OnJoin func(addr, code string)
	// OnScan 请求扫描局域网。同上。
	OnScan func()
	// OnLeaveView 退出观看。
	OnLeaveView func()

	// ExitAfter 大于 0 时到时自动退出（自动化验证用）。
	ExitAfter time.Duration
}

// ShareOptions 是用户在设置页选定的分享参数。
type ShareOptions struct {
	Display capture.Display
	Region  capture.Rect // 零值表示整屏
	Preset  string
}

// Shell 是应用外壳。
type Shell struct {
	cfg ShellConfig

	mu     sync.Mutex
	share  ShareState
	join   JoinState
	route  Route
	// frames 是观看端画面；preview 是分享端本地回显。两者都是"最新帧覆盖式"。
	frames  chan *image.NRGBA
	preview *image.NRGBA
	// snapshot 是框选用的桌面快照。
	snapshot *image.NRGBA
	// toast 是短暂提示（复制成功等）。
	toast     string
	toastAt   time.Time
	setupNote string

	// 页面控件状态（必须在帧之间保持）
	btnShare    widget.Clickable
	btnWatch    widget.Clickable
	btnBack     widget.Clickable
	btnStart    widget.Clickable
	btnStop     widget.Clickable
	btnPause    widget.Clickable
	btnPick     widget.Clickable
	btnReRegion widget.Clickable
	btnRegionOK widget.Clickable
	btnRegionNo widget.Clickable
	btnJoin     widget.Clickable
	btnScan     widget.Clickable
	btnLeave    widget.Clickable
	btnCodeCopy widget.Clickable
	btnRotate   widget.Clickable
	btnZoomFit  widget.Clickable
	btnZoom100  widget.Clickable
	addrCopies  []widget.Clickable
	kickBtns    []widget.Clickable
	foundBtns   []widget.Clickable
	presetBtns  []widget.Clickable
	dispBtns    []widget.Clickable

	dispEnum  widget.Enum
	presetEnm widget.Enum
	// panelList 让分享中面板的"信息区"可滚动：观众一多面板就超高，
	// 不滚动的话底部的"停止分享"会被推出窗口（实测 780 高窗口 + 2 个观众就放不下）。
	panelList widget.List

	edAddr widget.Editor
	edCode widget.Editor

	// 框选拖拽
	dragTag  bool // 作为 event.Tag，需要取地址
	dragging bool
	dragFrom image.Point
	dragTo   image.Point
	region   capture.Rect
	// regionReturn 记录框选完成（或取消）后回到哪个页面：
	// 设置页进来回设置页，分享中"换区域"进来回分享页。
	regionReturn Route

	// 观看端缩放（P0 #13）：
	// zoom100=false 适配窗口（contain，可能缩小变糊）；
	// zoom100=true  1:1 原始像素（清晰，画面比窗口大时可拖动平移）。
	zoom100  bool
	panTag   bool // 平移拖拽的 event.Tag
	panning  bool
	panFrom  image.Point // 按下时的指针位置（屏幕 px）
	panBaseX int         // 按下时已有的平移量
	panBaseY int
	panX     int // 相对"居中位置"的平移量（屏幕 px）
	panY     int

	// 内部
	selDisplay capture.Display
	selPreset  string
	lastFrame  *image.NRGBA
	frameStamp uint64
	viewW      int
	viewH      int
	// pendingFrames 缓存最近一帧，避免预览闪黑
}

// NewShell 创建外壳。displays 为空时调用方应先报错。
func NewShell(cfg ShellConfig) *Shell {
	s := &Shell{
		cfg:          cfg,
		frames:       make(chan *image.NRGBA, 2),
		route:        RouteHome,
		preview:      nil,
		regionReturn: RouteSetup,
	}
	if len(cfg.Displays) > 0 {
		s.selDisplay = cfg.Displays[0]
		for _, d := range cfg.Displays {
			if d.Primary {
				s.selDisplay = d
				break
			}
		}
	}
	if len(cfg.Presets) > 0 {
		s.selPreset = cfg.Presets[0]
	}
	s.edAddr.SingleLine = true
	s.edCode.SingleLine = true
	s.edAddr.SetText("")
	s.edCode.SetText("")
	// 框选页的显示器切换按钮（数量固定 = 显示器数量）。
	s.dispBtns = make([]widget.Clickable, len(cfg.Displays))
	s.panelList.Axis = layout.Vertical
	return s
}

// Route 返回当前路由。
func (s *Shell) Route() Route {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.route
}

// Go 切换路由。
func (s *Shell) Go(r Route) {
	s.mu.Lock()
	s.route = r
	s.mu.Unlock()
}

// SetShareState 更新分享端状态。
//
// 同时把按钮切片扩到够用：列表行数是动态的，按钮控件必须一一对应，
// 否则新出现的那几行会没有按钮（实测踩过：地址复制按钮整列消失）。
func (s *Shell) SetShareState(st ShareState) {
	s.mu.Lock()
	s.share = st
	for len(s.addrCopies) < len(st.Addrs) {
		s.addrCopies = append(s.addrCopies, widget.Clickable{})
	}
	for len(s.kickBtns) < len(st.Viewers) {
		s.kickBtns = append(s.kickBtns, widget.Clickable{})
	}
	s.mu.Unlock()
}

// SetJoinState 更新观看端状态。
func (s *Shell) SetJoinState(st JoinState) {
	s.mu.Lock()
	s.join = st
	for len(s.foundBtns) < len(st.Found) {
		s.foundBtns = append(s.foundBtns, widget.Clickable{})
	}
	s.mu.Unlock()
}

// PushFrame 推送一帧观看画面（覆盖式，只保留最新）。
func (s *Shell) PushFrame(img *image.NRGBA) {
	select {
	case s.frames <- img:
	default:
		// 渲染跟不上时丢旧帧 —— 观看端只看最新，不需要积压
		<-s.frames
		s.frames <- img
	}
}

// PushPreview 更新分享端本地回显。
func (s *Shell) PushPreview(img *image.NRGBA) {
	s.mu.Lock()
	s.preview = img
	s.mu.Unlock()
}

// SetSnapshot 设置框选用的桌面快照。
func (s *Shell) SetSnapshot(img *image.NRGBA) {
	s.mu.Lock()
	s.snapshot = img
	s.mu.Unlock()
}

// SetLastFrame 直接设置观看画面。正常路径走 PushFrame 的通道，
// 这个入口是给离屏渲染校验用的（那边没有帧事件来消费通道）。
func (s *Shell) SetLastFrame(img *image.NRGBA) {
	s.mu.Lock()
	s.lastFrame = img
	s.mu.Unlock()
}

// LastFrame 返回当前观看画面（校验用）。
func (s *Shell) LastFrame() *image.NRGBA {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastFrame
}

// SetRegionRect 直接设置框选结果（校验用）。
func (s *Shell) SetRegionRect(r capture.Rect) {
	s.mu.Lock()
	s.region = r
	s.mu.Unlock()
}

// SetDragPreview 直接设置拖拽框（窗口坐标，校验用）。
func (s *Shell) SetDragPreview(from, to image.Point) {
	s.dragFrom = from
	s.dragTo = to
}

// SetZoom100 直接设置观看端缩放模式（离屏校验用，界面里走按钮）。
func (s *Shell) SetZoom100(on bool) {
	s.zoom100 = on
	s.panX, s.panY = 0, 0
}

// SetSetupNote 设置设置页提示（如"已选择区域 800×600"）。
func (s *Shell) SetSetupNote(note string) {
	s.mu.Lock()
	s.setupNote = note
	s.mu.Unlock()
}

// Toast 显示一条短暂提示。
func (s *Shell) Toast(msg string) {
	s.mu.Lock()
	s.toast = msg
	s.toastAt = time.Now()
	s.mu.Unlock()
}

// Snapshot 返回框选结果（业务侧在 OnRegionDone 里取）。
func (s *Shell) Region() capture.Rect { return s.region }

// SelectedDisplay 返回设置页选中的显示器。
func (s *Shell) SelectedDisplay() capture.Display { return s.selDisplay }

// Run 运行窗口事件循环，阻塞到窗口关闭。
func (s *Shell) Run(ctx context.Context) error {
	w := new(app.Window)
	w.Option(
		app.Title(s.cfg.Title),
		app.Size(unit.Dp(1180), unit.Dp(780)),
		app.MinSize(unit.Dp(720), unit.Dp(520)),
	)

	th := material.NewTheme()
	th.Shaper = text.NewShaper(text.WithCollection(loadFontFaces()))
	th.Palette.Fg = colText
	th.Palette.Bg = colBG

	var ops op.Ops
	start := time.Now()
	// frames 与 closing 是**跨 goroutine** 的（渲染循环 + 独立定时器），
	// 必须原子访问。见下面 ExitAfter 的注释。
	var frames atomic.Int64
	var closing atomic.Bool
	// closeNow 是唯一的关窗路径：定时器与渲染循环都走它，CAS 保证只关一次。
	closeNow := func(reason string) {
		if !closing.CompareAndSwap(false, true) {
			return
		}
		log.Printf("shell: %s，关闭窗口（共渲染 %d 帧）", reason, frames.Load())
		go func() {
			time.Sleep(2 * time.Second)
			log.Printf("shell: 关窗后未自行退出，兜底结束")
			os.Exit(0)
		}()
		w.Perform(system.ActionClose)
	}
	if s.cfg.ExitAfter > 0 {
		// ⚠️ 定时器**必须独立于渲染帧**。
		//
		// 原来的写法是在 FrameEvent 里检查 time.Since(start) > ExitAfter —— 但
		// 防自摄入会把主窗最小化（见 internal/ui/winmin_windows.go），最小化后
		// Gio 不再产生 FrameEvent，检查被饿死，实测 `-exit 22s` 实际跑了 83 秒
		// 才退出（日志："共渲染 6 帧"）。危害不只是验证不准：进程残留会一直占着
		// 信令端口、持续采集编码、吃 1GB 内存，而它看起来"什么都没做"。
		time.AfterFunc(s.cfg.ExitAfter, func() { closeNow("ExitAfter 到时") })
	}
	wasRegion := false

	for {
		e := w.Event()
		switch e := e.(type) {
		case app.DestroyEvent:
			return e.Err
		case app.FrameEvent:
			gtx := app.NewContext(&ops, e)

			// 观看端：只保留最新帧
			select {
			case f := <-s.frames:
				if f != nil {
					s.mu.Lock()
					s.lastFrame = f
					s.viewW, s.viewH = f.Bounds().Dx(), f.Bounds().Dy()
					s.mu.Unlock()
				}
			default:
			}

			// 进出框选要切换窗口尺寸（框选需要最大化才能覆盖到整个桌面快照）
			curRegion := s.Route() == RouteRegion
			if curRegion != wasRegion {
				wasRegion = curRegion
				if curRegion {
					w.Option(app.Maximized.Option())
				} else {
					w.Option(app.Size(unit.Dp(1180), unit.Dp(780)))
				}
			}

			s.Layout(gtx, th)
			e.Frame(gtx.Ops)
			frames.Add(1)

			// 兜底：渲染循环也检查一次（窗口可见时比定时器更早响应，
			// 且能覆盖定时器被系统时钟大幅跳变影响的极端情况）。
			if s.cfg.ExitAfter > 0 && time.Since(start) > s.cfg.ExitAfter {
				closeNow("ExitAfter 到时")
			}
			w.Invalidate()
		}
	}
}

// NewTheme 构造带中文字体的深色主题。
//
// 导出是为了让离屏渲染校验（cmd/uicheck）能复用同一套主题 —— 校验时用的
// 必须是生产用的那份外观，否则"验过了"和"用户看到的"是两回事。
func NewTheme() *material.Theme {
	th := material.NewTheme()
	th.Shaper = text.NewShaper(text.WithCollection(loadFontFaces()))
	th.Palette.Fg = colText
	th.Palette.Bg = colBG
	return th
}

// Layout 按当前路由绘制。
//
// 导出以便离屏渲染做像素级校验（cmd/uicheck）：锁屏或无人值守时截屏渠道
// 拿不到有效像素，但离屏渲染照常可用。
func (s *Shell) Layout(gtx layout.Context, th *material.Theme) {
	// 背景
	paint.Fill(gtx.Ops, colBG)

	// 先处理控件事件（点击会改变路由），再按新路由画
	s.handleEvents(gtx)

	s.mu.Lock()
	r := s.route
	s.mu.Unlock()

	switch r {
	case RouteSetup:
		s.pageSetup(gtx, th)
	case RouteSharing:
		s.pageSharing(gtx, th)
	case RouteJoin:
		s.pageJoin(gtx, th)
	case RouteViewing:
		s.pageViewing(gtx, th)
	case RouteRegion:
		s.pageRegion(gtx, th)
	default:
		s.pageHome(gtx, th)
	}
	s.drawToast(gtx, th)
}

// drawToast 画短暂提示（右下角小条）。
func (s *Shell) drawToast(gtx layout.Context, th *material.Theme) {
	s.mu.Lock()
	msg, at := s.toast, s.toastAt
	s.mu.Unlock()
	if msg == "" || time.Since(at) > 1800*time.Millisecond {
		return
	}
	layout.SE.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.UniformInset(unit.Dp(14)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return s.chip(gtx, th, msg, colGood)
		})
	})
}

// chip 是一小块带底色的文字（提示/标签用）。
func (s *Shell) chip(gtx layout.Context, th *material.Theme, txt string, bg color.NRGBA) layout.Dimensions {
	m := op.Record(gtx.Ops)
	gtx.Constraints.Min = image.Point{}
	d := layout.UniformInset(unit.Dp(8)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		l := material.Body2(th, txt)
		l.Color = colText
		return l.Layout(gtx)
	})
	c := m.Stop()
	paint.FillShape(gtx.Ops, bg, clip.UniformRRect(image.Rectangle{Max: d.Size}, 6).Op(gtx.Ops))
	c.Add(gtx.Ops)
	return d
}
