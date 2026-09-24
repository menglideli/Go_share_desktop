// Package pipeline 把 采集 → 编码 → 扇出 串成一条可运行的分享管线。
//
// 关键设计（都是踩过坑后定的）：
//  1. 发送分辨率与观看端窗口完全解耦：拖窗口只影响观众本地 GPU 缩放，
//     绝不会回头触发编码器重建。
//  2. 每条观众连接独立背压：弱网观众自己丢帧，不拖慢其他人。
//  3. 桌面静止时 DXGI 不出帧是预期行为，靠心跳保活而不是靠"提高采集频率"。
package pipeline

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"goshare/internal/capture"
	"goshare/internal/codec"
	"goshare/internal/rtc"
)

// Preset 是质量/帧率档位。
//
// 帧率语义：MaxFPS=0 表示"最大"——不主动限帧，出帧节奏由桌面变化率与编码能力决定。
// DXGI 本身是变化驱动的（桌面不动就不出帧），所以"最大"不会空转烧 CPU。
type Preset struct {
	Name    string
	Quality int
	Tiles   int
	Dirty   bool
	MaxFPS  int // 0 = 不限（最大）
}

var (
	// PresetMax 是默认档：不限帧率，画质优先。
	PresetMax = Preset{Name: "最大", Quality: 75, Tiles: 0, Dirty: true, MaxFPS: 0}
	// Preset60 上限 60fps。
	Preset60 = Preset{Name: "流畅60", Quality: 75, Tiles: 0, Dirty: true, MaxFPS: 60}
	// Preset30 上限 30fps，最省带宽，适合人数多或弱网。
	Preset30 = Preset{Name: "流畅30", Quality: 80, Tiles: 0, Dirty: true, MaxFPS: 30}
	// PresetTurbo 急速：更多条带带来更细的增量粒度，略降质量换最低延迟。
	PresetTurbo = Preset{Name: "急速", Quality: 65, Tiles: 40, Dirty: true, MaxFPS: 0}
)

// Presets 是可选档位顺序（UI 直接遍历即可）。
var Presets = []Preset{PresetMax, Preset60, Preset30, PresetTurbo}

// PresetByName 按名字取档位。
func PresetByName(name string) Preset {
	for _, p := range Presets {
		if p.Name == name {
			return p
		}
	}
	return PresetMax
}

// Config 是分享管线配置。
type Config struct {
	Display capture.Display
	Region  capture.Rect
	FPS     int // 采集目标帧率（库参数），0 用默认 30
	Preset  Preset
	Cursor  bool
	// Warmup 是"丢弃首帧黑帧"的超时上限（R25）。
	// 0 表示用默认 600ms；负数表示禁用预热（排障对比时有用）。
	Warmup time.Duration
	// IdleHeartbeat 是桌面静止时的保活间隔。0 表示用默认 2s。
	IdleHeartbeat time.Duration
	// OnFrame 每编码完一帧回调（诊断/预览用），可为 nil。
	OnFrame func(f *codec.Frame, src capture.Frame)
}

func (c Config) withDefaults() Config {
	if c.FPS <= 0 {
		c.FPS = 30
	}
	if c.IdleHeartbeat <= 0 {
		c.IdleHeartbeat = 2 * time.Second
	}
	return c
}

// Stats 是管线运行统计。
type Stats struct {
	Frames    uint64
	Bytes     uint64
	DropIdle  uint64 // 因帧率限制跳过的帧
	Heartbeat uint64
	EncodeMs  float64 // 均值
	capture.Stats
}

// Sharer 是一条分享管线：单采集、单编码、扇出给多个观众。
type Sharer struct {
	cfg Config
	src *capture.Source
	enc *codec.Encoder

	mu    sync.Mutex
	peers map[*rtc.Peer]struct{}
	stats Stats
	// wantKey 表示下一位观众（或全部观众）需要全量帧
	wantKey bool
	// paused 为 true 时不再发送真实画面，改发纯黑帧（见 SetPaused）。
	paused bool
	// blackStreak 是连续"全黑采样"的次数，用来识别"采集源其实取不到画面"
	// （锁屏 / UAC 安全桌面 / 采集后端异常）。见 BlackScreen。
	blackStreak atomic.Int32
	// sampleTick 控制抽样频率（每帧都统计太浪费）。
	sampleTick int
	// black 是暂停用的纯黑 BGRA 缓冲，尺寸变化时重建。
	// 只有 Run 那个 goroutine 会碰它，不需要锁。
	black []byte
}

