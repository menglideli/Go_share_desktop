// goshare — 内网局域网桌面共享客户端（阶段 4：完整界面）
//
// 启动后是两个大按钮：「我要分享」「我要观看」。
// 界面只负责画和收集点击（internal/ui），业务编排全在这个文件里。
package main

import (
	"context"
	"flag"
	"fmt"
	"image"
	"image/png"
	"log"
	"math/bits"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"

	"goshare/internal/capture"
	"goshare/internal/codec"
	"goshare/internal/discover"
	"goshare/internal/pipeline"
	"goshare/internal/rtc"
	"goshare/internal/signal"
	"goshare/internal/tray"
	"goshare/internal/ui"
)

// version 由发布构建用 -ldflags "-X main.version=x.y.z" 注入；
// 开发态（go build / go run）保持 "dev"。
var version = "dev"

// UDP 端口区间：固定后防火墙放行规则可以写成一条固定命令（见启动时打印的指引）。
const (
	udpPortMin = 50000
	udpPortMax = 50100
)

func main() {
	logPath := flag.String("log", "", "日志文件路径（默认输出到控制台）")
	exitAfter := flag.Duration("exit", 0, "自动退出时长（自动化验证用，如 20s）")
	port := flag.Int("port", 9000, "信令起始端口（被占用时顺延）")
	maxViewers := flag.Int("max", 8, "最多允许多少人同时观看")
	code := flag.String("code", "", "固定授权码（空则随机生成）")
	auto := flag.String("auto", "", "自动化模式：share / watch（供无人值守验证用）")
	autoAddr := flag.String("addr", "", "-auto watch 时的目标地址，如 192.168.1.5:9000")
	autoCode := flag.String("code-watch", "", "-auto watch 时的授权码")
	autoPreset := flag.String("preset", "最大", "-auto share 时的质量档位")
	autopause := flag.Duration("autopause", 0, "自动分享后多久暂停（验证暂停链路用，0 = 不暂停）")
	displayIdx := flag.Int("display", -1, "采集哪块显示器（-1 = 主屏）")
	// 诊断用：观看端把第 N 帧原样落盘，用来回答"观众到底收到了什么"。
	// 光看 fps/带宽/非黑占比无法区分"网络问题"和"画面本身就是暗的"。
	dumpFrame := flag.String("dump-frame", "", "调试：观看端收到第 N 帧时存 PNG（配合 -dump-at）")
	dumpAt := flag.Uint64("dump-at", 40, "-dump-frame 指定存第几帧")
	// 既是为了逐条验证恢复路径（关掉某条路，看画面还能不能自愈），
	// 也是产品上的调优旋钮。
	switchAt := flag.Duration("switch-at", 0, "分享开始 N 秒后热切换采集区域（自动化验证用，0 = 不切）")
	switchRegion := flag.String("switch-region", "", "热切换到的区域，格式 宽x高+左+上（如 800x600+100+100）")
	switchDisp := flag.Int("switch-display", -1, "分享开始 N 秒后热切换到第 N 台显示器（配合 -switch-at，-1 = 不换屏）")
	keyInterval := flag.Duration("key-interval", 0, "稳态周期性全量帧间隔（0/负值=禁用，靠观众请求恢复）")
	keyBurst := flag.Duration("key-burst", 3*time.Second, "新观众接入后的密集补帧窗口（负值禁用）")
	flag.Parse()

	if *logPath != "" {
		if f, err := os.Create(*logPath); err == nil {
			defer f.Close()
			log.SetOutput(f)
		}
	}

	// 必须在创建窗口、枚举显示器之前：否则高 DPI 下 GetCursorPos 返回逻辑像素，
	// 光标与框选坐标都会错位（阶段 1 踩过，错误还完全自洽）。
	if lvl := capture.EnsureDPIAware(); lvl != 2 {
		log.Printf("警告: DPI 感知级别为 %d（期望 2），高 DPI 下坐标可能错位", lvl)
	}

	ctx := context.Background()
	ds, err := capture.Displays(ctx)
	if err != nil || len(ds) == 0 {
		// windowsgui 构建下 stderr 不可见，必须弹窗让用户看到失败原因。
		ui.AlertError("GoShare 启动失败", fmt.Sprintf("枚举显示器失败：%v", err))
		fmt.Fprintf(os.Stderr, "枚举显示器失败: %v\n", err)
		os.Exit(1)
	}
	for _, d := range ds {
		log.Printf("显示器: %s", d.String())
	}

	names := make([]string, 0, len(pipeline.Presets))
	for _, p := range pipeline.Presets {
		names = append(names, p.Name)
	}

	a := &app{
		ctx: ctx, port: *port, maxViewers: *maxViewers, fixedCode: *code,
		dumpPath: *dumpFrame, dumpAt: *dumpAt,
		keyInterval: *keyInterval, keyBurst: *keyBurst,
		switchAt: *switchAt, switchRegion: *switchRegion,
		switchDisp: *switchDisp, displays: ds,
		disp: newDispPool(),
	}
	title := "GoShare · 内网桌面共享"
	if version != "dev" {
		title += " v" + version
	}
	a.title = title
	a.shell = ui.NewShell(ui.ShellConfig{
		Title:     title,
		Displays:  ds,
		Presets:   names,
		ExitAfter: *exitAfter,

		// 观看画面缓冲池的归还通道（见 viewbuf.go）：
		// 渲染线程换掉上一帧时把缓冲还回来复用，省掉每帧 20MB 的分配。
		Recycle: a.disp.Recycle(),

		OnStartShare: a.startShare,
		OnStopShare:  a.stopShare,
		OnPause:      a.setPaused,
		OnPreset:     a.setPreset,
		OnKick:       a.kick,
		OnRotate:     a.rotateCode,
		OnPickRegion: a.pickRegionSnapshot,
		OnRegionDone: a.regionDone,
		OnJoin:       a.join,
		OnScan:       a.scan,
		OnLeaveView:  a.leaveView,
	})

	// 托盘常驻（P0 #1 收尾）：只进交互模式。分享开始主窗最小化后，
	// 用户通过托盘恢复窗口 / 停止分享 / 退出。
	// -auto 自动化路径**不启用**：托盘要弹菜单、抢前台，会污染 e2e 时序。
	if *auto == "" {
		t, terr := tray.New(tray.Config{
			Tooltip: title,
			OnShow: func() {
				if err := ui.RestoreMainWindow(); err != nil {
					log.Printf("托盘：恢复主窗失败: %v", err)
				}
			},
			OnStop: func() {
				a.stopShare()
				// 停完把窗口带回前台，让用户看到停下来的状态。
				if err := ui.RestoreMainWindow(); err != nil {
					log.Printf("托盘：恢复主窗失败: %v", err)
				}
			},
			OnQuit: func() { a.shell.RequestClose("托盘退出") },
		})
		if terr != nil {
			// 托盘失败不致命：主窗还在任务栏，功能不受影响。
			log.Printf("托盘创建失败（主窗口仍可正常使用）: %v", terr)
		} else {
			a.tray = t
			defer t.Close()
		}
	}

	// 无人值守验证入口：跳过界面点击，直接进入对应流程。
	switch *auto {
	case "share":
		d := a.shell.SelectedDisplay()
		if *displayIdx >= 0 && *displayIdx < len(ds) {
			d = ds[*displayIdx]
		}
		if err := a.startShare(ui.ShareOptions{Display: d, Preset: *autoPreset}); err != nil {
			log.Printf("自动分享失败: %v", err)
		} else {
			a.shell.Go(ui.RouteSharing)
			if *autopause > 0 {
				go func() {
					time.Sleep(*autopause)
					log.Printf("自动暂停（-autopause %v）", *autopause)
					a.setPaused(true)
				}()
			}
		}
	case "watch":
		if *autoAddr == "" || *autoCode == "" {
			log.Printf("-auto watch 需要同时给 -addr 与 -code-watch")
		} else {
			a.join(*autoAddr, *autoCode)
		}
	}

	if err := a.shell.Run(ctx); err != nil {
		log.Printf("界面退出: %v", err)
	}
	a.stopShare()
	a.leaveView()
}

