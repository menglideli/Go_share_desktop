// sigdemo — 阶段 3 · 信令与接入演示
//
// 两个进程跑完整接入链路：
//
//	host   采集屏幕 → 起 HTTP 信令 → 广播"我在这" → 等同事接入
//	watch  发现/直连 → HTTP 换 SDP → P2P 收帧 → Gio 观看窗
//
// 与阶段 2 的 sharedemo 的区别：sharedemo 在一个进程里手工把 offer/answer 递给对方，
// 这里必须走真实的 HTTP 信令与授权码 —— 也就是同事实际会走的那条路。
package main

import (
	"context"
	"flag"
	"fmt"
	"image"
	"log"
	"math"
	"os"
	ossignal "os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
	"goshare/internal/capture"
	"goshare/internal/clip"
	"goshare/internal/codec"
	"goshare/internal/discover"
	"goshare/internal/pipeline"
	"goshare/internal/rtc"
	"goshare/internal/signal"
	"goshare/internal/ui"
)

// 媒体面 UDP 端口区间：固定下来才能写防火墙放行规则（见打印的 netsh 指引）。
const (
	udpPortMin = 50000
	udpPortMax = 50100
)

type atomicBits uint64

func (a *atomicBits) store(v float64) { atomic.StoreUint64((*uint64)(a), math.Float64bits(v)) }
func (a *atomicBits) load() float64   { return math.Float64frombits(atomic.LoadUint64((*uint64)(a))) }