// NewSharer 启动采集并创建分享管线。
func NewSharer(ctx context.Context, cfg Config) (*Sharer, error) {
	cfg = cfg.withDefaults()
	src, err := capture.NewSource(ctx, capture.Options{
		Display: cfg.Display,
		Region:  cfg.Region,
		FPS:     cfg.FPS,
		Cursor:  cfg.Cursor,
	})
	if err != nil {
		return nil, err
	}
	// 预热：DXGI 建流后的首帧常是未初始化的黑帧（实测 3 次里 2 次）。
	// 在这里丢掉它，观众接入时第一眼就是真画面，而不是"闪一下黑"。
	if cfg.Warmup >= 0 {
		_, _ = src.Warmup(ctx, 3, cfg.Warmup)
	}
	return &Sharer{
		cfg:   cfg,
		src:   src,
		enc:   codec.NewEncoder(codec.Config{Quality: cfg.Preset.Quality, Tiles: cfg.Preset.Tiles, Dirty: cfg.Preset.Dirty}),
		peers: map[*rtc.Peer]struct{}{},
	}, nil
}

// Source 返回底层采集源。
func (s *Sharer) Source() *capture.Source { return s.src }

// Close 停止采集。
func (s *Sharer) Close() error { return s.src.Close() }

// SetRegion 热切换采集区域（分辨率不变时编码器不重建 → 无感切换）。
func (s *Sharer) SetRegion(r capture.Rect) {
	s.src.SetRegion(r)
	s.enc.ForceKeyFrame()
}

// AddPeer 加入一个观众。新观众会触发一次全量帧，保证秒开。
func (s *Sharer) AddPeer(p *rtc.Peer) {
	s.mu.Lock()
	s.peers[p] = struct{}{}
	s.wantKey = true
	s.mu.Unlock()
	s.enc.ForceKeyFrame()
}

// RemovePeer 移除观众。
func (s *Sharer) RemovePeer(p *rtc.Peer) {
	s.mu.Lock()
	delete(s.peers, p)
	s.mu.Unlock()
}

// PeerCount 返回当前观众数。
func (s *Sharer) PeerCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.peers)
}

// RequestKeyFrame 请求下一个编码帧为全量帧（观众丢片后恢复用）。
func (s *Sharer) RequestKeyFrame() { s.enc.ForceKeyFrame() }

// Preset 返回当前档位（界面显示用）。
func (s *Sharer) Preset() Preset {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.Preset
}

// SetPreset 切换质量档位。会触发一次全量帧。
func (s *Sharer) SetPreset(p Preset) {
	s.mu.Lock()
	s.cfg.Preset = p
	s.mu.Unlock()
	s.enc.ForceKeyFrame()
}

// pauseInterval 是暂停期间发送纯黑帧的间隔。
//
// 为什么暂停要发黑帧而不是干脆不发包：观众看到的"画面停住不动"和
// "连接断了"长得一模一样，他无法区分。发一帧全黑是明确的信号。
const pauseInterval = time.Second

// 全黑判定参数。抽样间隔取 15 帧、连续 4 次判定，意味着 ~60 帧（约 2 秒）
// 的全黑才认定 —— 足够避开一两次瞬时的黑帧（例如应用全屏切换），
// 又能在锁屏这类持续状态上很快反应过来。
const (
	blackSampleEvery = 15
	blackStreakLimit = 4
)

// BlackScreen 报告采集源是否疑似取不到画面（持续全黑）。
//
// 实测场景：会话锁屏后 DXGI Duplication 取不到内容，回退 GDI 也抓不到
// 安全桌面，于是每一帧都是黑的 —— 此时观众看到的是一片黑，带宽接近 0。
// 不检测的话用户只能对着黑屏猜（"是我设置错了？还是断线了？"）。
func (s *Sharer) BlackScreen() bool {
	return s.blackStreak.Load() >= blackStreakLimit
}

// sampleBlack 抽样判断一帧是否几乎全黑，并维护连续计数。
func (s *Sharer) sampleBlack(f capture.Frame) {
	s.sampleTick++
	if s.sampleTick < blackSampleEvery {
		return
	}
	s.sampleTick = 0
	if f.Empty() {
		return
	}
	// 大步长抽样：只为判断"是不是全黑"，不需要精确统计
	step := 8
	n, nb := 0, 0
	for y := 0; y < f.H; y += step {
		row := y * f.W * 4
		for x := 0; x < f.W; x += step {
			i := row + x*4
			if i+2 >= len(f.Pix) {
				break
			}
			lum := (77*int(f.Pix[i+2]) + 150*int(f.Pix[i+1]) + 29*int(f.Pix[i])) >> 8
			if lum > 8 {
				nb++
			}
			n++
		}
	}
	if n == 0 {
		return
	}
	if float64(nb)/float64(n) < 0.002 {
		s.blackStreak.Add(1)
	} else {
		s.blackStreak.Store(0)
	}
}

// Paused 报告当前是否处于暂停。
func (s *Sharer) Paused() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.paused
}