// app 持有业务侧的全部状态。
type app struct {
	ctx        context.Context
	shell      *ui.Shell
	tray       *tray.Tray // 仅交互模式非 nil
	title      string     // 窗口标题（含版本），托盘气泡复用
	port       int
	maxViewers int
	fixedCode  string

	mu sync.Mutex
	// ---- 分享端 ----
	sh      *pipeline.Sharer
	srv     *signal.Server
	ann     *discover.Announcer
	stop    context.CancelFunc
	stopHUD chan struct{}
	// hud 是分享中的置顶悬浮条（主窗最小化后屏幕上唯一的状态/停止入口）。
	hud *ui.HUD
	// shCtx 是本次分享的上下文（换屏时新建采集源要用它，保证停止分享时一起收掉）。
	shCtx  context.Context
	region capture.Rect
	// display 是当前正在采集的显示器（换区域/换屏时对照用）。
	display  capture.Display
	displays []capture.Display
	// HUD 增量统计用
	lastFrames uint64
	lastBytes  uint64
	lastSent   uint64
	lastAt     time.Time
	view       *ui.View

	// ---- 观看端 ----
	sess *signal.Session
	// 观看统计
	wFrames   uint64
	wBytes    uint64
	wStart    time.Time
	wDec      float64
	wNonBlack float64
	wLum      float64
	// wLatMS 是端到端延迟（毫秒，滑动均值）：帧里的 TS → 本地收到的时刻。
	//
	// 只有 fps 和带宽是答不了"延迟到底多少"的 —— 而这次优化的目标恰恰是延迟，
	// 没有这个指标就只能靠感觉。
	wLatMS float64
	// wChunks 是收到的**分块**数（一帧由多块组成，见 codec.Frame）。
	wChunks uint64
	// wBatches 是解码批次数（一批 = 邮箱里攒到的若干块，一起并行解）。
	// 用它算"平均每批几块"：批太小 = 并行度不够，解码会变慢。
	wBatches uint64
	// wLastSeq 是最近一次上屏的帧序号：分块按帧推进时，只有跨帧才把画面交给界面
	// （否则一帧会被推三次，白白多做两次同步）。
	wLastSeq atomic.Uint64
	// disp 是观看端画面缓冲池：复用内存 + 只同步变化过的条带（见 viewbuf.go）。
	disp *dispPool
	// wSkipped 是"解码跟不上被丢掉"的**分块**数（AsyncFrames 的单槽邮箱覆盖）。
	// 它不是网络问题，是本地解码产能问题，必须与网络丢块分开看。
	wSkipped atomic.Uint64
	// dropLogAt 给丢块日志降频（丢块可能每秒几十次，不能每次都打）。
	dropLogAt atomic.Int64
	// lastRepairAt 是最近收到"修复轮次帧"的时刻（UnixNano）：修复在飞时不必重复请求。
	lastRepairAt atomic.Int64
	// lastMissingAt / lastMissingMask 是"精确补发"的限流状态：
	// 位图变了 150ms 内不重发，没变则 500ms —— 缺 2 条带时不该每帧都敲对端。
	lastMissingAt   atomic.Int64
	lastMissingMask atomic.Uint64
	// incompleteSince 是画布开始不完整的时刻（0 = 完整）：超过 1 秒还没收敛，
	// 就从"精确补发"退化成"整轮修复"（见 reportMissing）。
	incompleteSince atomic.Int64
	// pliBackoffMs 是请求修复的退避间隔（毫秒）：300 → 600 → 1200 → 2000。
	// 固定 300ms 猛敲会在拥塞时变成"越要越堵"。
	pliBackoffMs atomic.Int64
	// wICE 是当前 ICE 连接状态（webrtc.ICEConnectionState 底层是 int）。
	//
	// 需要它是因为"收不到帧"有好几种原因：对方停止分享、自己网络断了、
	// 对方暂停了。只看 fps 归零分不清，必须结合 ICE 状态才能给出准确提示。
	wICE atomic.Int32
	// wLastFrameAt 是最后一次收到帧的时刻（UnixNano，0 表示还没收到过）。
	// 静止桌面靠 2s 心跳帧保活，所以"超过 5s 没有帧"本身就是异常信号。
	wLastFrameAt atomic.Int64

	// blackWarned 记录"全黑"告警是否已上报，避免每 500ms 刷屏。
	blackWarned bool
	// hudTicks 统计 HUD 刷新次数，用来把日志降频到每 3 秒一条。
	hudTicks int

	// dumpPath / dumpAt：诊断用，把观看端收到的第 N 帧原样落盘。
	dumpPath string
	dumpAt   uint64
	// keyInterval / keyBurst：全量帧补帧策略（见 -key-interval / -key-burst）。
	keyInterval time.Duration
	keyBurst    time.Duration
	// switchAt / switchRegion：自动化验证用，分享开始后热切换采集区域
	// （见 -switch-at / -switch-region）。
	switchAt     time.Duration
	switchRegion string
	// switchDisp：自动化验证用，分享开始后热切换到另一台显示器（-switch-display）。
	switchDisp int

	// ---- 观看端断线重连 ----
	// wAddr / wCode 是最近一次成功接入用的地址与授权码，断线自动重连用。
	wAddr string
	wCode string
	// reconnecting 防止状态循环同时发起多个重连。
	reconnecting atomic.Bool

	// ---- 观看端画面完整性（R32）----
	// needFull：当前画布不完整，需要请对端开一轮修复。初始为 true —— 观众在收到
	// 第一批分块之前，画布上什么都没有。
	needFull atomic.Bool
	// lastPLI 是上次请求修复的时刻（UnixNano），用于限流 + 退避。
	lastPLI atomic.Int64
	// peerBox 让 OnFrame 回调能拿到 peer（它要等 Dial 返回后才存在）。
	peerBox atomic.Pointer[rtc.Peer]

	// pliTarget 记录本次接入的 peer，供状态循环重试请求。
	pliTarget *rtc.Peer
}

// pliMinInterval 是请求修复的最小间隔；pliMaxBackoff 是退避上限。
//
// 不能太密：每次请求都会让编码器开一轮渐进式修复（每帧多带 1/10 条带），
// 请求过密反而互相打断，把恢复拖慢；拥塞时更是"越要越堵"。
const (
	pliMinInterval = 300 * time.Millisecond
	pliMaxBackoff  = 2 * time.Second
)

// sendPLI 请分享端开一轮**渐进式修复**（走可靠有序的 ctl 通道）。
//
// 退避是必须的：固定 300ms 猛敲，在拥塞时就是"越要越堵、越堵越黑"的正反馈。
// 每发一次退避翻倍（上限 2 秒），收到修复轮次的帧就复位。
func (a *app) sendPLI(p *rtc.Peer) {
	if p == nil {
		return
	}
	wait := time.Duration(a.pliBackoffMs.Load()) * time.Millisecond
	if wait < pliMinInterval {
		wait = pliMinInterval
	}
	now := time.Now().UnixNano()
	last := a.lastPLI.Load()
	if last != 0 && now-last < int64(wait) {
		return
	}
	if !a.lastPLI.CompareAndSwap(last, now) {
		return
	}
	// 退避翻倍（上限见 pliMaxBackoff）。
	next := a.pliBackoffMs.Load() * 2
	if next < int64(pliMinInterval/time.Millisecond) {
		next = int64(pliMinInterval / time.Millisecond)
	}
	if next > int64(pliMaxBackoff/time.Millisecond) {
		next = int64(pliMaxBackoff / time.Millisecond)
	}
	a.pliBackoffMs.Store(next)
	if err := p.SendControl(rtc.CtlPLI, nil); err != nil {
		log.Printf("请求修复失败: %v", err)
	}
}

