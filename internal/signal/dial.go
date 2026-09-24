package signal

import (
	"context"
	"errors"
	"fmt"
	"time"

	"goshare/internal/rtc"
)

// DialConfig 是观看端接入参数。
type DialConfig struct {
	// Addr 形如 "192.168.1.5:9000"。
	Addr string
	// Code 授权码。
	Code string
	// Name 观众显示名（分享端列表里能看到是谁）。
	Name string
	// Peer 是接收侧回调配置（OnFrame / OnState / OnControl）。
	Peer rtc.Config
	// GatherTimeout 等待本地候选收集的上限，0 用 3s。
	GatherTimeout time.Duration
	// ReadyTimeout 等待数据通道就绪的上限，0 用 20s。
	ReadyTimeout time.Duration
}

// Session 是一次成功的接入。
type Session struct {
	Client *Client
	Peer   *rtc.Peer
	Info   Info
	Token  string
	// ConnectMs 从发起到通道就绪的耗时（含 HTTP 握手 + ICE 建连）。
	ConnectMs float64
}

// Dial 完成观看端的完整接入：取信息 → 建 Peer → 生成 offer（等候选收集完）
// → HTTP 交换 → 设 answer → 等数据通道就绪。
//
// 用 non-trickle（一次性带齐候选）而不是 trickle：
// 内网 host candidate 收集实测 <100ms，换来的是"一个 POST 完成握手"，
// 少一整类"候选到达顺序"的竞态问题。
func Dial(ctx context.Context, cfg DialConfig) (*Session, error) {
	if cfg.GatherTimeout <= 0 {
		cfg.GatherTimeout = 3 * time.Second
	}
	if cfg.ReadyTimeout <= 0 {
		cfg.ReadyTimeout = 20 * time.Second
	}
	t0 := time.Now()

	cli := NewClient(cfg.Addr)
	info, err := cli.Info(ctx)
	if err != nil {
		return nil, err
	}

	peer, err := rtc.NewPeer(cfg.Peer)
	if err != nil {
		return nil, fmt.Errorf("创建连接失败：%w", err)
	}
	// 必须在 SetRemoteDescription 之前挂上：分享端创建通道后，OnDataChannel 是异步触发的。
	peer.WaitChannels()
	// 必须在 CreateOffer 之前：不建通道的话 offer 里没有 m-line 与 ice-ufrag，
	// 对端会直接拒收（详见 SetupViewer 的注释）。
	if err := peer.SetupViewer(); err != nil {
		_ = peer.Close()
		return nil, fmt.Errorf("创建通道失败：%w", err)
	}

	offer, err := peer.PC().CreateOffer(nil)
	if err != nil {
		_ = peer.Close()
		return nil, fmt.Errorf("生成 offer 失败：%w", err)
	}
	if err := peer.PC().SetLocalDescription(offer); err != nil {
		_ = peer.Close()
		return nil, fmt.Errorf("设置本地描述失败：%w", err)
	}
	// 等候选收齐再发：否则对端拿到的 offer 里没有候选，永远连不上。
	// 超时不致命（可能已经收到 host 候选），继续用现有 SDP 试。
	if !peer.WaitGathering(cfg.GatherTimeout) {
		// 这里不该静默：候选没收齐往往是 UDP 被拦的信号，但内网极少发生，
		// 先记在 Session 之外，由调用方决定是否上报。
		_ = errors.New("ice gathering timeout")
	}

	token, answer, hostName, err := cli.Join(ctx, cfg.Code, cfg.Name, *peer.PC().LocalDescription())
	if err != nil {
		_ = peer.Close()
		return nil, err
	}
	if hostName != "" {
		info.Name = hostName
	}
	if err := peer.PC().SetRemoteDescription(answer); err != nil {
		byeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = cli.Bye(byeCtx, token)
		cancel()
		_ = peer.Close()
		return nil, fmt.Errorf("设置远端描述失败：%w", err)
	}
	if err := peer.WaitReady(cfg.ReadyTimeout); err != nil {
		_ = peer.Close()
		return nil, fmt.Errorf("%w（地址与授权码正确，但媒体通道未建立 —— 检查防火墙是否放行 UDP）", err)
	}
	return &Session{
		Client:    cli,
		Peer:      peer,
		Info:      info,
		Token:     token,
		ConnectMs: float64(time.Since(t0).Milliseconds()),
	}, nil
}

// ByeClose 关闭会话并通知分享端。
//
// 超时压到 1s：调用方常是在"窗口已关、2 秒后就要硬退出"的窗口里执行它，
// 卡久了会被兜底退出抢跑，Bye 就发不出去了。
func (s *Session) ByeClose() error {
	if s == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = s.Client.Bye(ctx, s.Token)
	return s.Peer.Close()
}