// SetPaused 暂停/恢复画面推送。暂停期间观众看到纯黑画面。
//
// 恢复时会强制一次全量帧 —— MJPEG 每帧独立，但 dirty tile 增量会让观众
// 只收到变化区域，缺了全量帧就会一直看到上一帧的残影。
func (s *Sharer) SetPaused(on bool) {
	s.mu.Lock()
	s.paused = on
	s.mu.Unlock()
	if !on {
		s.enc.ForceKeyFrame()
	}
}

// blackFrame 返回一帧纯黑画面（按当前采集区域尺寸）。
func (s *Sharer) blackFrame() capture.Frame {
	r := s.src.Region()
	n := r.W * r.H * 4
	if n <= 0 {
		return capture.Frame{}
	}
	if len(s.black) != n {
		s.black = make([]byte, n)
		// BGRA：只有 alpha 需要置 255，其余保持 0 就是纯黑不透明
		for i := 3; i < n; i += 4 {
			s.black[i] = 255
		}
	}
	return capture.Frame{Pix: s.black, W: r.W, H: r.H, TS: time.Now()}
}

// Stats 返回统计快照。
func (s *Sharer) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.stats
	st.Stats = s.src.Stats()
	if st.Frames > 0 {
		st.EncodeMs /= float64(st.Frames)
	}
	return st
}

// ErrIdle 表示等待期间桌面没有变化（不是故障）。
var ErrIdle = errors.New("pipeline: 桌面无变化")

// minTolerance 是限帧判定的容差，见 Run 中的说明。
const minTolerance = 3 * time.Millisecond

// Run 运行采集-编码-扇出循环，直到 ctx 结束。
func (s *Sharer) Run(ctx context.Context) error {
	p := s.cfg.Preset
	var minInterval time.Duration
	if p.MaxFPS > 0 {
		minInterval = time.Second / time.Duration(p.MaxFPS)
	}
	lastSend := time.Time{}
	lastBeat := time.Now()
	lastPauseEmit := time.Time{}

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if s.Paused() {
			// 暂停期间不检测黑屏：黑帧是自己发的，与采集是否可用无关。
			s.blackStreak.Store(0)
			// 暂停：低频发一帧纯黑，明确告诉观众"被暂停了"而不是"卡住了"。
			if time.Since(lastPauseEmit) >= pauseInterval {
				_ = s.emit(s.blackFrame())
				lastPauseEmit = time.Now()
			}
			// 仍要用带超时的等待，否则 ctx 取消要等到下一次桌面变化才响应。
			c, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
			_, _ = s.src.WaitFrame(c)
			cancel()
			continue
		}
		f, err := s.src.WaitFrame(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// 等待超时 = 桌面没变化。发一次保活心跳，让观众端知道连接还活着。
			if time.Since(lastBeat) >= s.cfg.IdleHeartbeat && s.PeerCount() > 0 {
				if hb := s.src.Last(); !hb.Empty() {
					_ = s.emit(hb)
					s.mu.Lock()
					s.stats.Heartbeat++
					s.mu.Unlock()
				}
				lastBeat = time.Now()
			}
			continue
		}
		lastBeat = time.Now()
		// 只在真实采集帧上检测：暂停期间我们本来就发黑帧，不能算"取不到画面"。
		s.sampleBlack(f)

		// 帧率上限：限帧在编码之前，省掉不必要的 CPU。
		//
		// ⚠️ 必须留一个容差窗口。若严格用 `now-lastSend < interval` 判定，
		// 采集本身就是 ~33ms 一帧（30fps），抖动会让它反复卡在边界上被判为"太快"，
		// 结果 30fps 档实际只发出 19.7fps —— 实测踩过。
		if minInterval > 0 {
			if now := time.Now(); now.Sub(lastSend) < minInterval-minTolerance {
				s.mu.Lock()
				s.stats.DropIdle++
				s.mu.Unlock()
				continue
			}
			lastSend = time.Now()
		}
		if err := s.emit(f); err != nil {
			return err
		}
	}
}

// emit 编码一帧并扇出给所有观众。
func (s *Sharer) emit(f capture.Frame) error {
	if f.Empty() {
		return nil
	}
	enc := s.enc
	if s.mu.TryLock() {
		s.wantKey = false
		s.mu.Unlock()
	}
	out, err := enc.Encode(f.Pix, f.W, f.H)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.stats.Frames++
	s.stats.Bytes += uint64(out.Bytes)
	s.stats.EncodeMs += out.EncodeMs
	peers := make([]*rtc.Peer, 0, len(s.peers))
	for p := range s.peers {
		peers = append(peers, p)
	}
	s.mu.Unlock()

	if s.cfg.OnFrame != nil {
		s.cfg.OnFrame(out, f)
	}
	for _, p := range peers {
		// 背压在 Peer 内部处理：弱网观众自己丢帧，不影响其他人
		_ = p.SendFrame(out)
	}
	return nil
}