// reportMissing 把"缺哪几条带"报给分享端（精确补发），必要时退化成整轮修复。
//
// 为什么两条路都要：精确补发一帧就能补齐、只花几十 KB，但它依赖这条 ctl 消息
// 真的送达；万一没送达（或对面是不支持的旧版本），画面就会一直缺着 ——
// 所以超过 1 秒还没收敛就退化成"整轮轮转修复"（慢一点但确定会铺满）。
func (a *app) reportMissing(miss uint64, nTiles int, f *codec.Frame) {
	now := time.Now().UnixNano()
	if a.incompleteSince.Load() == 0 {
		a.incompleteSince.Store(now)
	}
	// 全缺（刚接入 / 换了分辨率）：直接要一轮修复轮次 —— 精确补发这时等于
	// 把整屏塞进一帧，就是我们要避免的 2MB 突发。
	if nTiles > 0 && bits.OnesCount64(miss) >= nTiles {
		a.requestRepair(f)
		return
	}
	// 精确补发：150ms 限流；位图没变就放宽到 500ms（避免刷屏式重发）。
	last := a.lastMissingAt.Load()
	gap := int64(150 * time.Millisecond)
	if miss == a.lastMissingMask.Load() {
		gap = int64(500 * time.Millisecond)
	}
	if now-last >= gap && a.lastMissingAt.CompareAndSwap(last, now) {
		a.lastMissingMask.Store(miss)
		if p := a.peerBox.Load(); p != nil {
			if err := p.SendControl(rtc.CtlMissing, rtc.MissingPayload(miss)); err != nil {
				log.Printf("上报缺失条带失败: %v", err)
			}
		}
	}
	// 兜底：1 秒还没收敛 → 退化成一轮轮转修复（自带退避与"在飞不重复"判定）。
	if now-a.incompleteSince.Load() > int64(time.Second) {
		a.requestRepair(f)
	}
}

// requestRepair 在"画布不完整"时请对端开一轮修复（带退避与去重）。
func (a *app) requestRepair(f *codec.Frame) {
	now := time.Now().UnixNano()
	if f.Refresh != 0 {
		// 对端正在做修复（这一帧带修复轮次）：它会在十几帧内铺满整屏，
		// 此时再要就是白要 —— 记下时刻、把退避复位。
		a.lastRepairAt.Store(now)
		a.pliBackoffMs.Store(0)
		return
	}
	if last := a.lastRepairAt.Load(); now-last < int64(time.Second) {
		return
	}
	a.sendPLI(a.peerBox.Load())
}

// tileMask 把一块里携带的条带下标折成位图（与 codec 的 TilesMask 同一编码）。
func tileMask(f *codec.Frame) uint64 {
	var m uint64
	for _, t := range f.Tiles {
		if t.Index >= 0 && t.Index < 64 {
			m |= 1 << uint(t.Index)
		}
	}
	return m
}

// ---------------------------------------------------------------------------
// 分享端
// ---------------------------------------------------------------------------

