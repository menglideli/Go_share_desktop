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
	"fmt"
	"log"
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
	// KeyBurst 是"新观众接入后密集开修复轮次"的窗口时长（R32）。
	//
	// 为什么需要：增量帧只能更新"变化过的"条带。观众若在接入那一刻丢掉了
	// 补画布的那批数据（通道尚未就绪 / 分块丢失），静止区域就**永远是黑的**，
	// 而且不会再有任何机制去补 —— 实测观众开局只能拿到 11.8% 的画面，
	// 靠桌面恰好大幅变化才偶然补全（等了 12 秒 / 226 帧）。
	// 接入后这段时间内允许反复开启修复轮次（渐进式铺满整屏），
	// 把"开局必然完整"变成确定性。
	// 0 表示用默认 3s；负数表示禁用。
	KeyBurst time.Duration
	// KeyBurstGap 是补画布窗口内两次修复轮次的最小间隔。0 表示用默认 400ms。
	KeyBurstGap time.Duration
	// KeyInterval 是**稳态**下周期性修复轮次的间隔（兜底自愈）。
	//
	// 默认 0（禁用）：稳态靠观众端"发现画布不完整 → 请求修复"（CtlPLI）来修，
	// 不必持续付出补画布的带宽。实测开启 3s 稳态周期会让带宽从 5.5 涨到
	// 9.2 Mbps（+67%），代价明显。负数同样表示禁用。
	KeyInterval time.Duration
	// AdaptiveOff 关闭自适应质量（默认开启）。
	//
	// 自适应干什么：扇出侧任何一名观众的发送缓冲积压（背压丢帧）时，
	// 自动降档——先降 JPEG 质量（每档 -8，底线 42），再限帧率（30→15）；
	// 压力消失约 10 秒后逐级回升，直到回到用户选定档位。
	// 触发信号有两个：已发生的背压丢帧数，以及**缓冲水位**这个连续量
	// （水位过半即降档，首次降档不等冷却 —— 只看丢帧数会"卡住了才慢慢限速"）。
	AdaptiveOff bool
	// OnFrame 每编码完一帧回调（诊断/预览用），可为 nil。
	OnFrame func(f *codec.Frame, src capture.Frame)
}