func main() {
	mode := flag.String("mode", "host", "host = 我要分享 / watch = 我要观看")
	// 分享端
	displayIdx := flag.Int("display", -1, "采集哪个显示器（-1 = 自动避开主屏）")
	presetName := flag.String("preset", "最大", "质量档位：最大 / 流畅60 / 流畅30 / 急速")
	port := flag.Int("port", 9000, "信令起始端口（被占自动顺延）")
	maxViewers := flag.Int("max", 8, "同时观看人数上限")
	hostName := flag.String("name", "", "分享名（默认用主机名）")
	noCopy := flag.Bool("nocopy", false, "不自动把地址写进剪贴板")
	// 观看端
	addr := flag.String("addr", "", "观看地址 IP:端口；留空则自动发现")
	code := flag.String("code", "", "授权码")
	pick := flag.Int("pick", 0, "自动发现时选第几个（0 = 第一个）")
	scan := flag.Duration("scan", 3*time.Second, "自动发现的扫描时长")
	// 通用
	exitAfter := flag.Duration("exit", 0, "自动退出时长（如 30s），0 = 一直运行")
	logPath := flag.String("log", "", "日志文件")
	flag.Parse()

	if *logPath != "" {
		if f, err := os.Create(*logPath); err == nil {
			defer f.Close()
			log.SetOutput(f)
		}
	}
	if lvl := capture.EnsureDPIAware(); lvl != 2 {
		log.Printf("警告: DPI 感知级别 %d（期望 2）", lvl)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigc := make(chan os.Signal, 1)
	ossignal.Notify(sigc, os.Interrupt)
	go func() { <-sigc; fmt.Println("\n收到中断，退出"); cancel() }()

	var err error
	switch *mode {
	case "host", "share":
		err = runHost(ctx, hostArgs{
			displayIdx: *displayIdx, presetName: *presetName, port: *port,
			maxViewers: *maxViewers, name: *hostName, noCopy: *noCopy, exitAfter: *exitAfter,
		})
	case "watch", "view":
		err = runWatch(ctx, watchArgs{
			addr: *addr, code: *code, pick: *pick, scan: *scan, exitAfter: *exitAfter,
		})
	default:
		fmt.Fprintf(os.Stderr, "未知 mode=%q（host / watch）\n", *mode)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

// ---------------------------------------------------------------------------
// 分享端
// ---------------------------------------------------------------------------

type hostArgs struct {
	displayIdx int
	presetName string
	port       int
	maxViewers int
	name       string
	noCopy     bool
	exitAfter  time.Duration
}

func runHost(ctx context.Context, a hostArgs) error {
	ds, err := capture.Displays(ctx)
	if err != nil || len(ds) == 0 {
		return fmt.Errorf("枚举显示器失败: %v", err)
	}
	// 自摄入防护（P0-1）：Win10 LTSC 17763 没有 WDA_EXCLUDEFROMCAPTURE，
	// 自己的窗口会被自己采进去。单进程演示里最省事的办法是"采另一块屏"。
	sel := ds[0]
	for _, d := range ds {
		if d.Primary {
			sel = d
			break
		}
	}
	if len(ds) > 1 {
		for _, d := range ds {
			if !d.Primary {
				sel = d
				break
			}
		}
	} else {
		fmt.Printf("⚠️ 只有一块显示器：本进程的窗口会被自己采集（自摄入）\n")
	}
	if a.displayIdx >= 0 && a.displayIdx < len(ds) {
		sel = ds[a.displayIdx]
	}

	p := pipeline.PresetByName(a.presetName)
	sh, err := pipeline.NewSharer(ctx, pipeline.Config{
		Display: sel,
		FPS:     30,
		Preset:  p,
		Cursor:  true,
	})
	if err != nil {
		return fmt.Errorf("启动采集失败: %v", err)
	}
	defer sh.Close()

	name := a.name
	if name == "" {
		if h, err := os.Hostname(); err == nil {
			name = h
		} else {
			name = "GoShare"
		}
	}

	// 先声明后创建：accept 闭包里要引用 srv（ICE 断了要回收观众记录），
	// 而 srv 要等采集与管线都就绪才建。闭包捕获的是变量本身，创建时已赋值。
	var srv *signal.Server

	// 观众接入处理器：每次 HTTP 握手都要建一个独立的 Peer 挂进管线。
	accept := func(req signal.JoinRequest) (webrtc.SessionDescription, func(), error) {
		// cleanup 会被三条路径触发（正常断开 / ICE failed / 断线超时），
		// 必须幂等 —— 否则 RemovePeer 会重复执行，日志也会刷屏。
		//
		// 幂等**不能用 sync.Once**：cleanup 会走到 srv.Drop，而 Drop 会回调
		// closeFn，closeFn 就是 cleanup 自己 —— 同一 goroutine 重入 once.Do
		// 会永久阻塞（实测：观众断开瞬间整个分享端卡死，后续接入全部超时）。
		// 用 mutex+flag，重入时直接返回。见 R27。
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

			// 重活必须挪出调用线程。cleanup 常由 pion 的 ICE 状态回调同步触发，
			// 而 peer.Close() 要等 ICE agent 收尾 —— 回调不返回、agent 就退不出，
			// 两边互等，整个分享端当场卡死（后续观众全部接入超时）。见 R27。
			work := func() {
				if peer != nil {
					sh.RemovePeer(peer)
					_ = peer.Close()
				}
				if srv != nil {
					// Detach 而不是 Drop：Peer 已经在这里关了，不让服务端再回调回来。
					srv.Detach(req.Token)
				}
				log.Printf("观众 %s 已释放（%s），剩余 %d 人", req.RemoteIP, reason, sh.PeerCount())
			}
			go work()
		}
		// 必须先声明再赋值，不能写成 `p, err := rtc.NewPeer(...{OnState: 用 p})`：
		// 短变量声明的变量作用域从**语句之后**才开始，回调里根本看不到自己。
		// （踩过：编译报 "p.PC undefined (type pipeline.Preset)"，因为它解析到了外层的档位变量。）
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
					// 不立刻回收是因为 disconnected 也常只是短暂抖动，几秒内会自愈。
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
	}

	srv, err = signal.NewServer(signal.ServerConfig{
		Name:       name,
		Port:       a.port,
		MaxViewers: a.maxViewers,
		Accept:     accept,
		Describe: func() (int, int, string, string) {
			r := sh.Source().Region()
			return r.W, r.H, p.Name, sh.Source().Backend()
		},
		Logger: log.Default(),
	})
	if err != nil {
		return fmt.Errorf("创建信令服务失败: %v", err)
	}
	if err := srv.Start(); err != nil {
		return fmt.Errorf("启动信令失败: %v", err)
	}
	defer srv.Close()

	ann, err := discover.NewAnnouncer(discover.Announce{
		ID:       fmt.Sprintf("%s-%d", name, srv.Port()),
		Name:     name,
		Addrs:    srv.ShareAddrs(),
		NeedCode: true,
		Width:    sh.Source().Region().W,
		Height:   sh.Source().Region().H,
		MaxView:  a.maxViewers,
	}, 1500*time.Millisecond)
	if err != nil {
		return fmt.Errorf("启动广播失败: %v", err)
	}
	defer ann.Close()

	// ---------- 把"同事要抄的东西"摆在最显眼的位置 ----------
	// 关键接入信息同时进日志：自动化验证要从日志里取授权码，不能只打印到屏幕。
	log.Printf("分享已开始：地址=%v 授权码=%s 端口=%d", srv.ShareAddrs(), srv.Code(), srv.Port())

	fmt.Printf("\n=============== 分享已开始 ===============\n")
	fmt.Printf("  采集：%s（%s）\n", sel.String(), sh.Source().Backend())
	fmt.Printf("  档位：%s · UDP 端口 %d-%d\n\n", p.Name, udpPortMin, udpPortMax)
	fmt.Printf("  授权码：  %s\n\n", srv.Code())
	fmt.Printf("  可复制的地址（按推荐顺序）：\n")
	for i, a2 := range srv.ShareAddrs() {
		tag := ""
		if i == 0 {
			tag = "   ← 优先复制这个"
		}
		fmt.Printf("    %d) %s%s\n", i+1, a2, tag)
	}
	line := srv.AddrLine()
	fmt.Printf("\n  一行版：  %s\n", line)
	if !a.noCopy {
		if err := clip.SetText(line); err == nil {
			fmt.Printf("  ✅ 已复制到剪贴板（Ctrl+V 即可发给同事）\n")
		} else {
			fmt.Printf("  ⚠️ 剪贴板写入失败（%v），请手动选中复制\n", err)
		}
	}
	fmt.Printf("\n  同事连不上时多半是防火墙。管理员 PowerShell 执行：\n")
	fmt.Printf("    netsh advfirewall firewall add rule name=\"GoShare-TCP\" dir=in action=allow protocol=TCP localport=%d\n", srv.Port())
	fmt.Printf("    netsh advfirewall firewall add rule name=\"GoShare-UDP\" dir=in action=allow protocol=UDP localport=%d-%d\n", udpPortMin, udpPortMax)
	fmt.Printf("==========================================\n\n")

	go func() {
		if err := sh.Run(ctx); err != nil && ctx.Err() == nil {
			log.Printf("分享循环结束: %v", err)
		}
	}()

	started := time.Now()
	var lastFrames uint64
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	var stop <-chan time.Time
	if a.exitAfter > 0 {
		t := time.NewTimer(a.exitAfter)
		defer t.Stop()
		stop = t.C
	}
	for {
		select {
		case <-ctx.Done():
			fmt.Printf("\n分享结束：共编码 %d 帧\n", sh.Stats().Frames)
			return nil
		case <-stop:
			el := time.Since(started).Seconds()
			st := sh.Stats()
			fmt.Printf("\n到时退出：%.1fs 内编码 %d 帧（%.1f fps），当前 %d 位观众\n",
				el, st.Frames, float64(st.Frames)/el, sh.PeerCount())
			return nil
		case <-tick.C:
			el := time.Since(started).Seconds()
			st := sh.Stats()
			cur := st.Frames
			d := cur - lastFrames
			lastFrames = cur
			mbps := float64(st.Bytes) * 8 / el / 1e6
			fmt.Printf("[分享] %.0fs · 采集 %.1f fps · 编码 %.1f fps · %.1f Mbps · 编码 %.1f ms · 观众 %d 人\n",
				el, st.FPS, float64(d)/2.0, mbps, st.EncodeMs, sh.PeerCount())
			ann.Update(func(x *discover.Announce) { x.Viewers = sh.PeerCount() })
		}
	}
}

// ---------------------------------------------------------------------------
// 观看端
// ---------------------------------------------------------------------------

type watchArgs struct {
	addr      string
	code      string
	pick      int
	scan      time.Duration
	exitAfter time.Duration
}

func runWatch(ctx context.Context, a watchArgs) error {
	addr := a.addr
	// 自动发现：不用手抄 IP，扫一下网段里谁在分享
	if addr == "" {
		br, err := discover.NewBrowser()
		if err != nil {
			return fmt.Errorf("监听发现端口失败: %v（可能已有另一个观看端在跑）", err)
		}
		defer br.Close()
		fmt.Printf("正在扫描局域网（%.0fs）…\n", a.scan.Seconds())
		got, _ := br.Scan(ctx, a.scan)
		if len(got) == 0 {
			return fmt.Errorf("没发现任何分享 —— 确认对方已开始分享，或用 -addr IP:端口 直接连")
		}
		fmt.Printf("发现 %d 个分享：\n", len(got))
		for i, e := range got {
			addrShown := ""
			if len(e.Addrs) > 0 {
				addrShown = e.Addrs[0]
			}
			fmt.Printf("  [%d] %s  %s  %dx%d  来自 %s\n", i, e.Name, addrShown, e.Width, e.Height, e.From)
		}
		if a.pick >= len(got) {
			a.pick = 0
		}
		e := got[a.pick]
		if len(e.Addrs) == 0 {
			return fmt.Errorf("分享 %q 没有公告地址", e.Name)
		}
		addr = e.Addrs[0]
		fmt.Printf("选择 [%d] %s → %s\n", a.pick, e.Name, addr)
	}
	if a.code == "" {
		return fmt.Errorf("需要授权码：-code 123456（分享端窗口里能看到）")
	}

	// 画布轮转：解码线程与渲染线程不能同时碰同一块内存（阶段 2 踩过，会直接崩）
	frames := make(chan *image.NRGBA, 2)
	recycle := make(chan *image.NRGBA, 3)
	dec := codec.NewDecoder(0)
	var recvFrames, recvBytes uint64
	var lastDec atomicBits

	started := time.Now()
	sess, err := signal.Dial(ctx, signal.DialConfig{
		Addr: addr,
		Code: a.code,
		Name: viewerName(),
		Peer: rtc.Config{
			UDPPortMin: udpPortMin,
			UDPPortMax: udpPortMax,
			OnFrame: func(f *codec.Frame) {
				img, err := dec.Decode(f)
				if err != nil {
					return
				}
				recvFrames++
				recvBytes += uint64(f.Bytes)
				lastDec.store(dec.DecodeMs)
				var dst *image.NRGBA
				select {
				case dst = <-recycle:
					if dst.Bounds().Dx() != img.Bounds().Dx() || dst.Bounds().Dy() != img.Bounds().Dy() {
						dst = image.NewNRGBA(img.Bounds())
					}
				default:
					dst = image.NewNRGBA(img.Bounds())
				}
				copy(dst.Pix, img.Pix)
				select {
				case frames <- dst:
				default:
					select {
					case recycle <- dst:
					default:
					}
				}
			},
		},
	})
	if err != nil {
		hint := ""
		if strings.Contains(err.Error(), "媒体通道未建立") {
			hint = "\n  提示：地址与授权码都对，说明 HTTP 握手成功但 UDP 打不通 —— 检查对方防火墙是否放行 UDP"
		}
		return fmt.Errorf("接入 %s 失败: %w%s", addr, err, hint)
	}
	defer sess.ByeClose()

	fmt.Printf("已接入 %s（%s）· 握手 %.0f ms · 发送 %dx%d\n",
		sess.Info.Name, addr, sess.ConnectMs, sess.Info.Width, sess.Info.Height)

	title := fmt.Sprintf("GoShare · 观看 %s", sess.Info.Name)
	return ui.RunViewer(ctx, ui.ViewerConfig{
		Title:     title,
		Frames:    frames,
		Recycle:   recycle,
		ExitAfter: a.exitAfter,
		OnExit: func() {
			// 必须在硬退出（os.Exit）之前同步发完：否则分享端会一直挂着这位观众，
			// 直到 ICE 超时才回收 —— 期间人数名额是被白占的。
			if err := sess.ByeClose(); err != nil {
				log.Printf("viewer: 告别分享端失败（%v），由对方超时回收", err)
			}
		},
		Status: func() string {
			el := time.Since(started).Seconds()
			fps := float64(recvFrames) / el
			mbps := float64(recvBytes) * 8 / el / 1e6
			return fmt.Sprintf("已接入 %.0f ms · 接收 %.1f fps · %.1f Mbps · 解码 %.1f ms",
				sess.ConnectMs, fps, mbps, lastDec.load())
		},
	})
}

func viewerName() string {
	if h, err := os.Hostname(); err == nil {
		return h
	}
	return "观众"
}