func (a *app) startShare(opts ui.ShareOptions) error {
	a.stopShare() // 重复点击时先收干净

	name := hostName()
	shCtx, cancel := context.WithCancel(a.ctx)

	// 本地回显：分享端自己也要看到在推什么（降采样到 ~960 宽，省 CPU）
	a.mu.Lock()
	a.region = opts.Region
	a.display = opts.Display
	a.view = ui.NewView(opts.Display.W, opts.Display.H, fitScale(opts.Display.W, opts.Display.H))
	a.mu.Unlock()

	sh, err := pipeline.NewSharer(shCtx, pipeline.Config{
		Display: opts.Display,
		Region:  opts.Region,
		Preset:  pipeline.PresetByName(opts.Preset),
		Cursor:  true,
		// 全量帧补帧策略（R32）：接入窗口内密集补 + 稳态靠观众请求。
		KeyBurst:    a.keyBurst,
		KeyInterval: a.keyInterval,
		OnFrame: func(f *codec.Frame, src capture.Frame) {
			a.mu.Lock()
			v := a.view
			a.mu.Unlock()
			if v == nil {
				return
			}
			v.Update(src)
			v.Render()
			a.shell.PushPreview(v.Image())
		},
	})
	if err != nil {
		cancel()
		return err
	}

	srv, err := signal.NewServer(signal.ServerConfig{
		Name:       name,
		Port:       a.port,
		Code:       a.fixedCode,
		MaxViewers: a.maxViewers,
		Logger:     log.Default(),
	})
	if err != nil {
		_ = sh.Close()
		cancel()
		return err
	}

	srv.SetAccept(func(req signal.JoinRequest) (webrtc.SessionDescription, func(), error) {
		// cleanup 会被多条路径触发（正常断开 / ICE failed / 断线超时），必须幂等。
		// ⚠️ 不能用 sync.Once：cleanup → Server.Detach → 不会再回调，但如果哪天
		// 改成 Drop（移除+回调）就会同 goroutine 重入并永久阻塞。用 flag 更稳。
		var peer *rtc.Peer
		var cuMu sync.Mutex
		cleaned := false
		cleanup := func(reason string) {
			cuMu.Lock()
			if cleaned {
				cuMu.Unlock()
				return
			}
			cleaned = true
			cuMu.Unlock()

			// 重活必须挪出回调线程：OnState 跑在 ICE agent 的内部 goroutine 上，
			// 而 peer.Close() 要等 agent 收尾 —— 同步调用会互等死锁，
			// 整个分享端静默卡死（R27，实测踩过）。
			go func() {
				if peer != nil {
					sh.RemovePeer(peer)
					_ = peer.Close()
				}
				srv.Detach(req.Token)
				log.Printf("观众 %s 已释放（%s），剩余 %d 人", req.RemoteIP, reason, sh.PeerCount())
			}()
		}

		var err error
		peer, err = rtc.NewPeer(rtc.Config{
			UDPPortMin: udpPortMin,
			UDPPortMax: udpPortMax,
			OnState: func(s webrtc.ICEConnectionState) {
				log.Printf("观众 %s 连接状态 %s", req.RemoteIP, s)
				switch s {
				case webrtc.ICEConnectionStateFailed, webrtc.ICEConnectionStateClosed:
					cleanup("连接 " + s.String())
				case webrtc.ICEConnectionStateDisconnected:
					// 观众进程被强杀时不会有 Bye，只能靠超时兜底。
					// 不立刻回收是因为 disconnected 常只是短暂抖动。
					go func() {
						time.Sleep(5 * time.Second)
						if peer.PC().ICEConnectionState() == webrtc.ICEConnectionStateDisconnected {
							cleanup("断线超过 5s")
						}
					}()
				}
			},
			// 观众端发现画布不完整时会发 PLI（R32 / R39）。
			// 这条请求走的是可靠有序的 ctl 通道，比 media 通道可靠得多，
			// 是观众侧唯一能主动自救的手段。
			OnControl: func(t rtc.CtlType, payload []byte) {
				switch t {
				case rtc.CtlPLI:
					log.Printf("观众 %s 请求修复轮次", req.RemoteIP)
					sh.RequestRepair()
				case rtc.CtlMissing:
					// 观众报上来"缺哪几条带"：只补那几条（一帧补齐、几十 KB），
					// 比开一整轮轮转修复（十几帧、几百 KB）省一个量级。
					if mask, ok := rtc.ParseMissing(payload); ok && mask != 0 {
						sh.RequestTiles(mask)
					}
				}
			},
		})
		if err != nil {
			return webrtc.SessionDescription{}, nil, err
		}
		if err := peer.SetupMedia(); err != nil {
			_ = peer.Close()
			return webrtc.SessionDescription{}, nil, err
		}
		if err := peer.PC().SetRemoteDescription(req.Offer); err != nil {
			_ = peer.Close()
			return webrtc.SessionDescription{}, nil, err
		}
		ans, err := peer.PC().CreateAnswer(nil)
		if err != nil {
			_ = peer.Close()
			return webrtc.SessionDescription{}, nil, err
		}
		if err := peer.PC().SetLocalDescription(ans); err != nil {
			_ = peer.Close()
			return webrtc.SessionDescription{}, nil, err
		}
		// non-trickle：等候选收齐，answer 里才带得上 UDP 端口
		peer.WaitGathering(3 * time.Second)
		sh.AddPeer(peer)
		// AddPeer 里开的那轮修复是"渐进式"的：编码发生在下一帧（~33ms 后），
		// 那一刻 DataChannel 往往还没 open（DTLS 握手未完成），SendChunks 直接
		// 返回 ErrNotOpen 把这一帧丢掉；而修复轮次只有一轮，观众端于是**缺初始画面**
		// （实测 R32：开局只拿到 11.8% 的内容）。通道就绪后再补一次请求，才算稳。
		go func() {
			if err := peer.WaitReady(15 * time.Second); err != nil {
				return
			}
			sh.RequestRepair()
		}()
		log.Printf("观众 %s（%s）已接入，当前 %d 人", req.Viewer, req.RemoteIP, sh.PeerCount())
		return *peer.PC().LocalDescription(), func() { cleanup("正常断开") }, nil
	})

	if err := srv.Start(); err != nil {
		_ = sh.Close()
		cancel()
		return err
	}

	ann, _ := discover.NewAnnouncer(discover.Announce{
		V:        1,
		ID:       fmt.Sprintf("%s-%d", name, srv.Port()),
		Name:     name,
		Addrs:    srv.ShareAddrs(),
		MaxView:  a.maxViewers,
		NeedCode: true,
		Width:    opts.Display.W,
		Height:   opts.Display.H,
	}, 1500*time.Millisecond)

	a.mu.Lock()
	a.sh = sh
	a.srv = srv
	a.ann = ann
	a.stop = cancel
	a.shCtx = shCtx
	a.stopHUD = make(chan struct{})
	a.lastFrames, a.lastBytes, a.lastSent, a.lastAt = 0, 0, 0, time.Now()
	hudStop := a.stopHUD
	a.mu.Unlock()

	go func() {
		if err := sh.Run(shCtx); err != nil && shCtx.Err() == nil {
			log.Printf("分享管线结束: %v", err)
		}
	}()

	// 自动化验证用：分享开始 N 秒后热切换采集区域。
	// 走的就是界面"换区域"该走的那条路（Sharer.SetRegion → src.SetRegion +
	// ForceKeyFrame），所以它能验证的结论对界面入口同样成立。
	//
	// 为什么必须 ForceKeyFrame：区域变了，观众侧画布尺寸也变了。
	// 增量帧只覆盖"变化过的条带"，尺寸变化后旧画布上没被覆盖的区域会残留
	// 上一块区域的画面（或黑边）—— 和 R32 是同一类问题。
	if a.switchAt > 0 && a.switchRegion != "" {
		r, perr := parseRect(a.switchRegion, opts.Display)
		if perr != nil {
			log.Printf("换区域参数无法解析（%q）：%v", a.switchRegion, perr)
		} else {
			wait := a.switchAt
			go func() {
				select {
				case <-time.After(wait):
				case <-shCtx.Done():
					return
				}
				log.Printf("自动化：热切换到区域 %dx%d+%d+%d", r.W, r.H, r.X, r.Y)
				sh.SetRegion(r)
				a.mu.Lock()
				a.region = r
				a.mu.Unlock()
			}()
		}
	}
	// 自动化验证用：分享开始 N 秒后热切换到另一台显示器（跨屏，P1）。
	// 走的是界面"更换区域 / 换屏"的同一条路（Sharer.SetDisplay）。
	if a.switchAt > 0 && a.switchDisp >= 0 && a.switchDisp < len(a.displays) {
		d := a.displays[a.switchDisp]
		if d.ID != opts.Display.ID {
			wait := a.switchAt
			go func() {
				select {
				case <-time.After(wait):
				case <-shCtx.Done():
					return
				}
				log.Printf("自动化：热切换到显示器 %s（%dx%d）", d.Name, d.W, d.H)
				a.applyDisplaySwitch(sh, d, capture.Rect{})
			}()
		}
	}
	go a.hudLoop(hudStop)

	// 置顶悬浮条：主窗最小化（防自摄入）后，它是屏幕上唯一能看到
	// "谁在看我 + 停止分享"的地方。OnStop 在 HUD 窗口 goroutine 里触发，
	// stopShare / Go / RestoreMainWindow 都是锁安全或异步的，可直接调。
	hud := ui.NewHUD(ui.HUDConfig{
		Title: ui.HUDWindowTitle,
		OnStop: func() {
			a.stopShare()
			a.shell.Go(ui.RouteHome)
			go func() {
				if err := ui.RestoreMainWindow(); err != nil {
					log.Printf("hud: 停止后恢复主窗失败：%v（双击托盘图标可恢复）", err)
				}
			}()
		},
	})
	hud.Start()
	a.mu.Lock()
	a.hud = hud
	a.mu.Unlock()

	log.Printf("分享已开始：端口 %d 授权码 %s 地址 %v", srv.Port(), srv.Code(), srv.ShareAddrs())
	printFirewallHint(srv.Port())
	a.pushShareState()
	if a.tray != nil {
		a.tray.SetSharing(true, a.title+" · 正在分享")
		a.tray.Balloon("GoShare 正在分享",
			fmt.Sprintf("端口 %d · 授权码 %s。双击托盘图标恢复窗口。", srv.Port(), srv.Code()))
	}

	// P0-1 防自摄入：自己的窗口会被自己采集成无限套娃（实测套了 6~7 层），
	// 每帧都在变 → 全量帧永不停止 → 带宽白烧。分享一开始就把主窗最小化。
	//
	// 为什么挪到 goroutine：ShowWindow 会同步派发窗口消息，而这里是 Gio 的按钮回调，
	// 在事件处理里重入 Gio 的消息循环是自找麻烦。
	//
	// 为什么要重试：`-auto share` 模式下 startShare 跑在 shell.Run(ctx) **之前**，
	// 那一刻 Gio 窗口还没创建，实测报"没找到本进程的可见顶层窗口"。
	// 交互模式下窗口一定已存在，第一次就成功；重试只是为了同时兼容两种入口，
	// 顺带也覆盖"窗口正在创建、尚未可见"的中间状态。
	go func() {
		for i := 0; i < 20; i++ {
			time.Sleep(150 * time.Millisecond)
			if err := ui.MinimizeMainWindow(); err == nil {
				log.Printf("防自摄入：主窗已最小化（双击托盘图标可恢复查看状态）")
				return
			}
		}
		// 失败不影响分享本身，但画面里会多出自己 —— 明确记一笔，别静默。
		log.Printf("防自摄入：主窗最小化失败（分享不受影响），画面里会看到自己")
	}()
	return nil
}

// hudLoop 定期把运行状态推给界面。
func (a *app) hudLoop(stop chan struct{}) {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-a.ctx.Done():
			return
		case <-t.C:
			a.pushShareState()
		}
	}
}