func (c Config) withDefaults() Config {
	if c.FPS <= 0 {
		c.FPS = 30
	}
	if c.KeyBurst == 0 {
		c.KeyBurst = 3 * time.Second
	}
	if c.KeyBurstGap == 0 {
		c.KeyBurstGap = 400 * time.Millisecond
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
	Keys      uint64 // 全量帧数（含接入/切换档位/周期兜底）
	EncodeMs  float64 // 均值
	// SentBytes / SentFrames 是**真正发给观众的**字节与人次帧数。
	//
	// 和 Bytes / Frames 的区别：编码只做一次（Bytes 记的是这一份），
	// 但要给 N 个观众各发一份，且弱网观众会被背压丢掉。
	// 所以 Bytes 是"编码码率"，SentBytes 才是"出网码率"——
	// 5 人时前者 4.0 Mbps、后者约 16 Mbps，HUD 上必须分开显示，
	// 否则按 4 Mbps 估带宽会严重低估（实测踩过）。
	SentBytes  uint64
	SentFrames uint64
	capture.Stats
}

// wireFrame 是一帧已序列化的线格式数据（扇出共享缓存）。
//
// 架构（"写缓存 / 读缓存"）：Run 循环每帧只 encode + Marshal 一次，把结果
// 放进 latest；每个观众有独立的发送 goroutine，被唤醒后读 latest 里**最新**
// 那份自己发送。慢观众自然跳过中间帧（永远拿最新的），快观众一帧不落，
// 谁都不占采集/编码 goroutine 的时间 —— 旧实现把"每观众一次 Marshal +
// 分片"全串在采集循环里，4 人时全员 fps 从 28+ 腰斩到 15（实测 e2e63）。
//
// chunks 是切好的分块（每块自带帧头 + 完整条带，见 codec.Frame.Split）：
// 发送就是逐条发，对端收到一块解一块 —— 丢一块只丢它带的条带。
type wireFrame struct {
	seq    uint64
	chunks [][]byte
	bytes  int
}

// peerSlot 是一个观众的扇出状态。
type peerSlot struct {
	wake chan struct{}  // cap 1：新帧到达信号（多次发布会聚合，读时总是取最新）
	done chan struct{}  // 关闭即让发送 goroutine 退出
	once sync.Once      // done 只关一次
}

// Sharer 是一条分享管线：单采集、单编码、扇出给多个观众。
type Sharer struct {
	cfg Config
	// src 用原子指针：Run 的 WaitFrame 可能阻塞很久（桌面静止时几秒不出帧），
	// 跨屏热切换（SetDisplay）若持锁等它就会卡死。换源 = 建新源 → 原子换 → 关旧源，
	// Run 每轮循环重新取指针，最多少发一帧旧画面，编码器按新尺寸自重配置。
	src atomic.Pointer[capture.Source]
	enc *codec.Encoder

	mu    sync.Mutex
	peers map[*rtc.Peer]*peerSlot
	stats Stats
	// latest 是扇出共享缓存：最近一帧的线格式数据（见 wireFrame 注释）。
	latest atomic.Pointer[wireFrame]
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
	// lastKeyAt 是上一次"全量帧"发出（编码）的时刻。
	lastKeyAt time.Time
	// keyUntil 是"补帧窗口"的截止时刻（新观众接入时开启，R32）。
	keyUntil time.Time
	// bpDrops 是扇出侧背压丢帧计数（自适应控制器的反馈信号）。
	// 只有 sendLoop 写、adaptLoop 读，用原子。
	bpDrops atomic.Uint64
	// bpWatermark 是这一秒内观测到的最高缓冲水位（占阈值的百分比，0~100）。
	//
	// 为什么不能只看"已经发生的丢帧数"：丢帧是**阈值被突破之后**的结果，
	// 而候选信号（积压水位）在突破之前就已经在爬升了。只看丢帧数，用户感受
	// 就是"都卡成那样了才慢慢降档"。水位是连续量，能让控制器提前一步。
	bpWatermark atomic.Int64
	// adaptMu 保护 adapt 的 level/cleanTicks/lastDown：
	// adaptLoop（每秒 tick）与 SetPreset（界面 goroutine）都会写。
	adaptMu sync.Mutex
	// lastRepair 是上一次开启渐进式修复轮次的时刻（持 adaptMu 访问）。
	// 冷却见 repairCooldown。
	lastRepair time.Time
	// adapt 是自适应质量控制器状态（持 adaptMu 访问）。
	adapt adaptCtrl
}

// adaptCtrl 是自适应质量控制器的内部状态（见 Config.AdaptiveOff）。
type adaptCtrl struct {
	level      int       // 当前档（0 = 用户设定档位，越大越省带宽）
	maxLevel   int       // 由基准档位算出：质量阶梯级数 + 帧率阶梯级数
	cleanTicks int       // 连续无背压的秒数（回升依据）
	// lastDown 是上次**降档**时刻（降档冷却用）。回升不更新它：
	// 刚升上去就遇到背压说明升错了，要能立刻降回来，不受冷却挡。
	lastDown time.Time
	// wantQuality / wantMaxFPS 是控制器算出的期望参数，Run 每帧读取并应用
	// （编码器非并发安全，只能由 Run 那个 goroutine 碰）。
	wantQuality atomic.Int64
	wantMaxFPS  atomic.Int64
}

// 自适应阶梯参数：质量每档 -8、底线 42；质量到底后限帧 30→15。
// 42 的底线是实测取的：再低文字边缘明显发虚，不如直接限帧。
const (
	adaptQualityStep  = 8
	adaptQualityFloor = 42
	adaptFPSMid       = 30
	adaptFPSLow       = 15
	// adaptDownCooldown 是连续两次降档的最小间隔：给降档效果一点生效时间，
	// 避免一秒内从满血连降到底。**首次降档不受它约束** —— 水位都过半了还等 2 秒，
	// 用户的感受就是"卡住了它才慢慢反应"。
	adaptDownCooldown = 2 * time.Second
	// adaptUpAfterTicks 是回升一格需要的连续无背压秒数（回升必须比降档慢，
	// 否则在网络临界点上会来回震荡）。
	adaptUpAfterTicks = 10
	// adaptPressurePct / adaptCleanPct 是"缓冲水位"的两个门限（百分比）。
	// 过半即视为有压力（提前降档）；必须落到低位才计入"干净秒"（避免临界点震荡）。
	adaptPressurePct = 50
	adaptCleanPct    = 20
)

// adaptMaxLevel 计算基准档位下可用的降档级数。
func adaptMaxLevel(base Preset) int {
	n := 0
	q, f := base.Quality, base.MaxFPS
	for {
		switch {
		case q-adaptQualityStep >= adaptQualityFloor:
			q -= adaptQualityStep
		case f <= 0 || f > adaptFPSMid:
			f = adaptFPSMid
		case f > adaptFPSLow:
			f = adaptFPSLow
		default:
			return n
		}
		n++
	}
}

// adaptParams 返回第 level 档对应的（质量, 帧率上限）。0 表示不限帧。
func adaptParams(base Preset, level int) (quality, maxFPS int) {
	q, f := base.Quality, base.MaxFPS
	for i := 0; i < level; i++ {
		switch {
		case q-adaptQualityStep >= adaptQualityFloor:
			q -= adaptQualityStep
		case f <= 0 || f > adaptFPSMid:
			f = adaptFPSMid
		case f > adaptFPSLow:
			f = adaptFPSLow
		}
	}
	return q, f
}

// applyAdapt 把当前档位的期望参数写出去（Run 每帧拾取应用）。
// 调用方必须持 adaptMu。
func (s *Sharer) applyAdapt(base Preset) {
	q, f := adaptParams(base, s.adapt.level)
	s.adapt.wantQuality.Store(int64(q))
	s.adapt.wantMaxFPS.Store(int64(f))
}

// resetAdapt 切换基准档位时调用：回到 0 档并按新基准重算阶梯。
func (s *Sharer) resetAdapt(base Preset) {
	s.adaptMu.Lock()
	defer s.adaptMu.Unlock()
	s.adapt.level = 0
	s.adapt.maxLevel = adaptMaxLevel(base)
	s.adapt.cleanTicks = 0
	s.adapt.lastDown = time.Time{}
	s.applyAdapt(base)
}

// adaptLoop 每秒评估一次扇出侧背压，驱动升降档。
func (s *Sharer) adaptLoop(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.adaptTick(s.bpDrops.Swap(0), int(s.bpWatermark.Swap(0)))
	}
}

