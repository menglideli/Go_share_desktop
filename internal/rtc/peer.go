// Package rtc 封装 pion/webrtc，提供屏幕共享所需的两个通道：
//
//	media —— 不可靠、无序（MaxRetransmits=0）：传帧数据。丢一片就丢这一帧的对应区域，
//	         绝不重传阻塞；MJPEG 每帧独立，下一帧会自然补上。
//	ctl   —— 可靠、有序：传控制消息（请求关键帧、分辨率通知、统计）。
//
// 为什么不用一条可靠通道：可靠 + 有序在丢包时会让后续帧全部排队等重传，
// 延迟瞬间飙到数百毫秒 —— 这正是要避免的。代价是丢失的条带要靠 PLI 补。
package rtc

import (
	"encoding/binary"
	"errors"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"goshare/internal/codec"
)

// 分片大小。阶段 0 实测单条 64KB 消息可行，但那是上限边界，
// 留出余量用 60KB，避免触碰 SCTP 消息大小限制。
const (
	chunkPayload   = 60 * 1024
	chunkHeaderLen = 8
	// 背压阈值：媒体通道积压超过它就丢弃这一帧，防止弱网观众把自己拖死。
	// 30fps × 2K 全量约 2MB/帧，积压 8MB ≈ 落后 4 帧，已经没有追赶价值。
	maxBuffered = 8 << 20
)

// ErrBackpressure 表示发送缓冲积压过多，该帧被主动丢弃。
var ErrBackpressure = errors.New("rtc: 发送缓冲积压，丢弃该帧")

// ErrNotOpen 表示通道尚未就绪。
var ErrNotOpen = errors.New("rtc: 数据通道未打开")

// Stats 是传输侧统计。
type Stats struct {
	FramesSent   uint64
	FramesRecv   uint64
	FramesDrop   uint64 // 因背压丢弃
	FragLost     uint64 // 分片不完整被丢弃的帧
	BytesSent    uint64
	BytesRecv    uint64
	Buffered     uint64
}

// Config 创建 Peer 的参数。
type Config struct {
	// ICEServers 为空时纯内网直连（不需要 STUN/TURN）。
	ICEServers []webrtc.ICEServer
	// OnFrame 收到完整一帧时回调（在 pion 的读 goroutine 中调用，别做重活）。
	OnFrame func(f *codec.Frame)
	// OnControl 收到控制消息时回调。
	OnControl func(t CtlType, payload []byte)
	// OnState 连接状态变化。
	OnState func(state webrtc.ICEConnectionState)
}

// Peer 封装一个 PeerConnection 及两条数据通道。
type Peer struct {
	cfg Config
	pc   *webrtc.PeerConnection
	media *webrtc.DataChannel
	ctl   *webrtc.DataChannel

	mu    sync.Mutex
	stats Stats
	asm   map[uint32]*frameBuf
}

// NewPeer 创建 Peer。
func NewPeer(cfg Config) (*Peer, error) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: cfg.ICEServers,
	})
	if err != nil {
		return nil, err
	}
	p := &Peer{cfg: cfg, pc: pc, asm: map[uint32]*frameBuf{}}
	if cfg.OnState != nil {
		pc.OnICEConnectionStateChange(cfg.OnState)
	}
	return p, nil
}

// PC 暴露底层 PeerConnection，供信令层使用。
func (p *Peer) PC() *webrtc.PeerConnection { return p.pc }

// OnICECandidate 注册候选回调。
func (p *Peer) OnICECandidate(f func(*webrtc.ICECandidate)) { p.pc.OnICECandidate(f) }

// AddICECandidate 添加对端候选。
func (p *Peer) AddICECandidate(c webrtc.ICECandidateInit) error {
	return p.pc.AddICECandidate(c)
}

// Close 关闭连接。
func (p *Peer) Close() error {
	if p.pc == nil {
		return nil
	}
	return p.pc.Close()
}