// pushShareState 汇总分享端状态推给界面。
func (a *app) pushShareState() {
	a.mu.Lock()
	sh, srv := a.sh, a.srv
	a.mu.Unlock()
	if sh == nil || srv == nil {
		return
	}

	st := sh.Stats()
	now := time.Now()
	a.mu.Lock()
	el := now.Sub(a.lastAt).Seconds()
	df := st.Frames - a.lastFrames
	db := st.Bytes - a.lastBytes
	ds := st.SentBytes - a.lastSent
	a.lastFrames, a.lastBytes, a.lastSent, a.lastAt = st.Frames, st.Bytes, st.SentBytes, now
	a.mu.Unlock()

	fps := 0.0
	mbps := 0.0
	// outMbps 是**出网**码率：同一份编码要给 N 个观众各发一份，
	// 所以它是编码码率的 N 倍（弱网被背压丢掉的还不算）。
	// 只显示编码码率会让人严重低估带宽（5 人时 4.0 vs 实际约 16 Mbps）。
	outMbps := 0.0
	if el > 0 {
		fps = float64(df) / el
		mbps = float64(db) * 8 / el / 1e6
		outMbps = float64(ds) * 8 / el / 1e6
	}

	rows := make([]ui.ViewerRow, 0, 8)
	for _, v := range srv.Viewers() {
		rows = append(rows, ui.ViewerRow{Name: v.Name, IP: v.IP, Since: v.Joined})
	}

	regionText := "整屏"
	a.mu.Lock()
	r := a.region
	dispName := a.display.Name
	multiDisp := len(a.displays) > 1
	a.mu.Unlock()
	if !r.Empty() {
		regionText = fmt.Sprintf("区域 %d×%d", r.W, r.H)
	}
	// 多屏机器上标出采的是哪块屏 —— 换屏之后 HUD 是唯一能看到
	// "现在到底在分享哪台显示器"的地方，不写清楚用户只能猜。
	if multiDisp && dispName != "" {
		regionText = dispName + " · " + regionText
	}

	// Stats.EncodeMs 在 Stats() 里已经折算成**均值**（见 pipeline.Sharer.Stats），
	// 拿到手就是"每帧编码耗时"，不要再除一次帧数 —— 除两次会显示成 0.0 ms。
	encAvg := st.EncodeMs

	hud := fmt.Sprintf("发送 %.1f fps · 出网 %.1f Mbps（%d 路）\n编码 %.1f ms · 单路 %.1f Mbps · 采集 %s\n已发 %d 帧 · 观众 %d 人\n共享 %s",
		fps, outMbps, len(rows), encAvg, mbps, st.Backend, st.Frames, len(rows), regionText)

	// 采集源取不到画面（锁屏 / 安全桌面 / 后端异常）时明确说出来。
	// 不提示的话，用户看到的只是"一片黑"，无法区分是自己设置错了还是断线了。
	black := sh.BlackScreen()
	if black {
		hud += "\n⚠ 采集到的画面是全黑的 —— 请确认机器没有锁屏，且不在 UAC 安全桌面"
	}
	// 只在状态翻转时打日志：这个方法每 500ms 跑一次，不去重就会刷屏（实测踩过）。
	a.mu.Lock()
	changed := black != a.blackWarned
	a.blackWarned = black
	a.mu.Unlock()
	if changed {
		if black {
			log.Printf("警告：采集源持续全黑（锁屏 / 安全桌面 / 后端 %s 异常），观众只会看到黑屏", st.Backend)
		} else {
			log.Printf("采集画面已恢复正常")
		}
	}

	// 每 3 秒一条分享端统计。**全量帧占比**是关键指标：增量帧只发变化条带，
	// 全量帧发整帧，两者带宽差一个数量级 —— 带宽异常时先看这个比例。
	a.mu.Lock()
	a.hudTicks++
	tick := a.hudTicks
	a.mu.Unlock()
	if tick%6 == 0 {
		log.Printf("分享统计：%.1f fps · 出网 %.2f Mbps（单路 %.2f × %d 人）· 编码均值 %.1f ms · 全量帧 %d/%d · 观众 %d 人",
			fps, outMbps, mbps, len(rows), encAvg, st.Keys, st.Frames, len(rows))
	}

	a.shell.SetShareState(ui.ShareState{
		Active:     true,
		Code:       srv.Code(),
		Addrs:      srv.ShareAddrs(),
		Port:       srv.Port(),
		PresetName: presetNameOf(sh),
		Paused:     sh.Paused(),
		Viewers:    rows,
		HUD:        hud,
		RegionText: regionText,
		SrcText:    fmt.Sprintf("%.1f fps", st.FPS),
	})

	// 悬浮条：满血时显示档位名，被自适应降档后带上当前质量 ——
	// 画面变糊时用户能直接看到原因，而不是猜"是不是网络坏了"。
	level, _, aq, _ := sh.AdaptiveState()
	qualityText := presetNameOf(sh)
	if level > 0 {
		qualityText = fmt.Sprintf("%s·q%d", qualityText, aq)
	}
	a.mu.Lock()
	hudWin := a.hud
	a.mu.Unlock()
	if hudWin != nil {
		hudWin.SetState(len(rows), qualityText, sh.Paused())
	}
}

func (a *app) stopShare() {
	a.mu.Lock()
	hadSession := a.sh != nil || a.srv != nil
	sh, srv, ann, cancel, hudStop := a.sh, a.srv, a.ann, a.stop, a.stopHUD
	hudWin := a.hud
	a.sh, a.srv, a.ann, a.stop, a.stopHUD = nil, nil, nil, nil, nil
	a.hud = nil
	a.shCtx = nil
	a.view = nil
	a.mu.Unlock()

	if hudWin != nil {
		hudWin.Stop()
	}
	if hudStop != nil {
		close(hudStop)
	}
	if ann != nil {
		ann.Close()
	}
	if srv != nil {
		_ = srv.Close()
	}
	if sh != nil {
		_ = sh.Close()
	}
	if cancel != nil {
		cancel()
	}
	a.shell.SetShareState(ui.ShareState{})
	if a.tray != nil {
		a.tray.SetSharing(false, "GoShare · 内网桌面共享")
	}
	if hadSession {
		log.Printf("分享已停止")
	}
}

func (a *app) setPaused(on bool) {
	a.mu.Lock()
	sh := a.sh
	a.mu.Unlock()
	if sh == nil {
		return
	}
	sh.SetPaused(on)
	log.Printf("暂停状态 → %v", on)
	a.pushShareState()
}

func (a *app) setPreset(name string) {
	a.mu.Lock()
	sh := a.sh
	a.mu.Unlock()
	if sh == nil {
		return
	}
	sh.SetPreset(pipeline.PresetByName(name))
	a.pushShareState()
}

func (a *app) kick(ip string) {
	a.mu.Lock()
	srv := a.srv
	a.mu.Unlock()
	if srv == nil {
		return
	}
	if n := srv.Kick(ip); n > 0 {
		a.shell.Toast("已断开 " + ip)
	}
	a.pushShareState()
}

func (a *app) rotateCode() string {
	a.mu.Lock()
	srv := a.srv
	a.mu.Unlock()
	if srv == nil {
		return ""
	}
	code := srv.Rotate()
	a.pushShareState()
	return code
}

// ---------------------------------------------------------------------------
// 区域框选
// ---------------------------------------------------------------------------

// pickRegionSnapshot 抓一张该显示器的当前画面，供框选页当背景。
func (a *app) pickRegionSnapshot(d capture.Display) *image.NRGBA {
	src, err := capture.NewSource(a.ctx, capture.Options{Display: d, FPS: 30})
	if err != nil {
		log.Printf("框选快照：创建采集源失败 %v", err)
		return nil
	}
	defer src.Close()
	// 首帧可能是未初始化的黑帧（R25），预热掉它，否则用户对着黑屏框选。
	_, _ = src.Warmup(a.ctx, 3, 600*time.Millisecond)

	c, cancel := context.WithTimeout(a.ctx, 2*time.Second)
	defer cancel()
	f, err := src.WaitFrame(c)
	if err != nil || f.Empty() {
		log.Printf("框选快照：取帧失败 err=%v", err)
		return nil
	}
	// 缩一档再交给界面：全屏铺 4K 快照没必要
	scale := fitScale(f.W, f.H)
	v := ui.NewView(f.W, f.H, scale)
	v.Update(f)
	v.Render()
	return v.Image()
}

func (a *app) regionDone(r capture.Rect, ok bool) {
	a.mu.Lock()
	sh := a.sh
	a.mu.Unlock()

	// 分享中进来（"更换区域 / 换屏"）：热切换，不动设置页的状态。
	if sh != nil {
		if !ok {
			return
		}
		d := a.shell.SelectedDisplay()
		a.mu.Lock()
		sameDisp := d.ID == a.display.ID
		a.mu.Unlock()
		if sameDisp {
			log.Printf("界面换区域：%dx%d+%d+%d（整屏=%v）", r.W, r.H, r.X, r.Y, r.Empty())
			sh.SetRegion(r)
			a.mu.Lock()
			a.region = r
			a.mu.Unlock()
		} else {
			log.Printf("界面换屏：%s（%dx%d）区域 %dx%d+%d+%d", d.Name, d.W, d.H, r.W, r.H, r.X, r.Y)
			a.applyDisplaySwitch(sh, d, r)
		}
		a.pushShareState()
		if r.Empty() {
			a.shell.Toast("已切换为整屏分享")
		} else {
			a.shell.Toast(fmt.Sprintf("已切换分享区域 %d×%d", r.W, r.H))
		}
		return
	}

	a.mu.Lock()
	if ok {
		a.region = r
	}
	a.mu.Unlock()
	if ok {
		a.shell.SetSetupNote(fmt.Sprintf("已选择区域 %d×%d @ (%d,%d)", r.W, r.H, r.X, r.Y))
	} else {
		a.shell.SetSetupNote("")
	}
}