// reportWatermark 记录这一秒内的最高水位（占阈值百分比）。只有 sendLoop 调用。
func (s *Sharer) reportWatermark(pct int) {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	for {
		cur := s.bpWatermark.Load()
		if int64(pct) <= cur {
			return
		}
		if s.bpWatermark.CompareAndSwap(cur, int64(pct)) {
			return
		}
	}
}

// adaptTick 是单步调节逻辑，独立出来是为了确定性校验（internal/pipeline 的测试直接驱动它）。
//
// drops 是这一秒的背压丢帧数，watermark 是这一秒观测到的最高缓冲水位（%）。
func (s *Sharer) adaptTick(drops uint64, watermark int) {
	s.adaptMu.Lock()
	defer s.adaptMu.Unlock()
	base := s.Preset()
	if drops > 0 || watermark >= adaptPressurePct {
		s.adapt.cleanTicks = 0
		// 首次降档不等冷却（见 adaptDownCooldown 注释）。
		allow := s.adapt.level == 0 || time.Since(s.adapt.lastDown) >= adaptDownCooldown
		if s.adapt.level < s.adapt.maxLevel && allow {
			old := s.adapt.level
			s.adapt.level++
			s.adapt.lastDown = time.Now()
			s.applyAdapt(base)
			q, f := adaptParams(base, s.adapt.level)
			log.Printf("pipeline: 自适应降档 %d→%d（背压丢帧 %d/s，缓冲水位 %d%%，质量→%d，fps 上限→%s）",
				old, s.adapt.level, drops, watermark, q, fpsText(f))
		}
		return
	}
	if s.adapt.level == 0 {
		return
	}
	// 水位没落到低位就不算"干净秒"：阈值边缘来回抖时不该回升。
	if watermark >= adaptCleanPct {
		return
	}
	s.adapt.cleanTicks++
	if s.adapt.cleanTicks >= adaptUpAfterTicks {
		old := s.adapt.level
		s.adapt.level--
		s.adapt.cleanTicks = 0
		// 注意：不更新 lastDown —— 回升后立刻再遇背压要能马上降回来。
		s.applyAdapt(base)
		q, f := adaptParams(base, s.adapt.level)
		log.Printf("pipeline: 自适应回升 %d→%d（持续无背压，缓冲水位 %d%%，质量→%d，fps 上限→%s）",
			old, s.adapt.level, watermark, q, fpsText(f))
	}
}

