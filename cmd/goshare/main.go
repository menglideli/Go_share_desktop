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
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"

	"goshare/internal/capture"
	"goshare/internal/codec"
	"goshare/internal/discover"
	"goshare/internal/pipeline"
	"goshare/internal/rtc"
	"goshare/internal/signal"
	"goshare/internal/ui"
)

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

	a := &app{ctx: ctx, port: *port, maxViewers: *maxViewers, fixedCode: *code}
	a.shell = ui.NewShell(ui.ShellConfig{
		Title:    "GoShare · 内网桌面共享",
		Displays: ds,
		Presets:  names,
		ExitAfter: *exitAfter,

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
	region  capture.Rect
	// HUD 增量统计用
	lastFrames uint64
	lastBytes  uint64
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

	// blackWarned 记录"全黑"告警是否已上报，避免每 500ms 刷屏。
	blackWarned bool
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
	a.view = ui.NewView(opts.Display.W, opts.Display.H, fitScale(opts.Display.W, opts.Display.H))
	a.mu.Unlock()

	sh, err := pipeline.NewSharer(shCtx, pipeline.Config{
		Display: opts.Display,
		Region:  opts.Region,
		Preset:  pipeline.PresetByName(opts.Preset),
		Cursor:  true,
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
	a.stopHUD = make(chan struct{})
	a.lastFrames, a.lastBytes, a.lastAt = 0, 0, time.Now()
	hudStop := a.stopHUD
	a.mu.Unlock()

	go func() {
		if err := sh.Run(shCtx); err != nil && shCtx.Err() == nil {
			log.Printf("分享管线结束: %v", err)
		}
	}()
	go a.hudLoop(hudStop)

	log.Printf("分享已开始：端口 %d 授权码 %s 地址 %v", srv.Port(), srv.Code(), srv.ShareAddrs())
	printFirewallHint(srv.Port())
	a.pushShareState()
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
	a.lastFrames, a.lastBytes, a.lastAt = st.Frames, st.Bytes, now
	a.mu.Unlock()

	fps := 0.0
	mbps := 0.0
	if el > 0 {
		fps = float64(df) / el
		mbps = float64(db) * 8 / el / 1e6
	}

	rows := make([]ui.ViewerRow, 0, 8)
	for _, v := range srv.Viewers() {
		rows = append(rows, ui.ViewerRow{Name: v.Name, IP: v.IP, Since: v.Joined})
	}

	regionText := "整屏"
	a.mu.Lock()
	r := a.region
	a.mu.Unlock()
	if !r.Empty() {
		regionText = fmt.Sprintf("区域 %d×%d", r.W, r.H)
	}

	hud := fmt.Sprintf("发送 %.1f fps · %.1f Mbps\n编码 %.1f ms · 采集 %s\n已发 %d 帧 · 观众 %d 人\n共享 %s",
		fps, mbps, st.EncodeMs, st.Backend, st.Frames, len(rows), regionText)

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
}

func (a *app) stopShare() {
	a.mu.Lock()
	hadSession := a.sh != nil || a.srv != nil
	sh, srv, ann, cancel, hudStop := a.sh, a.srv, a.ann, a.stop, a.stopHUD
	a.sh, a.srv, a.ann, a.stop, a.stopHUD = nil, nil, nil, nil, nil
	a.view = nil
	a.mu.Unlock()

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

// ---------------------------------------------------------------------------
// 观看端
// ---------------------------------------------------------------------------

func (a *app) join(addr, code string) {
	go func() {
		a.mu.Lock()
		a.wFrames, a.wBytes, a.wStart, a.wDec = 0, 0, time.Now(), 0
		a.mu.Unlock()

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
		a.mu.Unlock()
		log.Printf("已接入 %s（%s）· 握手 %.0f ms", sess.Info.Name, addr, sess.ConnectMs)
		a.shell.Go(ui.RouteViewing)
		go a.watchStatusLoop()
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
				OnFrame: func(f *codec.Frame) {
					img, err := dec.Decode(f)
					if err != nil {
						return
					}
					nb, lum := pixStat(img.Pix, img.Bounds().Dx(), img.Bounds().Dy())
					a.mu.Lock()
					a.wFrames++
					a.wBytes += uint64(f.Bytes)
					a.wDec = dec.DecodeMs
					a.wNonBlack, a.wLum = nb, lum
					a.mu.Unlock()
					// 直接给界面新画布：解码器内部缓冲会被下一帧覆盖，
					// 与渲染线程共用同一块内存会直接崩（阶段 2 踩过）。
					a.shell.PushFrame(copyNRGBA(img))
				},
			},
	})
	return sess, err
}

func (a *app) watchStatusLoop() {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	ticks := 0
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
		st := fmt.Sprintf("%s · %.1f fps · %.1f Mbps · 解码 %.1f ms", sess.Info.Name, fps, mbps, a.wDec)
		nb, lum, fr := a.wNonBlack, a.wLum, a.wFrames
		a.mu.Unlock()
		a.shell.SetJoinState(ui.JoinState{Status: st})
		ticks++
		if ticks%6 == 0 {
			// 每 3s 一条：非黑占比是"画面真的到了"的像素级证据
			log.Printf("观看统计：%s · 非黑 %.1f%% · 亮度 %.1f · 已收 %d 帧", st, nb*100, lum, fr)
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

func copyNRGBA(src *image.NRGBA) *image.NRGBA {
	dst := image.NewNRGBA(src.Bounds())
	copy(dst.Pix, src.Pix)
	return dst
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

// printFirewallHint 打印一条可直接执行的放行命令。
//
// 同事机器上最常见的失败原因就是 Domain 防火墙拦了入站 UDP：
// 地址、授权码都对，但连接就是建不起来。
func printFirewallHint(port int) {
	log.Printf("若同事连不上，在本机管理员终端执行一次：")
	log.Printf(`  netsh advfirewall firewall add rule name="GoShare" dir=in action=allow protocol=TCP localport=%d`, port)
	log.Printf(`  netsh advfirewall firewall add rule name="GoShare-UDP" dir=in action=allow protocol=UDP localport=%d-%d`, udpPortMin, udpPortMax)
}