// applyDisplaySwitch 热切换采集显示器：换源、更新本地状态、重建预览视图。
//
// 预览视图必须跟着重建：它按"显示器宽高"建的，换了屏还用旧尺寸，
// 本地回显会按错的比例缩放甚至越界。
func (a *app) applyDisplaySwitch(sh *pipeline.Sharer, d capture.Display, r capture.Rect) {
	a.mu.Lock()
	shCtx := a.shCtx
	a.mu.Unlock()
	if shCtx == nil {
		return
	}
	if err := sh.SetDisplay(shCtx, d, r); err != nil {
		log.Printf("换屏失败：%v（保持原显示器）", err)
		a.shell.Toast("换屏失败：" + err.Error())
		return
	}
	a.mu.Lock()
	a.display = d
	a.region = r
	a.view = ui.NewView(d.W, d.H, fitScale(d.W, d.H))
	a.mu.Unlock()
	log.Printf("已切换到显示器 %s（%dx%d）", d.Name, d.W, d.H)
}

// ---------------------------------------------------------------------------
// 观看端
// ---------------------------------------------------------------------------

func (a *app) join(addr, code string) {
	go func() {
		a.mu.Lock()
		a.wFrames, a.wBytes, a.wStart, a.wDec = 0, 0, time.Now(), 0
		a.wLatMS = 0
		a.wChunks = 0
		a.mu.Unlock()
		a.wSkipped.Store(0)
		a.wLastSeq.Store(0)
		a.pliBackoffMs.Store(0)
		a.lastRepairAt.Store(0)
		a.lastMissingAt.Store(0)
		a.lastMissingMask.Store(0)
		a.incompleteSince.Store(0)
		// 新接入 = 画布从零开始：一块都没到之前画布上是空的。
		a.needFull.Store(true)
		a.peerBox.Store(nil)

		dec := codec.NewDecoder(0)
		var sess *signal.Session
		var err error
		for attempt := 1; attempt <= joinAttempts; attempt++ {
			if attempt == 1 {
				a.shell.SetJoinState(ui.JoinState{Dialing: true, Status: "正在连接 " + addr + " …"})
			} else {
				a.shell.SetJoinState(ui.JoinState{Dialing: true,
					Status: fmt.Sprintf("还没连上，%d 秒后重试（第 %d/%d 次）…",
						int(joinRetryWait.Seconds()), attempt, joinAttempts)})
				select {
				case <-time.After(joinRetryWait):
				case <-a.ctx.Done():
					return
				}
			}
			sess, err = a.dialOnce(dec, addr, code)
			if err == nil {
				break
			}
			// 授权码错、观众已满属于确定性失败，重试没有意义
			if strings.Contains(err.Error(), "授权码") || strings.Contains(err.Error(), "已满") {
				break
			}
		}
		if err != nil {
			hint := ""
			if strings.Contains(err.Error(), "媒体通道未建立") {
				hint = "（地址与授权码都对，但 UDP 打不通 —— 检查对方防火墙是否放行 UDP）"
			}
			// 必须同时写日志：界面提示只有人看着才有用，
			// 无人值守验证时日志是唯一的失败线索。
			log.Printf("接入 %s 失败：%v%s", addr, err, hint)
			a.shell.SetJoinState(ui.JoinState{Err: "接入失败：" + err.Error() + hint})
			return
		}
		a.mu.Lock()
		a.sess = sess
		a.wAddr, a.wCode = addr, code
		a.mu.Unlock()
		log.Printf("已接入 %s（%s）· 握手 %.0f ms", sess.Info.Name, addr, sess.ConnectMs)
		a.shell.Go(ui.RouteViewing)
		go a.watchStatusLoop(dec)
	}()
}

// joinAttempts / joinRetryWait 是接入重试次数与间隔。
//
// 为什么要有重试：对方常常还没点"开始分享"（信令端口尚未监听），此时的
// 失败只是时机问题。实测踩过：分享端初始化要 2 秒左右，先打开的观看端
// 一次连不上就永远停在错误页，用户只能手动再点一次。
const (
	joinAttempts  = 3
	joinRetryWait = 4 * time.Second
)

func (a *app) dialOnce(dec *codec.Decoder, addr, code string) (*signal.Session, error) {
	sess, err := signal.Dial(a.ctx, signal.DialConfig{
		Addr: addr,
		Code: code,
		Name: hostName(),
		Peer: rtc.Config{
			UDPPortMin: udpPortMin,
			UDPPortMax: udpPortMax,
			// 解码搬出 SCTP 读 goroutine（见 rtc.Config.AsyncFrames）。
			// 不这么做的话，读路径被我们自己的解码（2K 约 10~20ms/帧）堵住，
			// 帧在接收缓冲里排队 → 延迟单调增长，连 PLI 都发不及时。
			AsyncFrames: true,
			// 丢掉一块 = 它带的条带更新永远没了（增量帧不会重发）。
			// 必须让状态循环去要对端开一轮修复，否则就是"永久缺一块"（R32 同类）。
			OnFrameDrop: func() {
				a.wSkipped.Add(1)
				a.needFull.Store(true)
				now := time.Now().UnixNano()
				if last := a.dropLogAt.Load(); now-last > int64(time.Second) && a.dropLogAt.CompareAndSwap(last, now) {
					log.Printf("观看端解码跟不上，丢弃积压分块（累计 %d）—— 已请求修复轮次补画布", a.wSkipped.Load())
				}
			},
			// OnState 只做记录与打日志 —— 它跑在 ICE agent 的内部 goroutine 上，
			// 任何会等待 ICE 的动作（尤其是 Close）都会和 agent 收尾互等死锁，
			// 表现为进程静默卡死（PLAN 约束 8）。
			OnState: func(st webrtc.ICEConnectionState) {
				prev := webrtc.ICEConnectionState(a.wICE.Swap(int32(st)))
				if prev != st {
					log.Printf("观看连接状态：%v → %v", prev, st)
				}
				// ICE 自愈（disconnected/failed → connected）：抖动期间可能丢过
				// 全量帧的分片，画布上缺的块不会自己回来（增量只覆盖变化区域）——
				// 主动请一帧全量，别等观众盯着残影自己发现。
				if (prev == webrtc.ICEConnectionStateDisconnected || prev == webrtc.ICEConnectionStateFailed) &&
					(st == webrtc.ICEConnectionStateConnected || st == webrtc.ICEConnectionStateCompleted) {
					log.Printf("连接自愈，请求全量帧刷新画布")
					a.needFull.Store(true)
				}
			},
			OnFrames: func(batch []*codec.Frame) {
				// ⚠️ 这里收到的每个元素是**一个分块**，不是一个完整帧：
				// 分块丢了就少解几个条带，其余照常上屏（"丢一块不再废整帧"）。
				// 一批一起解：单块只带 1~2 个条带，逐块串行会把帧内并行度丢光
				// （实测 2K 全量解码 7ms → 98ms）。
				a.wLastFrameAt.Store(time.Now().UnixNano())
				img, err := dec.DecodeBatch(batch)
				if err != nil {
					return
				}
				// 尺寸（或条带切分）变了 → 界面缓冲池重建（旧缓冲尺寸不对）。
				if b := img.Bounds(); a.disp.W() != b.Dx() || a.disp.H() != b.Dy() {
					a.disp.Reset(b.Dx(), b.Dy(), batch[0].TileH, batch[0].TotalTiles)
				}
				var (
					bytes   int
					latSum  float64
					latN    int
					lastSeq uint64
					newSeq  bool
				)
				for _, f := range batch {
					// 记下这一块带的条带：界面缓冲池据此知道要补哪几行。
					a.disp.MarkDirty(tileMask(f))
					bytes += f.Bytes
					// 端到端延迟：帧里的 TS（编码端 Unix 毫秒低 32 位）→ 现在。
					// 用 32 位回绕差，两端时钟差会自然抵消；同机测试尤其准。
					lat := float64(int32(uint32(time.Now().UnixMilli()) - uint32(f.TS)))
					if lat >= 0 {
						latSum += lat
						latN++
					}
					if f.Seq != a.wLastSeq.Load() {
						a.wLastSeq.Store(f.Seq)
						newSeq = true
					}
					lastSeq = f.Seq
				}
				a.mu.Lock()
				a.wChunks += uint64(len(batch))
				a.wBatches++
				a.wBytes += uint64(bytes)
				// DecodeMs 是这一批的解码耗时（同一批往往就是同一帧的几块）。
				a.wDec = dec.DecodeMs
				if latN > 0 {
					lat := latSum / float64(latN)
					// 滑动均值（0.2 权重 ≈ 最近 5 批），单批抖动不误导用户。
					if a.wLatMS == 0 {
						a.wLatMS = lat
					} else {
						a.wLatMS = a.wLatMS*0.8 + lat*0.2
					}
				}
				a.mu.Unlock()

				// ---- 帧边界：这一批跨到了新的一帧，说明上一帧的块都到齐了 ----
				// 只有跨帧才把画面交给界面（每帧一次），否则一帧会被推好几次。
				if !newSeq {
					return
				}
				a.mu.Lock()
				a.wFrames++
				n := a.wFrames
				path := a.dumpPath
				at := a.dumpAt
				a.mu.Unlock()

				// 逐像素统计**降频**：它每帧扫 30 万像素，纯诊断用，
				// 没必要为了日志把解码 goroutine 占满。
				if n%5 == 1 {
					nb, lum := pixStat(img.Pix, img.Bounds().Dx(), img.Bounds().Dy())
					a.mu.Lock()
					a.wNonBlack, a.wLum = nb, lum
					a.mu.Unlock()
				}

				// 画面完整性：判据是我们自己维护的"条带是否都是当前内容"
				// （codec.Decoder.Missing），不再是"收到过全量帧" ——
				// 新格式下丢一块只丢几条带，缺的那几条是**精确已知**的。
				miss := dec.Missing()
				complete := miss == 0
				a.needFull.Store(!complete)
				if complete {
					a.incompleteSince.Store(0)
					a.pliBackoffMs.Store(0)
				} else {
					a.reportMissing(miss, dec.TileCount(), batch[len(batch)-1])
				}
				f0 := batch[len(batch)-1]
				if n <= 5 || f0.Full || f0.Refresh != 0 {
					log.Printf("收到帧 #%d seq=%d（本批 %d 块）全量=%v 修复轮次=%d %dx%d 画布完整=%v",
						n, lastSeq, len(batch), f0.Full, f0.Refresh, f0.W, f0.H, complete)
				}
				if path != "" && n == at {
					log.Printf("诊断：把观看端第 %d 帧（%dx%d）落到 %s",
						n, img.Bounds().Dx(), img.Bounds().Dy(), path)
					if err := savePNG(path, img); err != nil {
						log.Printf("诊断：落盘失败: %v", err)
					}
				}
				// 交给界面：从**缓冲池**取一块（已经与解码画布同步）。
				// 不再每帧新分配 20MB —— 那等于 600MB/s 的垃圾，
				// 解码 goroutine 会被 GC 拖住，进而丢块。
				if buf := a.disp.Take(dec); buf != nil {
					a.shell.PushFrame(buf)
				}
			},
		},
	})
	if err != nil {
		return nil, err
	}
	// Dial 内部才创建 peer，回调里拿不到，只能先装箱再交给 OnFrame。
	a.peerBox.Store(sess.Peer)
	return sess, nil
}