func fpsText(f int) string {
	if f <= 0 {
		return "不限"
	}
	return fmt.Sprintf("%d", f)
}

// AdaptiveState 返回自适应状态（界面显示用）。level==0 表示满血（用户设定档位）。
func (s *Sharer) AdaptiveState() (level, maxLevel, quality, maxFPS int) {
	s.adaptMu.Lock()
	defer s.adaptMu.Unlock()
	return s.adapt.level, s.adapt.maxLevel,
		int(s.adapt.wantQuality.Load()), int(s.adapt.wantMaxFPS.Load())
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
	sh := &Sharer{
		cfg:       cfg,
		enc:       codec.NewEncoder(codec.Config{Quality: cfg.Preset.Quality, Tiles: cfg.Preset.Tiles, Dirty: cfg.Preset.Dirty}),
		peers:     map[*rtc.Peer]*peerSlot{},
		lastKeyAt: time.Now(),
	}
	sh.src.Store(src)
	return sh, nil
}

// Source 返回底层采集源。
func (s *Sharer) Source() *capture.Source { return s.src.Load() }

// Close 停止采集。
func (s *Sharer) Close() error { return s.src.Load().Close() }

// SetRegion 热切换采集区域（分辨率不变时编码器不重建 → 无感切换）。
func (s *Sharer) SetRegion(r capture.Rect) {
	s.src.Load().SetRegion(r)
	s.enc.ForceKeyFrame()
}

// SetDisplay 热切换到另一台显示器（跨屏）。
//
// 为什么不能复用 SetRegion：采集流绑定在显示器上，换屏必须重建 capture.Source
// （新显示器可能分辨率、DPI 缩放都不同，光标坐标原点也跟着变）。
//
// 顺序是"先建好新源再换"：新建/预热可能花几百毫秒甚至失败，这段时间旧源照常出帧，
// 观众无感；反过来"先关旧再建新"会让观众看到一段黑。预热同样不能省 ——
// DXGI 首帧常是未初始化黑帧（R25），换屏后观众第一眼不能是黑的。
func (s *Sharer) SetDisplay(ctx context.Context, d capture.Display, r capture.Rect) error {
	src, err := capture.NewSource(ctx, capture.Options{
		Display: d,
		Region:  r,
		FPS:     s.cfg.FPS,
		Cursor:  s.cfg.Cursor,
	})
	if err != nil {
		return err
	}
	if s.cfg.Warmup >= 0 {
		_, _ = src.Warmup(ctx, 3, s.cfg.Warmup)
	}
	old := s.src.Swap(src)
	if old != nil {
		// 旧源的 WaitFrame 可能还在阻塞：Close 会让它报错返回，Run 下一轮取到新源。
		_ = old.Close()
	}
	// 尺寸大概率变了：全量帧让观众画布整体重建，避免 dirty 增量只盖一部分（R32 同类）。
	s.enc.ForceKeyFrame()
	return nil
}

// AddPeer 加入一个观众：开启修复轮次 + 补帧窗口，保证秒开且开局画面完整。
func (s *Sharer) AddPeer(p *rtc.Peer) {
	slot := &peerSlot{wake: make(chan struct{}, 1), done: make(chan struct{})}
	s.mu.Lock()
	// 同一 Peer 重复 Add（理论上不该发生）时先停掉旧 goroutine，避免泄漏。
	if old, ok := s.peers[p]; ok {
		old.once.Do(func() { close(old.done) })
	}
	s.peers[p] = slot
	s.wantKey = true
	// 开启补帧窗口：新观众要的是"尽快看到完整画面"，而不是"恰好发过一次
	// 全量帧"（那一次很可能撞在通道未就绪上，见 R32）。
	if s.cfg.KeyBurst > 0 {
		s.keyUntil = time.Now().Add(s.cfg.KeyBurst)
	}
	s.mu.Unlock()
	// 渐进式修复：约 10 帧内把整屏铺一遍。比"发一帧全量"更适合弱网 ——
	// 全量帧是一发 2MB 的突发，丢了就整发白丢，观众端只会再来要一次。
	s.adaptMu.Lock()
	s.lastRepair = time.Time{}
	s.adaptMu.Unlock()
	s.RequestRepair()
	go s.sendLoop(p, slot)
}

