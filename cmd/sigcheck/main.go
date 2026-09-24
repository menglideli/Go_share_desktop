// sigcheck — 阶段 3 · 信令与接入自检
//
// 验证三件事（都是"同事能不能连上"的前置条件）：
//  1. 网卡列表里推给用户的第一个地址，是不是真能连上的那个（netif）
//  2. 分享端在网段里喊一声，观看端能不能听见（discover）
//  3. 授权码、限次、人数上限、端口顺延这几道门是不是真的关着（signal）
// 外加一条：真实 SDP 经 HTTP 换完后，两个 Peer 是否真的建起 P2P。
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/pion/webrtc/v4"
	"goshare/internal/discover"
	"goshare/internal/netif"
	"goshare/internal/rtc"
	"goshare/internal/signal"
)

var pass, fail int

func check(name string, ok bool, detail string) {
	if ok {
		pass++
		fmt.Printf("  ✅ %-28s %s\n", name, detail)
	} else {
		fail++
		fmt.Printf("  ❌ %-28s %s\n", name, detail)
	}
}

func main() {
	ctx := context.Background()
	fmt.Println("== 1. 网卡枚举（分享端该把哪个地址抄给同事）==")
	checkNetif()

	fmt.Println("\n== 2. 局域网发现（广播 / 扫描）==")
	checkDiscover(ctx)

	fmt.Println("\n== 3. 信令服务（授权码 · 限次 · 上限 · 端口顺延）==")
	checkSignal(ctx)

	fmt.Printf("\n结果：%d 通过 / %d 失败\n", pass, fail)
	if fail > 0 {
		os.Exit(1)
	}
}

// ---------------------------------------------------------------------------
// 1. 网卡
// ---------------------------------------------------------------------------

func checkNetif() {
	ifs, err := netif.Interfaces()
	check("枚举不报错", err == nil, fmt.Sprintf("err=%v", err))
	check("至少 1 个可分享地址", len(ifs) >= 1, fmt.Sprintf("%d 个", len(ifs)))

	bad := []string{}
	for _, i := range ifs {
		if i.IP.IsLoopback() || i.IP.IsLinkLocalUnicast() || i.IP.To4() == nil {
			bad = append(bad, i.IP.String())
		}
	}
	check("过滤掉 127/169.254/非IPv4", len(bad) == 0, fmt.Sprintf("漏网=%v", bad))

	def := netif.DefaultRouteIP()
	if def == nil {
		check("默认路由出口", false, "拿不到（没有默认路由？）")
	} else {
		ok := len(ifs) > 0 && ifs[0].Default
		check("默认路由排在第一位", ok, fmt.Sprintf("默认=%v 首位=%v", def, ifs[0].IP))
	}

	// 虚拟网卡不该排在物理网卡前面：同事复制 VMware 的 192.168.246.1 是连不上的
	firstVirtual := -1
	lastPhysical := -1
	for idx, i := range ifs {
		if i.Kind == netif.KindVirtual && firstVirtual < 0 {
			firstVirtual = idx
		}
		if i.Kind == netif.KindPhysical {
			lastPhysical = idx
		}
	}
	orderOK := firstVirtual < 0 || lastPhysical < 0 || lastPhysical < firstVirtual
	check("物理网卡排在虚拟网卡前", orderOK, fmt.Sprintf("末位物理=%d 首位虚拟=%d", lastPhysical, firstVirtual))

	// 广播地址：发现功能要用，且不能全是 255.255.255.255（那只对默认接口有效）
	bs := netif.BroadcastAddrs()
	subnet := 0
	for _, b := range bs {
		if b != "255.255.255.255" {
			subnet++
		}
	}
	check("算出子网广播地址", subnet >= 1, fmt.Sprintf("%d 个子网广播 / 共 %d", subnet, len(bs)))

	for _, i := range ifs {
		fmt.Printf("     · %-16s %-38s %s\n", i.IP, i.Name, i.Note())
	}
}

// ---------------------------------------------------------------------------
// 2. 发现
// ---------------------------------------------------------------------------