// reconnect 断线后自动重连：同一地址同一授权码，每 4s 一次、最多 12 次。
//
// 为什么值得做：对方重启分享、网络抖动恢复都是常态，让用户手动回到
// 接入页重新输一遍地址授权码，体验是断的。重连成功后 needFull 置位，
// 第一帧全量到达前会主动 PLI —— 画面从"完整"开始，而不是从残影拼回来（R32）。
//
// dead 是触发重连时的那个会话：期间用户退出观看或手动重连都会换掉
// a.sess，发现换了就立即收手（连成了也礼貌关闭）。
func (a *app) reconnect(dec *codec.Decoder, dead *signal.Session) {
	defer a.reconnecting.Store(false)
	// 旧 peer 已经死了，立刻关掉释放资源。不能等重连成功再关：
	// 它的 OnState(closed) 会把刚建好的新会话的 wICE 覆盖成 closed，
	// 状态循环会误判"新会话也断了"，把健康会话再杀一次。
	_ = dead.Peer.Close()
	for attempt := 1; attempt <= 12; attempt++ {
		a.mu.Lock()
		cur := a.sess
		addr, code := a.wAddr, a.wCode
		a.mu.Unlock()
		if cur != dead {
			return
		}
		log.Printf("自动重连 %s（第 %d/12 次）…", addr, attempt)
		sess, err := a.dialOnce(dec, addr, code)
		if err != nil {
			log.Printf("自动重连失败：%v", err)
			select {
			case <-time.After(4 * time.Second):
			case <-a.ctx.Done():
				return
			}
			continue
		}
		a.mu.Lock()
		if a.sess != dead {
			a.mu.Unlock()
			_ = sess.ByeClose()
			return
		}
		a.sess = sess
		a.wFrames, a.wBytes, a.wStart, a.wDec = 0, 0, time.Now(), 0
		a.wLatMS = 0
		a.wChunks = 0
		a.mu.Unlock()
		a.peerBox.Store(sess.Peer)
		a.needFull.Store(true)
		a.wLastSeq.Store(0)
		a.pliBackoffMs.Store(0)
		a.lastRepairAt.Store(0)
		a.lastMissingAt.Store(0)
		a.lastMissingMask.Store(0)
		a.incompleteSince.Store(0)
		log.Printf("自动重连成功（%s），已请求修复轮次补画布", sess.Info.Name)
		return
	}
	log.Printf("自动重连 12 次都失败，放弃（可回到接入页手动重连）")
}