// SetupMedia 由分享端调用：创建 media 与 ctl 两条通道。
func (p *Peer) SetupMedia() error {
	ordered := false
	m, err := p.pc.CreateDataChannel("media", &webrtc.DataChannelInit{
		Ordered:        &ordered,
		MaxRetransmits: func() *uint16 { v := uint16(0); return &v }(),
	})
	if err != nil {
		return err
	}
	c, err := p.pc.CreateDataChannel("ctl", nil) // 默认可靠有序
	if err != nil {
		return err
	}
	p.attach(m, c)
	return nil
}

// attach 绑定通道回调。观看端由 OnDataChannel 触发。
func (p *Peer) attach(media, ctl *webrtc.DataChannel) {
	p.media = media
	p.ctl = ctl
	media.OnMessage(func(msg webrtc.DataChannelMessage) {
		p.onMedia(msg.Data)
	})
	ctl.OnMessage(func(msg webrtc.DataChannelMessage) {
		if len(msg.Data) < 1 || p.cfg.OnControl == nil {
			return
		}
		p.cfg.OnControl(CtlType(msg.Data[0]), msg.Data[1:])
	})
}

// WaitChannels 由观看端调用：等待分享端创建的两条通道。
func (p *Peer) WaitChannels() {
	opened := make(chan struct{}, 2)
	p.pc.OnDataChannel(func(d *webrtc.DataChannel) {
		switch d.Label() {
		case "media":
			d.OnMessage(func(msg webrtc.DataChannelMessage) { p.onMedia(msg.Data) })
			p.mu.Lock()
			p.media = d
			p.mu.Unlock()
			d.OnOpen(func() { opened <- struct{}{} })
		case "ctl":
			d.OnMessage(func(msg webrtc.DataChannelMessage) {
				if len(msg.Data) < 1 || p.cfg.OnControl == nil {
					return
				}
				p.cfg.OnControl(CtlType(msg.Data[0]), msg.Data[1:])
			})
			p.mu.Lock()
			p.ctl = d
			p.mu.Unlock()
			d.OnOpen(func() { opened <- struct{}{} })
		}
	})
}

// Ready 报告媒体通道是否可用。
func (p *Peer) Ready() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.media != nil && p.media.ReadyState() == webrtc.DataChannelStateOpen
}

// WaitReady 等待媒体通道就绪（观看端由 OnDataChannel 异步建立，需要等）。
func (p *Peer) WaitReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if p.Ready() {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return errors.New("rtc: 等待媒体通道就绪超时")
}

// SendFrame 编码并发送一帧。buf 会被切成 ≤60KB 的分片。
func (p *Peer) SendFrame(f *codec.Frame) error {
	p.mu.Lock()
	m := p.media
	p.mu.Unlock()
	if m == nil {
		return ErrNotOpen
	}
	if m.ReadyState() != webrtc.DataChannelStateOpen {
		return ErrNotOpen
	}
	// 背压：宁可丢帧也不让队列无限增长（否则延迟会越拖越大）
	if m.BufferedAmount() > maxBuffered {
		p.mu.Lock()
		p.stats.FramesDrop++
		p.mu.Unlock()
		return ErrBackpressure
	}
	data, err := f.Marshal()
	if err != nil {
		return err
	}
	total := (len(data) + chunkPayload - 1) / chunkPayload
	if total > 65535 {
		return errors.New("rtc: 帧过大，超出分片上限")
	}
	seq := uint32(f.Seq)
	sent := 0
	for i := 0; i < total; i++ {
		lo := i * chunkPayload
		hi := lo + chunkPayload
		if hi > len(data) {
			hi = len(data)
		}
		msg := make([]byte, chunkHeaderLen+hi-lo)
		binary.BigEndian.PutUint32(msg[0:4], seq)
		binary.BigEndian.PutUint16(msg[4:6], uint16(i))
		binary.BigEndian.PutUint16(msg[6:8], uint16(total))
		copy(msg[chunkHeaderLen:], data[lo:hi])
		if err := m.Send(msg); err != nil {
			return err
		}
		sent += len(msg)
	}
	p.mu.Lock()
	p.stats.FramesSent++
	p.stats.BytesSent += uint64(sent)
	p.mu.Unlock()
	return nil
}