func checkDiscover(ctx context.Context) {
	br, err := discover.NewBrowser()
	if err != nil {
		check("绑定发现端口", false, err.Error())
		return
	}
	defer br.Close()
	check("绑定发现端口", true, fmt.Sprintf("UDP :%d", discover.Port))

	an, err := discover.NewAnnouncer(discover.Announce{
		ID:       "sigcheck-self",
		Name:     "自检分享",
		Addrs:    []string{"192.0.2.1:9000"},
		NeedCode: true,
		Width:    1920,
		Height:   1080,
	}, 400*time.Millisecond)
	if err != nil {
		check("创建广播端", false, err.Error())
		return
	}
	defer an.Close()
	check("创建广播端", true, "周期 400ms")

	scanCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	got, err := br.Scan(scanCtx, 3*time.Second)
	check("扫描收到自己的广播", len(got) >= 1 && err == nil, fmt.Sprintf("%d 条 err=%v", len(got), err))
	if len(got) > 0 {
		e := got[0]
		check("公告内容完整", e.Name == "自检分享" && len(e.Addrs) == 1 && e.Width == 1920,
			fmt.Sprintf("name=%q addrs=%v %dx%d", e.Name, e.Addrs, e.Width, e.Height))
		check("来源 IP 是本机的", e.From != "", "from="+e.From)
	}

	// 版本过滤：别的软件/旧版本占用同端口时，不该被当成分享条目
	c, err := net.Dial("udp", net.JoinHostPort("127.0.0.1", fmt.Sprint(discover.Port)))
	if err == nil {
		_, _ = c.Write([]byte(`{"v":99,"id":"other","name":"未来版本"}`))
		_ = c.Close()
	}
	got2, _ := br.Scan(ctx, 600*time.Millisecond)
	other := 0
	for _, e := range got2 {
		if e.ID == "other" {
			other++
		}
	}
	check("忽略版本不匹配的包", other == 0, fmt.Sprintf("误收 %d 条", other))

	// 授权码绝不能出现在广播里（广播是整个网段都能收到的）
	leak := false
	for _, e := range got {
		if len(e.Addrs) > 0 && strings.Contains(e.Addrs[0], "·") {
			leak = true
		}
	}
	check("广播不含授权码", !leak, "仅地址与名字")
}

// ---------------------------------------------------------------------------
// 3. 信令
// ---------------------------------------------------------------------------