// RemovePeer 移除观众。
func (s *Sharer) RemovePeer(p *rtc.Peer) {
	s.mu.Lock()
	slot, ok := s.peers[p]
	delete(s.peers, p)
	s.mu.Unlock()
	if ok {
		slot.once.Do(func() { close(slot.done) })
	}
}

// sendLoop 是单个观众的发送 goroutine：被唤醒后读共享缓存里**最新**一帧
// 分块逐条发送。慢观众被背压时直接跳过整帧（下一唤醒拿到的还是最新的），
// 不拖慢采集循环，也不影响其他观众。
func (s *Sharer) sendLoop(p *rtc.Peer, slot *peerSlot) {
	var lastSent uint64
	for {
		select {
		case <-slot.done:
			return
		case <-slot.wake:
		}
		wf := s.latest.Load()
		if wf == nil || wf.seq <= lastSent {
			continue
		}
		lim := rtc.SendLimit(wf.bytes)
		// 发送**之前**先看水位：已经积压到阈值就整帧跳过，不推进 lastSent ——
		// 下一次唤醒读到的还是 latest 里最新的那帧，这才是真正的"跳帧不排队"。
		if buf := p.Buffered(); buf > lim {
			s.bpDrops.Add(1)
			s.reportWatermark(100)
			continue
		}
		// 发送侧还有一道背压兜底（弱网观众自己丢帧）。
		if err := p.SendChunks(wf.chunks); err == nil {
			lastSent = wf.seq
			s.mu.Lock()
			s.stats.SentBytes += uint64(wf.bytes)
			s.stats.SentFrames++
			s.mu.Unlock()
			// 发完再看一次水位：它是"下一秒会不会堵"的先行指标。
			s.reportWatermark(int(p.Buffered() * 100 / lim))
		} else if errors.Is(err, rtc.ErrBackpressure) {
			// 有人顶不住了：喂给自适应控制器（它每秒取走并清零）。
			s.bpDrops.Add(1)
		} else if errors.Is(err, rtc.ErrNotOpen) {
			// 通道还没建好（握手中）：不算发送成功，但 seq 也别推进，
			// 等下一帧再试 —— 接入期的修复轮次会保证开局完整性。
		}
	}
}

// PeerCount 返回当前观众数。
func (s *Sharer) PeerCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.peers)
}

// repairCooldown 是两次"渐进式修复轮次"之间的最小间隔。
//
// 不能没有：修复轮次会给每帧多带 1/N 条带，如果每次丢块都重启一轮，
// 拥塞下就变成"修复流量 → 更堵 → 更多丢块 → 更多修复"的正反馈。
// 一轮修复本身约 10 帧（0.3 秒）就能铺满，冷却期里丢的块也会被铺到。
const repairCooldown = 800 * time.Millisecond

// defaultRepairOverFrames 是一轮渐进式修复摊到多少帧。
// 10 帧 @30fps ≈ 0.33 秒铺满整屏，期间每帧多带 1/10 的条带 —— 单帧依旧很小。
const defaultRepairOverFrames = 10

// RequestRepair 请求一轮**渐进式修复**：把补齐整屏分摊到后续约 10 帧里，
// 而不是发一发 2MB 的全量帧突发（拥塞时那一发大概率整发丢掉）。
//
// 观众端在"画布不完整"（丢块/丢帧/刚接入）时调用；抢不到冷却期的请求会被合并。
func (s *Sharer) RequestRepair() {
	s.adaptMu.Lock()
	if s.enc.Repairing() || time.Since(s.lastRepair) < repairCooldown {
		s.adaptMu.Unlock()
		return
	}
	s.lastRepair = time.Now()
	s.adaptMu.Unlock()
	// 编码器非并发安全：BeginRepair 只改它自己的状态位，由 Run 那个 goroutine
	// 在下一帧读取并生效（applyAdapt 是同一种做法）。
	s.enc.BeginRepair(defaultRepairOverFrames)
}

