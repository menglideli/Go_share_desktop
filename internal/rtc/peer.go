// Package rtc 封装 pion/webrtc，提供屏幕共享所需的两个通道：
//
//	media —— 不可靠、无序（MaxRetransmits=0）：传画面分块。每个线路消息就是一个
//	         **可独立解析的分块**（自带帧头 + 完整条带，见 codec 的线格式说明），
//	         所以丢一块只丢它带的那几条带 —— 不重传、不阻塞、不等重传。
//	ctl   —— 可靠、有序：传控制消息（请求修复、统计等）。
//
// 为什么不用一条可靠通道：可靠 + 有序在丢包时会让后续数据全部排队等重传，
// 延迟瞬间飙到数百毫秒 —— 这正是要避免的。代价是丢的那几条带要靠修复轮次补
// （分享端把补齐整屏摊到十几帧里，而不是发一发 2MB 的全量帧）。
//
// ⚠️ 这里**没有重组层**：旧实现要求把同一帧的所有分块凑齐才能解析，
// 于是丢任意一块 = 整帧 2MB 作废（2K 一帧 35 块），观众只能再要一整帧全量 ——
// 拥塞时"越要越堵"。现在收到一块解一块，丢一块只影响它自己那几条带。
package rtc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
	"goshare/internal/codec"
)

// 分块大小上限。分块的**切法**由 codec.Frame.Split 决定（保证条带不跨块），
// 这里只是取同一个常量，避免两处各写一个数字。
const chunkPayload = codec.MaxChunkPayload

// 背压阈值：媒体通道积压超过它（按当前帧大小算出来，见 SendLimit）就丢弃这一帧，
// 防止弱网观众把自己拖死。
//
// ⚠️ 这里曾经是固定 8MB。8MB ≈ 4 个 2K 全量帧 —— 等于允许落后 4 帧才丢，
// 而这个通道是"不可靠+无序"的：积压的字节要等 SCTP 发完才轮到新帧，
// 光是这个阈值就自带几百毫秒延迟（用户感受就是"卡的时候延迟很长"）。
// 现在按帧大小自适应，目标只有"约一帧半"。
const (
	minBuffered = 512 << 10
	maxBuffered = 4 << 20
)

// SendLimit 返回"当前允许积压多少字节"，按这一帧的大小自适应。
//
// 语义：超过它就整帧丢弃（下一帧顶上）。丢一帧的代价是那几块条带暂时没更新，
// 而拖着的代价是**整条流都变慢**——两害相权取其轻。
func SendLimit(frameBytes int) uint64 {
	lim := uint64(frameBytes) * 3 / 2
	if lim < minBuffered {
		lim = minBuffered
	}
	if lim > maxBuffered {
		lim = maxBuffered
	}
	return lim
}

// ErrBackpressure 表示发送缓冲积压过多，该帧被主动丢弃。
var ErrBackpressure = errors.New("rtc: 发送缓冲积压，丢弃该帧")

// ErrNotOpen 表示通道尚未就绪。
var ErrNotOpen = errors.New("rtc: 数据通道未打开")

// Stats 是传输侧统计。
type Stats struct {
	FramesSent uint64
	FramesRecv uint64
	FramesDrop uint64 // 发送侧因背压丢弃
	ChunksSent uint64
	ChunksRecv uint64
	ChunksSkip uint64 // 接收侧因解码跟不上丢弃的分块（AsyncFrames）
	// BadChunks 是解析失败的分块（魔数不对 / 长度越界）。
	// ⚠️ 它**不再**表示"分块丢了"：现在丢一块只会让观众端少几条带，
	// 由 codec.Decoder 的完整性位图判定（见 Decoder.Complete）。
	BadChunks uint64
	BytesSent uint64
	BytesRecv uint64
	Buffered  uint64
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
	// UDPPortMin / UDPPortMax 限定本地 UDP 端口范围。
	//
	// 为什么需要：媒体面用的是临时端口，随机的话没法写防火墙规则 ——
	// 同事那台开着 Domain 防火墙，入站 UDP 一拦就是"地址没错但连不上"。
	// 固定区间后，放行规则可以写成一条固定命令（见 sigdemo 启动时打印的 netsh 指引）。
	// 都为 0 表示用 pion 默认（49152~65535）。
	UDPPortMin uint16
	UDPPortMax uint16

	// AsyncFrames 让 OnFrame/OnFrames 在**独立 goroutine** 里执行（观看端开启）。
	//
	// 为什么：pion 的 OnMessage 回调跑在 SCTP 的读 goroutine 上。解码（2K 约
	// 10~20ms）+ 逐像素统计 + 画面拷贝如果都做在这里，读路径就被我们自己的解码
	// 堵住 —— 帧在接收缓冲里排队，延迟单调增长，**连 ctl 通道（PLI）一起被堵**，
	// 于是丢包后连"请求修复"都发不及时。
	// 开启后：读 goroutine 只把裸分块塞进一个小邮箱（满了覆盖最旧的），
	// 解析与解码在独立 goroutine 上跑，慢了自己丢块、绝不排队。
	AsyncFrames bool
	// OnFrames 是 AsyncFrames 下的**批量**回调：一次给若干块。
	//
	// 为什么要批量：单块通常只带 1~2 个条带，而 JPEG 解码一条带就要几毫秒 ——
	// 逐块串行解码等于把帧内并行度全丢掉（实测 2K 全量 7ms → 98ms）。
	// 把邮箱里攒着的几块一起交出去，调用方就能用一个并行池把它们的条带一起解。
	// 设置了 OnFrames 时优先用它；只设 OnFrame 则逐块回调。
	OnFrames func(frames []*codec.Frame)
	// OnFrameDrop 在"解码跟不上、丢掉一帧"时回调。
	//
	// 在读 goroutine 里调用，必须立刻返回（一般只是置一个原子标志）。
	// 语义很重要：丢帧等于那几块条带的更新永远没了（增量帧不会重发），
	// 调用方必须据此把画布标记为不完整并请求全量帧，否则就是 R32 那类
	// "永久缺一块"。不设则丢帧完全静默。
	OnFrameDrop func()
}