func (a *app) watchStatusLoop(dec *codec.Decoder) {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	ticks := 0
	// 这两个都是本 goroutine 私有的（每次接入新起一个循环），
	// 放在这里而不是 app 上，省掉锁也省掉跨接入的残留状态。
	downTicks := 0
	lastNotice := ""
	for {
		a.mu.Lock()
		sess := a.sess
		a.mu.Unlock()
		if sess == nil {
			return
		}
		select {
		case <-a.ctx.Done():
			return
		case <-t.C:
		}
		a.mu.Lock()
		el := time.Since(a.wStart).Seconds()
		fps := 0.0
		mbps := 0.0
		if el > 0 {
			fps = float64(a.wFrames) / el
			mbps = float64(a.wBytes) * 8 / el / 1e6
		}
		st := fmt.Sprintf("%s · %.1f fps · %.1f Mbps · 解码 %.1f ms · 延迟 %.0f ms",
			sess.Info.Name, fps, mbps, a.wDec, a.wLatMS)
		nb, lum, fr := a.wNonBlack, a.wLum, a.wFrames
		a.mu.Unlock()

		// 断流判定。分两种情况给不同的话术 —— 只报"fps 0.0"用户不知道发生了什么：
		//   ICE 断开  → 对方停止分享 or 网络断了（要去排查）
		//   ICE 正常但画面不来 → 多半是对方暂停了（等一下就好）
		js := ui.JoinState{Status: st}
		ice := webrtc.ICEConnectionState(a.wICE.Load())
		frameAge := time.Duration(0)
		if t := a.wLastFrameAt.Load(); t > 0 {
			frameAge = time.Since(time.Unix(0, t))
		}
		switch ice {
		case webrtc.ICEConnectionStateFailed, webrtc.ICEConnectionStateClosed,
			webrtc.ICEConnectionStateDisconnected:
			// 防抖：ICE 短暂 disconnected 很常见（几秒内常会自愈），
			// 立刻弹"对方已停止分享"会误报；连续 1.5s 才认。
			downTicks++
			switch {
			case downTicks >= 3:
				js.Notice = fmt.Sprintf("连接已断开（%v）—— 正在自动重连，也可退出后手动连接", ice)
				js.NoticeWarn = true
				// 持续断开 → 后台自动重连（同一地址/授权码，限 12 次）。
				// CAS 保证同一时间只有一个重连在进行。
				if a.reconnecting.CompareAndSwap(false, true) {
					go a.reconnect(dec, sess)
				}
			case lastNotice != "":
				// 防抖期内**沿用**上一条提示，不要凭空清空。
				// 清空会让下面那句"提示已清除"的日志谎报"画面恢复正常" ——
				// 实测踩过：ICE 刚转 disconnected 的那一帧打出"画面恢复正常"，
				// 而画面明明还是死的（只是防抖计数还没到 3）。
				js.Notice = lastNotice
				js.NoticeWarn = true
			}
		default:
			downTicks = 0
			// 静止桌面有 2s 心跳帧保活，所以"5s 没有帧"本身就是异常。
			if frameAge > 5*time.Second {
				js.Notice = "画面已超过 5 秒没有更新 —— 对方可能已暂停分享"
			}
		}
		if js.Notice != lastNotice {
			// 只在翻转时打一条，否则每 500ms 刷屏（PLAN 约束 17）。
			if js.Notice == "" {
				// 措辞必须区分"真恢复"与"判据切换导致提示消失"：
				// 只有最近确实收到帧，才敢说画面恢复了。
				if frameAge <= 2*time.Second {
					log.Printf("观看提示已清除：画面恢复更新（%.1fs 前收到帧）", frameAge.Seconds())
				} else {
					log.Printf("观看提示已清除，但最近一帧已是 %.1fs 前 —— 画面并未恢复，只是判据切换",
						frameAge.Seconds())
				}
			} else {
				log.Printf("观看提示上屏：%s", js.Notice)
			}
			lastNotice = js.Notice
		}
		a.shell.SetJoinState(js)
		ps := sess.Peer.Stats()
		// 画布不完整（丢块 / 丢帧 / 刚接入）→ 请对端开一轮渐进式修复。
		// 退避与"修复在飞就不重复要"都在 sendPLI/requestRepair 里。
		if a.needFull.Load() {
			a.sendPLI(sess.Peer)
		}
		ticks++
		if ticks%6 == 0 {
			// 每 3s 一条：非黑占比是"画面真的到了"的像素级证据。
			// 解码跳块（本地产能不足）与坏块（对端/协议问题）单列，
			// 别把"我自己解不过来"错当成"网络丢了"。
			a.mu.Lock()
			chunks, batches := a.wChunks, a.wBatches
			a.mu.Unlock()
			freeN, heldN, missN := a.disp.Stats()
			avgBatch := 0.0
			if batches > 0 {
				avgBatch = float64(chunks) / float64(batches)
			}
			log.Printf("观看统计：%s · 非黑 %.1f%% · 亮度 %.1f · 已收 %d 帧/%d 块（均 %.1f 块/批）· 解码跳块 %d · 坏块 %d · 画布完整=%v · 缓冲池 空闲%d/持有%d/兜底累计%d",
				st, nb*100, lum, fr, chunks, avgBatch, a.wSkipped.Load(), ps.BadChunks, !a.needFull.Load(),
				freeN, heldN, missN)
		}
	}
}

func (a *app) leaveView() {
	a.mu.Lock()
	sess := a.sess
	a.sess = nil
	a.mu.Unlock()
	if sess != nil {
		// 主动告别：否则分享端要等 ICE 超时才回收，期间名额白占。
		if err := sess.ByeClose(); err != nil {
			log.Printf("告别分享端失败（%v），由对方超时回收", err)
		}
	}
	a.shell.SetJoinState(ui.JoinState{})
}

func (a *app) scan() {
	go func() {
		a.shell.SetJoinState(ui.JoinState{Scanning: true, Status: "正在扫描局域网…"})
		b, err := discover.NewBrowser()
		if err != nil {
			a.shell.SetJoinState(ui.JoinState{Err: "扫描失败：" + err.Error()})
			return
		}
		defer b.Close()
		ctx, cancel := context.WithTimeout(a.ctx, 4*time.Second)
		defer cancel()
		entries, err := b.Scan(ctx, 4*time.Second)
		if err != nil {
			a.shell.SetJoinState(ui.JoinState{Err: "扫描失败：" + err.Error()})
			return
		}
		rows := make([]ui.FoundRow, 0, len(entries))
		for _, e := range entries {
			addr := ""
			if len(e.Addrs) > 0 {
				addr = e.Addrs[0]
			}
			rows = append(rows, ui.FoundRow{Name: e.Name, Addr: addr})
		}
		if len(rows) == 0 {
			a.shell.SetJoinState(ui.JoinState{Status: "没有发现其他人在分享"})
			return
		}
		a.shell.SetJoinState(ui.JoinState{Status: fmt.Sprintf("发现 %d 个分享，点击填入地址", len(rows)), Found: rows})
	}()
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// pixStat 统计一幅 NRGBA 的非黑占比与平均亮度。
//
// 这是"画面真的到了"的判据：连接建立、帧计数增长都只说明数据在流动，
// 只有像素统计能说明内容不是一片黑（阶段 2/3 反复用到）。
func pixStat(pix []byte, w, h int) (nonBlack, meanLum float64) {
	step := 1
	if w*h > 400000 {
		step = 4
	}
	n, sum, nb := 0, 0, 0
	for y := 0; y < h; y += step {
		row := y * w * 4
		for x := 0; x < w; x += step {
			i := row + x*4
			lum := (77*int(pix[i]) + 150*int(pix[i+1]) + 29*int(pix[i+2])) >> 8
			sum += lum
			if lum > 8 {
				nb++
			}
			n++
		}
	}
	if n == 0 {
		return 0, 0
	}
	return float64(nb) / float64(n), float64(sum) / float64(n)
}

// 注意：这里**没有** copyNRGBA 了。观看画面以前是"解码器画布整帧拷一份"
// （2K 一次 20MB，每帧一次），现在是 cmd/goshare/viewbuf.go 的缓冲池：
// 复用固定两块内存，只同步变化过的条带，交给界面时零拷贝。

// savePNG 把一帧原样落盘（诊断用）。
func savePNG(path string, img image.Image) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}

// fitScale 计算把桌面缩到界面显示区（约 960×540）所需的整数降采样倍率。
func fitScale(w, h int) int {
	s := w / 960
	if sy := h / 540; sy > s {
		s = sy
	}
	if s < 1 {
		s = 1
	}
	return s
}

func presetNameOf(sh *pipeline.Sharer) string { return sh.Preset().Name }

func hostName() string {
	if h, err := os.Hostname(); err == nil {
		return h
	}
	return "GoShare"
}

// parseRect 解析 "宽x高+左+上"（如 800x600+100+100）。
// 空串与 "full"/"整屏" 表示恢复整屏（用空区域表示，与界面语义一致）。
//
// 结果统一过一遍 Rect.Clamp：一是裁到显示器范围内，二是**保证宽高为偶数**
// （视频编码要求，奇数宽高会在转换环节出问题）。偶数化这种事交给
// 已有的 Clamp 做，比自己再写一遍靠谱。
func parseRect(s string, d capture.Display) (capture.Rect, error) {
	t := strings.TrimSpace(s)
	if t == "" || t == "full" || t == "整屏" {
		return capture.Rect{}, nil
	}
	var w, h, x, y int
	n, err := fmt.Sscanf(t, "%dx%d+%d+%d", &w, &h, &x, &y)
	if n < 2 {
		return capture.Rect{}, fmt.Errorf("格式应为 宽x高+左+上（如 800x600+100+100）：%v", err)
	}
	if n < 4 {
		x, y = 0, 0
	}
	r := capture.Rect{X: x, Y: y, W: w, H: h}.Clamp(d)
	if r.Empty() {
		return capture.Rect{}, fmt.Errorf("区域 %dx%d+%d+%d 裁剪后为空（显示器 %dx%d）", w, h, x, y, d.W, d.H)
	}
	return r, nil
}

// printFirewallHint 打印一条可直接执行的放行命令。
//
// 同事机器上最常见的失败原因就是 Domain 防火墙拦了入站 UDP：
// 地址、授权码都对，但连接就是建不起来。
func printFirewallHint(port int) {
	log.Printf("若同事连不上，在本机管理员终端执行一次：")
	log.Printf(`  netsh advfirewall firewall add rule name="GoShare" dir=in action=allow protocol=TCP localport=%d`, port)
	log.Printf(`  netsh advfirewall firewall add rule name="GoShare-UDP" dir=in action=allow protocol=UDP localport=%d-%d`, udpPortMin, udpPortMax)
}