// RequestKeyFrame 请求下一个编码帧为**完整全量帧**。
// 只在画布尺寸/内容整体失效时用（换屏、换区域、切换档位）——
// 丢块后的恢复请用 RequestRepair（渐进式，抗拥塞）。
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
	r := s.src.Load().Region()
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
	st.Stats = s.src.Load().Stats()
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
	// 自适应控制器：每秒看一次扇出侧背压，升降档结果写进 wantQuality /
	// wantMaxFPS，由本循环每帧拾取（编码器非并发安全，只能在这里应用）。
	if !s.cfg.AdaptiveOff {
		go s.adaptLoop(ctx)
	}
	lastSend := time.Time{}
	lastBeat := time.Now()
	lastPauseEmit := time.Time{}

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// 拾取自适应期望参数（未开启时恒为基准档位，零成本）。
		var minInterval time.Duration
		if f := int(s.adapt.wantMaxFPS.Load()); f > 0 {
			minInterval = time.Second / time.Duration(f)
		}
		if q := int(s.adapt.wantQuality.Load()); q > 0 && q != s.enc.Config().Quality {
			s.enc.SetQuality(q)
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
			_, _ = s.src.Load().WaitFrame(c)
			cancel()
			continue
		}
		f, err := s.src.Load().WaitFrame(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// 等待超时 = 桌面没变化。发一次保活心跳，让观众端知道连接还活着。
			if time.Since(lastBeat) >= s.cfg.IdleHeartbeat && s.PeerCount() > 0 {
				if hb := s.src.Load().Last(); !hb.Empty() {
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
	// 修复策略（R32/R39）——分两层，代价与收益分开算：
	//
	//  1) 接入期（新观众刚接入的 KeyBurst 窗口内）：反复开启**修复轮次**，
	//     保证"开局一定拿到完整画面"，不依赖那一次性触发是否撞在通道未就绪上。
	//  2) 稳态周期（KeyInterval，默认关闭）：只在明确启用时才有，属于兜底中的
	//     兜底 —— 常态恢复靠观众端请求全量，不值得为它一直付全量帧的带宽。
	if s.cfg.KeyBurst > 0 || s.cfg.KeyInterval > 0 {
		now := time.Now()
		s.mu.Lock()
		nPeers := len(s.peers)
		inBurst := !s.keyUntil.IsZero() && now.Before(s.keyUntil)
		burstDue := inBurst && s.cfg.KeyBurst > 0 && now.Sub(s.lastKeyAt) >= s.cfg.KeyBurstGap
		steadyDue := s.cfg.KeyInterval > 0 && now.Sub(s.lastKeyAt) >= s.cfg.KeyInterval
		s.mu.Unlock()
		if nPeers > 0 && (burstDue || steadyDue) {
			// 用渐进式修复而不是一发全量帧：接入期连发好几个 2MB 突发，
			// 正是"新人一进来大家都卡"的来源（实测过）。
			s.RequestRepair()
			s.mu.Lock()
			s.lastKeyAt = time.Now()
			s.mu.Unlock()
		}
	}
	out, err := enc.Encode(f.Pix, f.W, f.H)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.stats.Frames++
	s.stats.Bytes += uint64(out.Bytes)
	s.stats.EncodeMs += out.EncodeMs
	if out.Full {
		s.lastKeyAt = time.Now()
		s.stats.Keys++
	}
	slots := make([]*peerSlot, 0, len(s.peers))
	for _, slot := range s.peers {
		slots = append(slots, slot)
	}
	s.mu.Unlock()

	if s.cfg.OnFrame != nil {
		s.cfg.OnFrame(out, f)
	}
	if len(slots) == 0 {
		return nil
	}
	// 写共享缓存：线格式只序列化一次（旧实现每个观众各 Marshal 一遍，
	// 2K 全量 ≈2MB/次的拷贝全串在采集 goroutine 上，4 人时全员 fps 腰斩）。
	// 现在按"每块自带帧头 + 完整条带"切成若干可独立解析的分块，逐条发。
	parts := out.Split(codec.MaxChunkPayload)
	chunks := make([][]byte, 0, len(parts))
	total := 0
	for _, c := range parts {
		data, err := c.Marshal()
		if err != nil {
			return err
		}
		chunks = append(chunks, data)
		total += len(data)
	}
	s.latest.Store(&wireFrame{seq: out.Seq, chunks: chunks, bytes: total})
	// 广播唤醒（非阻塞：慢观众的 wake 里已有信号就说明它还没读，
	// 反正它读的时候拿的是 latest 里最新的，不丢"最新性"）。
	for _, slot := range slots {
		select {
		case slot.wake <- struct{}{}:
		default:
		}
	}
	return nil
}