// ---------------------------------------------------------------------------
// 控制消息
// ---------------------------------------------------------------------------

// CtlType 是控制消息类型。
type CtlType byte

const (
	// CtlPLI 观众请求关键帧（分片丢失后用于快速恢复）。
	CtlPLI CtlType = 1
	// CtlHello 观众上报能力（窗口尺寸、DPI 等），payload 为 JSON。
	CtlHello CtlType = 2
	// CtlStats 观众回传统计，payload 为 JSON。
	CtlStats CtlType = 3
	// CtlBye 主动断开。
	CtlBye CtlType = 4
)

// SendControl 发送控制消息（可靠通道）。
func (p *Peer) SendControl(t CtlType, payload []byte) error {
	p.mu.Lock()
	c := p.ctl
	p.mu.Unlock()
	if c == nil || c.ReadyState() != webrtc.DataChannelStateOpen {
		return ErrNotOpen
	}
	msg := make([]byte, 1+len(payload))
	msg[0] = byte(t)
	copy(msg[1:], payload)
	return c.Send(msg)
}

// ---------------------------------------------------------------------------
// 接收与重组
// ---------------------------------------------------------------------------

type frameBuf struct {
	total uint16
	have  uint16
	parts [][]byte
	first time.Time
}

func (p *Peer) onMedia(data []byte) {
	if len(data) < chunkHeaderLen {
		return
	}
	seq := binary.BigEndian.Uint32(data[0:4])
	idx := binary.BigEndian.Uint16(data[4:6])
	total := binary.BigEndian.Uint16(data[6:8])
	if total == 0 {
		return
	}

	p.mu.Lock()
	fb, ok := p.asm[seq]
	if !ok {
		// 清理陈旧重组槽。
		//
		// ⚠️ 这里曾经"超过 8 个就随机删一个"，结果把正在重组的活跃帧也删了，
		// 表现为莫名的丢帧（2K 掉 1 帧、增量掉 2 帧）。不可靠无序通道下分片
		// 会乱序抵达，同时活跃的帧数可能远超预期 —— 只能按超时清理，不能按数量砍。
		if len(p.asm) > 32 {
			for k, v := range p.asm {
				if time.Since(v.first) > 1500*time.Millisecond {
					delete(p.asm, k)
					// 分片始终没凑齐：不可靠通道下的预期行为，记一笔供观众端触发 PLI
					p.stats.FragLost++
				}
			}
		}
		fb = &frameBuf{total: total, parts: make([][]byte, total), first: time.Now()}
		p.asm[seq] = fb
	}
	if idx >= fb.total || fb.parts[idx] != nil {
		p.mu.Unlock()
		return
	}
	part := make([]byte, len(data)-chunkHeaderLen)
	copy(part, data[chunkHeaderLen:])
	fb.parts[idx] = part
	fb.have++
	p.stats.BytesRecv += uint64(len(part))
	if fb.have < fb.total {
		p.mu.Unlock()
		return
	}
	// 收齐：拼回完整帧
	n := 0
	for _, q := range fb.parts {
		n += len(q)
	}
	full := make([]byte, n)
	off := 0
	for _, q := range fb.parts {
		copy(full[off:], q)
		off += len(q)
	}
	delete(p.asm, seq)
	p.mu.Unlock()

	f, err := codec.UnmarshalFrame(full)
	if err != nil {
		p.mu.Lock()
		p.stats.FragLost++
		p.mu.Unlock()
		return
	}
	p.mu.Lock()
	p.stats.FramesRecv++
	p.mu.Unlock()
	if p.cfg.OnFrame != nil {
		p.cfg.OnFrame(f)
	}
}

// Stats 返回当前统计快照。
func (p *Peer) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.stats
	if p.media != nil {
		s.Buffered = uint64(p.media.BufferedAmount())
	}
	return s
}