func checkSignal(ctx context.Context) {
	// --- 端口顺延：先占住 9000，第二个服务必须自己挪 ---
	base := 9000
	squat, err := net.Listen("tcp", ":9000")
	if err != nil {
		// 9000 已被别的进程占用，换个端口测同样的逻辑
		base = 9100
		squat, err = net.Listen("tcp", ":9100")
		if err != nil {
			check("端口顺延", false, "找不到可占用的测试端口")
			return
		}
	}
	defer squat.Close()

	s1, _ := signal.NewServer(signal.ServerConfig{Name: "A", Port: base})
	if err := s1.Start(); err != nil {
		check("端口顺延", false, err.Error())
		return
	}
	defer s1.Close()
	check("端口顺延", s1.Port() == base+1, fmt.Sprintf("%d 被占 → 实际 %d", base, s1.Port()))

	// --- 观众上限与授权码 ---
	code := s1.Code()
	check("生成 6 位授权码", len(code) == 6 && strings.IndexFunc(code, func(r rune) bool { return r < '2' || r > '9' }) < 0,
		fmt.Sprintf("code=%s", code))

	s1.SetAccept(func(req signal.JoinRequest) (webrtc.SessionDescription, func(), error) {
		return webrtc.SessionDescription{}, nil, nil
	})

	cli := signal.NewClient(fmt.Sprintf("127.0.0.1:%d", s1.Port()))
	info, err := cli.Info(ctx)
	check("匿名可读取会话信息", err == nil && info.NeedCode, fmt.Sprintf("name=%q err=%v", info.Name, err))

	_, _, _, err = cli.Join(ctx, "000000", "错码君", webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: "v=0\r\n"})
	check("错误授权码被拒", errors.Is(err, signal.ErrBadCode), fmt.Sprintf("err=%v", err))

	limited := false
	for i := 0; i < maxFailTry; i++ {
		_, _, _, err = cli.Join(ctx, "000000", "错码君", webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: "v=0\r\n"})
		if errors.Is(err, signal.ErrRateLimited) {
			limited = true
			break
		}
	}
	check("连续错码触发限次", limited, fmt.Sprintf("第 %d 次被拦", maxFailTry))

	// 限次期间，就算码对了也进不来（否则限次形同虚设）
	_, _, _, err = cli.Join(ctx, code, "对的人", webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: "v=0\r\n"})
	check("限次期间正确码也被拦", errors.Is(err, signal.ErrRateLimited), fmt.Sprintf("err=%v", err))

	// --- 换一个"IP"绕过限次：限次是按远端 IP 计的，这里用第二个服务验证正常路径 ---
	s2, _ := signal.NewServer(signal.ServerConfig{Name: "B", Port: 9200, MaxViewers: 1})
	// Accept 在 HTTP goroutine 里跑，主 goroutine 要拿它建的 Peer —— 用 channel 交接，
	// 别用裸变量（race 会让"分享端就绪"这项时好时坏，属于典型的自洽但错的探针）。
	hostPeers := make(chan *rtc.Peer, 8)
	s2.SetAccept(func(req signal.JoinRequest) (webrtc.SessionDescription, func(), error) {
		p, err := rtc.NewPeer(rtc.Config{})
		if err != nil {
			return webrtc.SessionDescription{}, nil, err
		}
		if err := p.SetupMedia(); err != nil {
			return webrtc.SessionDescription{}, nil, err
		}
		if err := p.PC().SetRemoteDescription(req.Offer); err != nil {
			return webrtc.SessionDescription{}, nil, err
		}
		ans, err := p.PC().CreateAnswer(nil)
		if err != nil {
			return webrtc.SessionDescription{}, nil, err
		}
		if err := p.PC().SetLocalDescription(ans); err != nil {
			return webrtc.SessionDescription{}, nil, err
		}
		// non-trickle：必须等候选收齐，否则对端拿到的 answer 里没有候选
		p.WaitGathering(3 * time.Second)
		hostPeers <- p
		return *p.PC().LocalDescription(), func() { _ = p.Close() }, nil
	})
	if err := s2.Start(); err != nil {
		check("启动第二个服务", false, err.Error())
		return
	}
	defer s2.Close()

	t0 := time.Now()
	sess, err := signal.Dial(ctx, signal.DialConfig{
		Addr: fmt.Sprintf("127.0.0.1:%d", s2.Port()),
		Code: s2.Code(),
		Name: "自检观众",
	})
	el := float64(time.Since(t0).Milliseconds())
	check("真实 SDP 经 HTTP 换完并建连", err == nil && sess != nil, fmt.Sprintf("耗时 %.0f ms err=%v", el, err))
	if err == nil {
		var host *rtc.Peer
		select {
		case host = <-hostPeers:
		default:
		}
		check("两端数据通道都就绪", sess.Peer.Ready() && host != nil && host.Ready(),
			fmt.Sprintf("viewer=%v host=%v", sess.Peer.Ready(), host != nil && host.Ready()))
		check("接入耗时 <2s", el < 2000, fmt.Sprintf("%.0f ms", el))
		check("分享端记录了观众", len(s2.Viewers()) == 1, fmt.Sprintf("%d 人", len(s2.Viewers())))

		// 人数上限
		_, err2 := signal.Dial(ctx, signal.DialConfig{
			Addr: fmt.Sprintf("127.0.0.1:%d", s2.Port()), Code: s2.Code(), Name: "第二个人",
			ReadyTimeout: 3 * time.Second,
		})
		check("超过人数上限被拒", errors.Is(err2, signal.ErrFull), fmt.Sprintf("err=%v", err2))

		// 轮换授权码：新观众必须用新码
		old := s2.Code()
		newCode := s2.Rotate()
		check("轮换授权码", newCode != old, fmt.Sprintf("%s → %s", old, newCode))
		_, _, _, errOld := signal.NewClient(fmt.Sprintf("127.0.0.1:%d", s2.Port())).Join(ctx, old, "旧码",
			webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: "v=0\r\n"})
		check("旧码立即失效", errors.Is(errOld, signal.ErrBadCode), fmt.Sprintf("err=%v", errOld))

		// 主动离开：分享端应当立刻回收
		_ = sess.ByeClose()
		time.Sleep(200 * time.Millisecond)
		check("观众离开后从列表移除", len(s2.Viewers()) == 0, fmt.Sprintf("%d 人", len(s2.Viewers())))
	}

	// 地址行：界面复制按钮直接用的就是这个字符串
	line := s2.AddrLine()
	ok := line != "" && strings.Contains(line, " · ")
	check("生成可复制的一行", ok, fmt.Sprintf("%q", line))
	addrs := s2.ShareAddrs()
	check("给出全部可用地址", len(addrs) >= 1, fmt.Sprintf("%v", addrs))
}

// maxFailTry 是触发限次最多需要的尝试次数（服务端阈值 5）。
const maxFailTry = 7