// Peer 封装一个 PeerConnection 及两条数据通道。
type Peer struct {
	cfg      Config
	pc       *webrtc.PeerConnection
	media    *webrtc.DataChannel
	ctl      *webrtc.DataChannel
	viewerCh *webrtc.DataChannel // 观看端为协商 m-line 而建，见 SetupViewer

	// frameIn / frameDone 是 AsyncFrames 的单槽邮箱与停止信号。
	frameIn   chan []byte
	frameDone chan struct{}
	stopOnce  atomic.Bool

	mu    sync.Mutex
	stats Stats
	// lastSeq 用于把"分块计数"折算成"逻辑帧计数"（见 deliver）。
	lastSeq uint64
}

// NewPeer 创建 Peer。
func NewPeer(cfg Config) (*Peer, error) {
	api := webrtc.NewAPI()
	if cfg.UDPPortMax > cfg.UDPPortMin && cfg.UDPPortMin > 0 {
		var se webrtc.SettingEngine
		if err := se.SetEphemeralUDPPortRange(cfg.UDPPortMin, cfg.UDPPortMax); err != nil {
			return nil, fmt.Errorf("rtc: 设置 UDP 端口范围失败：%w", err)
		}
		api = webrtc.NewAPI(webrtc.WithSettingEngine(se))
	}
	pc, err := api.NewPeerConnection(webrtc.Configuration{
		ICEServers: cfg.ICEServers,
	})
	if err != nil {
		return nil, err
	}
	p := &Peer{cfg: cfg, pc: pc}
	if cfg.OnState != nil {
		pc.OnICEConnectionStateChange(cfg.OnState)
	}
	if cfg.AsyncFrames {
		// 邮箱深度见 chunkBatchMax：够攒一批做并行解码，又不至于压出延迟。
		p.frameIn = make(chan []byte, chunkBatchMax)
		p.frameDone = make(chan struct{})
		go p.frameWorker()
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

// WaitGathering 等待本地候选收集完成，non-trickle 信令（HTTP 一次换完 SDP）需要它。
//
// ⚠️ 必须在 SetLocalDescription **之前或紧随其后**调用：pion 的 GatheringCompletePromise
// 在收集已完成时会立即关闭 channel，所以就算晚一步也不会永久阻塞，
// 但晚太久会白等一个超时。返回 false 表示超时（不是故障，仍可带着已有候选继续）。
func (p *Peer) WaitGathering(timeout time.Duration) bool {
	done := webrtc.GatheringCompletePromise(p.pc)
	if timeout <= 0 {
		<-done
		return true
	}
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// Close 关闭连接。
func (p *Peer) Close() error {
	// 先停解码 goroutine：它对 OnFrame 的调用属于"本对象还在用"，
	// 关完连接再回调容易把上层状态改乱。
	if p.frameDone != nil && p.stopOnce.CompareAndSwap(false, true) {
		close(p.frameDone)
	}
	if p.pc == nil {
		return nil
	}
	return p.pc.Close()
}

// frameWorker 是 AsyncFrames 的解析+解码 goroutine：把邮箱里攒着的块**成批**取走，
// 读 goroutine 从不在这里等它（详见 Config.AsyncFrames / OnFrames）。
func (p *Peer) frameWorker() {
	batch := make([][]byte, 0, chunkBatchMax)
	for {
		var first []byte
		select {
		case <-p.frameDone:
			return
		case first = <-p.frameIn:
		}
		batch = append(batch[:0], first)
		// 把邮箱里已经压着的都带上：单块只带 1~2 个条带，
		// 攒一批才有足够的并行度（见 Config.OnFrames）。
	drain:
		for len(batch) < chunkBatchMax {
			select {
			case d := <-p.frameIn:
				batch = append(batch, d)
			default:
				break drain
			}
		}
		p.deliverBatch(batch)
	}
}

// deliverBatch 解析一批分块并回调（同步路径与异步路径共用）。
//
// FramesRecv 按"逻辑帧"计数（同一 seq 的多个分块只算一次），
// 便于和分享端的发送帧数对齐；分块数另计 ChunksRecv。
func (p *Peer) deliverBatch(batch [][]byte) {
	if p.cfg.OnFrames == nil {
		for _, data := range batch {
			p.deliver(data)
		}
		return
	}
	frames := make([]*codec.Frame, 0, len(batch))
	for _, data := range batch {
		f, err := codec.UnmarshalFrame(data)
		if err != nil {
			p.mu.Lock()
			p.stats.BadChunks++
			p.mu.Unlock()
			continue
		}
		p.mu.Lock()
		p.stats.ChunksRecv++
		if f.Seq != p.lastSeq {
			p.lastSeq = f.Seq
			p.stats.FramesRecv++
		}
		p.mu.Unlock()
		frames = append(frames, f)
	}
	if len(frames) > 0 {
		p.cfg.OnFrames(frames)
	}
}

// deliver 解析**一个分块**并回调 OnFrame（单块路径，工具用）。
func (p *Peer) deliver(data []byte) {
	f, err := codec.UnmarshalFrame(data)
	if err != nil {
		p.mu.Lock()
		p.stats.BadChunks++
		p.mu.Unlock()
		return
	}
	p.mu.Lock()
	p.stats.ChunksRecv++
	if f.Seq != p.lastSeq {
		p.lastSeq = f.Seq
		p.stats.FramesRecv++
	}
	p.mu.Unlock()
	if p.cfg.OnFrame != nil {
		p.cfg.OnFrame(f)
	}
}

// Buffered 返回媒体通道当前积压字节数。
//
// 扇出侧用它做"水位"判据：积压接近 SendLimit 就整帧跳过，
// 而不是等发送接口内部的阈值兜底（那时已经多排了一帧）。
func (p *Peer) Buffered() uint64 {
	p.mu.Lock()
	m := p.media
	p.mu.Unlock()
	if m == nil {
		return 0
	}
	return uint64(m.BufferedAmount())
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

// SetupViewer 由观看端在 CreateOffer **之前**调用：创建一条通道，
// 让 SDP 里出现 m=application 段。
//
// ⚠️ 这不是可选动作。pion 只在 PeerConnection 上有 data channel 或 transceiver 时
// 才生成 m-line；观看端的 media/ctl 都是**对端**创建的、要等 OnDataChannel 才有，
// 于是不建通道就 CreateOffer 会得到一份 232 字节、连 ice-ufrag 都没有的 SDP ——
// 对端 SetRemoteDescription 直接报 "called with no ice-ufrag"（实测踩过）。
// 这条通道本身不传数据，纯粹为了把 application 段协商出来。
func (p *Peer) SetupViewer() error {
	d, err := p.pc.CreateDataChannel("viewer", nil) // 可靠有序，无需回调
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.viewerCh = d
	p.mu.Unlock()
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

// chunkBatchMax 是"攒一批"的上限（块）。
//
// 取 4：太小则并行度不够（单块只有 1~2 个条带），太大则邮箱里会压出额外延迟
// （4 块 × 60KB 上限，最坏 ~10ms 的处理延迟，超出的老块会被覆盖丢弃 —— 最新优先）。
const chunkBatchMax = 4

// SendFrame 把一帧切成若干分块并发送（工具/测试用；生产走 SendChunks 共享缓存）。
func (p *Peer) SendFrame(f *codec.Frame) error {
	chunks := f.Split(codec.MaxChunkPayload)
	wire := make([][]byte, 0, len(chunks))
	for _, c := range chunks {
		data, err := c.Marshal()
		if err != nil {
			return err
		}
		wire = append(wire, data)
	}
	return p.SendChunks(wire)
}

// SendChunks 发送**已序列化**的若干分块，每个分块一条消息。
//
// ⚠️ 这里不做分片/重组：分块是编码器按"自带完整帧头 + 完整条带"切好的
// （见 codec.Frame.Split），对端收到一块就能解一块。所以本函数就是"逐条发"，
// 没有缓冲区、没有等待、没有超时清理 —— 丢一块只丢它带的条带。
//
// 为什么参数是"已序列化的分块列表"：多观众扇出时 Marshal 是一帧里最大的拷贝
// （2K 全量约 2MB），每个观众各 Marshal 一次会全部串在采集 goroutine 上 ——
// 4 人时全员 fps 腰斩到 15。管线只 Marshal 一次放进共享缓存，
// 每个观众的发送 goroutine 各拿同一份数据自己发（见 pipeline.Sharer 的扇出）。
func (p *Peer) SendChunks(chunks [][]byte) error {
	p.mu.Lock()
	m := p.media
	p.mu.Unlock()
	if m == nil {
		return ErrNotOpen
	}
	if m.ReadyState() != webrtc.DataChannelStateOpen {
		return ErrNotOpen
	}
	total := 0
	for _, c := range chunks {
		total += len(c)
	}
	// 背压：宁可丢帧也不让队列无限增长（否则延迟会越拖越大）。
	// 阈值按这一帧的大小算（见 SendLimit）——固定 8MB 等于允许落后 4 帧。
	if lim := SendLimit(total); m.BufferedAmount() > lim {
		p.mu.Lock()
		p.stats.FramesDrop++
		p.mu.Unlock()
		return ErrBackpressure
	}
	for _, c := range chunks {
		if err := m.Send(c); err != nil {
			return err
		}
	}
	p.mu.Lock()
	p.stats.FramesSent++
	p.stats.ChunksSent += uint64(len(chunks))
	p.stats.BytesSent += uint64(total)
	p.mu.Unlock()
	return nil
}

// ---------------------------------------------------------------------------
// 控制消息
// ---------------------------------------------------------------------------

// CtlType 是控制消息类型。
type CtlType byte

const (
	// CtlPLI 观众请求"开一轮渐进式修复"（发现画布不完整时用：丢块、丢帧、刚接入）。
	// 名字沿用 RTP 的 Picture Loss Indication，语义是"我这儿的画面缺东西了"。
	CtlPLI CtlType = 1
	// CtlHello 观众上报能力（窗口尺寸、DPI 等），payload 为 JSON。
	CtlHello CtlType = 2
	// CtlStats 观众回传统计，payload 为 JSON。
	CtlStats CtlType = 3
	// CtlBye 主动断开。
	CtlBye CtlType = 4
	// CtlMissing 观众上报"画布上内容已过期的条带位图"，请分享端**只补这几条**。
	//
	// 为什么要有它：光靠 CtlPLI（开一轮渐进式修复）意味着"缺 2 条带也要把
	// 整屏 17 条带轮着发一遍"（十几帧、几百 KB）。观众端本来就知道自己缺哪几条
	// （codec.Decoder.Missing），报上来就能一帧补齐、只花几十 KB。
	// payload = 8 字节大端位图，编码与 Frame.TilesMask 一致。
	CtlMissing CtlType = 5
)

// MissingPayload 把条带位图编码成 CtlMissing 的 payload。
func MissingPayload(mask uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, mask)
	return b
}

// ParseMissing 解析 CtlMissing 的 payload。
func ParseMissing(payload []byte) (uint64, bool) {
	if len(payload) < 8 {
		return 0, false
	}
	return binary.BigEndian.Uint64(payload[:8]), true
}

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
// 接收
// ---------------------------------------------------------------------------
//
// 这里**没有重组层**：每个线路消息就是一个可独立解析的分块（自带帧头 + 完整条带）。
// 旧实现要把同一帧的所有分块凑齐才解析得出来，于是丢任意一块 = 整帧 2MB 作废
// （2K 一帧 35 块，丢 1 块的概率不低），观众端只能发 PLI 再要一整帧 ——
// 拥塞时越要越堵。现在丢一块只丢它带的那几条带，其余照常上屏。

func (p *Peer) onMedia(data []byte) {
	p.mu.Lock()
	p.stats.BytesRecv += uint64(len(data))
	p.mu.Unlock()

	// 异步路径（观看端）：读 goroutine 只把**裸分块**塞进单槽邮箱，
	// 解析与解码都在独立 goroutine 上（见 Config.AsyncFrames）。
	// 单槽覆盖式：邮箱里已经压着一块就丢掉更旧的那块、换成最新的 ——
	// 慢的解码器只丢块，绝不让 SCTP 接收缓冲堆积（那才是"卡起来延迟很长"的根因）。
	if p.frameIn != nil {
		select {
		case p.frameIn <- data:
			return
		default:
		}
		select {
		case <-p.frameIn: // 丢掉更旧的那块
		default:
		}
		select {
		case p.frameIn <- data:
		default:
		}
		p.mu.Lock()
		p.stats.ChunksSkip++
		p.mu.Unlock()
		// 丢掉一块 = 它带的条带内容过期了（增量帧不会重发），
		// 必须让调用方按完整性位图去要一轮修复。
		if p.cfg.OnFrameDrop != nil {
			p.cfg.OnFrameDrop()
		}
		return
	}

	p.deliver(data)
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
