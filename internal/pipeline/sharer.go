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
	"math/bits"
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
	Name string
	// Quality 是 JPEG 质量。
	Quality int
	// Tiles 是显式条带数（<=0 = 按 TileHeight 算）。
	Tiles int
	// TileHeight 是目标条带高度（像素）：0 = 用 Tiles / NumCPU。
	//
	// 32px 是实测选的：2K 下 57 条，光标闪动的每帧字节从 64.8KB 降到 41.9KB
	// （-35%），打字类 -56%；全量帧只多 3.4% 的 JPEG 头，编码耗时反而略降
	// （每帧重编的面积小了）。见 `cmd/tilebench`。
	TileHeight int
	Dirty      bool
	MaxFPS     int // 0 = 不限（最大）
}

var (
	// PresetMax 是默认档：不限帧率，画质优先。
	PresetMax = Preset{Name: "最大", Quality: 75, TileHeight: 32, Dirty: true, MaxFPS: 0}
	// Preset60 上限 60fps。
	Preset60 = Preset{Name: "流畅60", Quality: 75, TileHeight: 32, Dirty: true, MaxFPS: 60}
	// Preset30 上限 30fps，最省带宽，适合人数多或弱网。
	Preset30 = Preset{Name: "流畅30", Quality: 80, TileHeight: 32, Dirty: true, MaxFPS: 30}
	// PresetTurbo 急速：略降质量换最低延迟（条带粒度同上，全量帧编码并不更慢）。
	PresetTurbo = Preset{Name: "急速", Quality: 65, TileHeight: 32, Dirty: true, MaxFPS: 0}
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
	// FPS 是采集目标帧率，0 = 按档位与面积自动决定（见 captureFPSFor）。
	// 显式给值（-fps）时会原样使用，用于调参与排障。
	FPS    int
	Preset Preset
	Cursor bool
	// fpsAuto 记录 FPS 是否由档位自动决定（withDefaults 里设置）。
	// 自动模式下，换区域/换屏会按新面积重算帧率；显式指定则不动。
	fpsAuto bool
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
	c.fpsAuto = c.FPS <= 0
	if c.fpsAuto {
		c.FPS = captureFPSFor(c.Preset, c.Region, c.Display)
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

// captureFPSFor 决定采集目标帧率（P0-2）。
//
// 为什么不能直接照抄 Preset.MaxFPS：MaxFPS 原先只限制**发送**，采集源一直按
// 30fps 配置 —— 于是"流畅60"永远是假 60（实测 ≤30）。
//
// 但 60fps 不是免费的：2K 一帧编码 12~15ms + 采集 4.8ms，60fps 的预算是
// 16.7ms/帧，根本不成立；硬开只会让编码队列堆积、延迟暴涨。所以按**采集面积**
// 回落：超过 2.5M 像素（≈2K）且档位要 60 时，按 30 采集并在日志里说明原因。
func captureFPSFor(p Preset, r capture.Rect, d capture.Display) int {
	target := p.MaxFPS
	if target <= 0 {
		// "最大"档不额外限帧，但采集仍按 30：60fps 会让带宽翻倍，
		// 而这个档位的承诺是"画质优先"，不是"高帧率"。
		target = 30
	}
	if target > 60 {
		target = 60
	}
	w, h := d.W, d.H
	if !r.Empty() {
		w, h = r.W, r.H
	}
	if w*h > 2_500_000 && target > 30 {
		return 30
	}
	return target
}

// Stats 是管线运行统计。
type Stats struct {
	Frames    uint64
	Bytes     uint64
	DropIdle  uint64 // 因帧率限制跳过的帧
	Heartbeat uint64
	Keys      uint64  // 全量帧数（含接入/切换档位/周期兜底）
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
	wake chan struct{} // cap 1：新帧到达信号（多次发布会聚合，读时总是取最新）
	done chan struct{} // 关闭即让发送 goroutine 退出
	once sync.Once     // done 只关一次
	// cursor 表示该观众声明了"能自己合成光标"（CtlHello）。它决定分享端
	// 是否把光标画进帧 —— 只要有一个观众没声明，就必须继续画（P0-3）。
	cursor bool
	// drops 是该观众这一秒内的背压丢帧数（P0-4：只有"过半观众有压力"
	// 才允许全局降档，单个弱观众不该拖累其他人）。adaptLoop 每秒取走清零。
	drops atomic.Uint64
}

// Sharer 是一条分享管线：单采集、单编码、扇出给多个观众。
type Sharer struct {
	cfg Config
	// ctx 留给"换区域/换屏时可能要重建采集源"用（帧率随之变化，见 desiredFPS）。
	ctx context.Context
	// src 用原子指针：Run 的 WaitFrame 可能阻塞很久（桌面静止时几秒不出帧），
	// 跨屏热切换（SetDisplay）若持锁等它就会卡死。换源 = 建新源 → 原子换 → 关旧源，
	// Run 每轮循环重新取指针，最多少发一帧旧画面，编码器按新尺寸自重配置。
	src atomic.Pointer[capture.Source]
	enc *codec.Encoder
	// fps 是当前采集目标帧率（可随区域变化，用原子读给日志/HUD 用）。
	fps atomic.Int32
	// fpsExplicit > 0 表示用户用 -fps 指定了帧率，不再自动重算。
	fpsExplicit int

	mu    sync.Mutex
	peers map[*rtc.Peer]*peerSlot
	stats Stats
	// curDisplay 是当前采集的显示器（SetRegion 重算帧率要用它的尺寸）。
	curDisplay capture.Display
	// followStop 用于停止"跟随窗口"的轮询（P1-2）。
	followStop chan struct{}
	// cursorOn 为 true 表示"所有观众都能自己合成光标"，此时不再画进帧，
	// 改走 ctl 的 60Hz 光标流（见 cursorLoop）。
	cursorOn atomic.Bool
	// scalePct 是编码前的降采样比例（P1-5）：100 = 不缩放，67 = 2/3，50 = 1/2。
	// 降采样发生在编码前，所以带宽和编码 CPU 一起降；观众端画布按新尺寸重建。
	scalePct atomic.Int32
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
	// bpStress 是上一秒"有背压的观众数"（P0-4，日志/诊断用）。
	bpStress atomic.Int32
	// adaptMu 保护 adapt 的 level/cleanTicks/lastDown：
	// adaptLoop（每秒 tick）与 SetPreset（界面 goroutine）都会写。
	adaptMu sync.Mutex
	// lastRepair 是上一次开启渐进式修复轮次的时刻（持 adaptMu 访问）。
	// 冷却见 repairCooldown。
	lastRepair time.Time
	// missingTiles 是观众端"点名"要补的条带位图（多观众 OR 在一起）。
	// Run/emit 每帧取走一次（见 codec.Encoder.RequestTiles）。
	missingTiles atomic.Uint64
	// adapt 是自适应质量控制器状态（持 adaptMu 访问）。
	adapt adaptCtrl
}

// adaptCtrl 是自适应质量控制器的内部状态（见 Config.AdaptiveOff）。
type adaptCtrl struct {
	level      int // 当前档（0 = 用户设定档位，越大越省带宽）
	maxLevel   int // 由基准档位算出：质量阶梯级数 + 帧率阶梯级数
	cleanTicks int // 连续无背压的秒数（回升依据）
	// lastDown 是上次**降档**时刻（降档冷却用）。回升不更新它：
	// 刚升上去就遇到背压说明升错了，要能立刻降回来，不受冷却挡。
	lastDown time.Time
	// wantQuality / wantMaxFPS 是控制器算出的期望参数，Run 每帧读取并应用
	// （编码器非并发安全，只能由 Run 那个 goroutine 碰）。
	wantQuality atomic.Int64
	wantMaxFPS  atomic.Int64
	wantScale   atomic.Int64 // 编码前降采样百分比（P1-5）
}

// 自适应阶梯参数：质量每档 -8、底线 42；质量到底后限帧 30→15；
// 最后一档才降分辨率（100 → 67 → 50）—— 像素少 60%，带宽和编码 CPU 一起降。
// 42 的底线是实测取的：再低文字边缘明显发虚，不如直接限帧。
const (
	adaptQualityStep  = 8
	adaptQualityFloor = 42
	adaptFPSMid       = 30
	adaptFPSLow       = 15
	adaptScaleMid     = 67 // 2/3
	adaptScaleLow     = 50 // 1/2
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
	q, f, sc := base.Quality, base.MaxFPS, 100
	for {
		switch {
		case q-adaptQualityStep >= adaptQualityFloor:
			q -= adaptQualityStep
		case f <= 0 || f > adaptFPSMid:
			f = adaptFPSMid
		case f > adaptFPSLow:
			f = adaptFPSLow
		case sc > adaptScaleMid:
			sc = adaptScaleMid
		case sc > adaptScaleLow:
			sc = adaptScaleLow
		default:
			return n
		}
		n++
	}
}

// adaptParams 返回第 level 档对应的（质量, 帧率上限, 降采样百分比）。
// 0 帧率表示不限帧；100 表示不缩放。
func adaptParams(base Preset, level int) (quality, maxFPS, scale int) {
	q, f, sc := base.Quality, base.MaxFPS, 100
	for i := 0; i < level; i++ {
		switch {
		case q-adaptQualityStep >= adaptQualityFloor:
			q -= adaptQualityStep
		case f <= 0 || f > adaptFPSMid:
			f = adaptFPSMid
		case f > adaptFPSLow:
			f = adaptFPSLow
		case sc > adaptScaleMid:
			sc = adaptScaleMid
		case sc > adaptScaleLow:
			sc = adaptScaleLow
		}
	}
	return q, f, sc
}

// applyAdapt 把当前档位的期望参数写出去（Run 每帧拾取应用）。
// 调用方必须持 adaptMu。
func (s *Sharer) applyAdapt(base Preset) {
	q, f, sc := adaptParams(base, s.adapt.level)
	s.adapt.wantQuality.Store(int64(q))
	s.adapt.wantMaxFPS.Store(int64(f))
	s.adapt.wantScale.Store(int64(sc))
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
		// P0-4：先把丢帧记到各观众头上，再判断"要不要因为压力降档"。
		// 规则：**过半观众**有压力（或只有 1 个观众）才降；单个弱观众自己跳帧，
		// 不再连累其他人。水位同理 —— 它只是全局信号，不能反映出是谁堵了。
		stressed, total := 0, s.PeerCount()
		s.mu.Lock()
		for _, slot := range s.peers {
			if slot.drops.Swap(0) > 0 {
				stressed++
			}
		}
		s.mu.Unlock()
		s.bpStress.Store(int32(stressed))
		drops := s.bpDrops.Swap(0)
		wm := int(s.bpWatermark.Swap(0))
		if !shouldDegrade(stressed, total) {
			// 压力只在少数观众身上：不发降档信号（他们的 sendLoop 会自己跳帧）。
			drops, wm = 0, 0
		}
		s.adaptTick(drops, wm)
	}
}

// scaleShapeFor 按比例返回（缩放后的形状, 该比例）。100% 时原样返回。
func scaleShapeFor(c rtc.CursorShape, pct int) (rtc.CursorShape, int) {
	if pct <= 0 || pct >= 100 {
		return c, 100
	}
	if sp, ok := scaleCursor(c, pct); ok {
		return sp, pct
	}
	return c, 100
}

// scaleCursor 把光标位图按百分比缩小（热点同比）。id 带上比例，
// 这样同一形状在不同比例下观看端不会命中旧缓存。
func scaleCursor(c rtc.CursorShape, pct int) (rtc.CursorShape, bool) {
	pix, w, h, err := codec.DownscaleBGRA(c.Pix, c.W, c.H, pct, 100)
	if err != nil || w <= 0 || h <= 0 {
		return c, false
	}
	return rtc.CursorShape{
		ID: c.ID ^ uint32(pct)<<24, W: w, H: h,
		HotX: c.HotX * pct / 100, HotY: c.HotY * pct / 100,
		Pix: pix,
	}, true
}

// shouldDegrade 判断这一秒的压力是否足以触发全局降档（P0-4）。
//
// 规则：只有 1 个观众时保持旧行为（他有压力就降）；多观众时必须**过半**有压力
// 才降 —— 否则一个 Wi-Fi 差的观众会连累所有人一起掉质量。
func shouldDegrade(stressed, total int) bool {
	if total <= 1 {
		return true
	}
	return stressed*2 >= total
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
			q, f, sc := adaptParams(base, s.adapt.level)
			log.Printf("pipeline: 自适应降档 %d→%d（背压丢帧 %d/s，缓冲水位 %d%%，质量→%d，fps 上限→%s，缩放→%d%%）",
				old, s.adapt.level, drops, watermark, q, fpsText(f), sc)
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
		q, f, sc := adaptParams(base, s.adapt.level)
		log.Printf("pipeline: 自适应回升 %d→%d（持续无背压，缓冲水位 %d%%，质量→%d，fps 上限→%s，缩放→%d%%）",
			old, s.adapt.level, watermark, q, fpsText(f), sc)
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
		cfg: cfg,
		enc: codec.NewEncoder(codec.Config{
			Quality:    cfg.Preset.Quality,
			Tiles:      cfg.Preset.Tiles,
			TileHeight: cfg.Preset.TileHeight,
			Dirty:      cfg.Preset.Dirty,
		}),
		peers:      map[*rtc.Peer]*peerSlot{},
		lastKeyAt:  time.Now(),
		ctx:        ctx,
		curDisplay: cfg.Display,
	}
	if !cfg.fpsAuto {
		sh.fpsExplicit = cfg.FPS
	}
	sh.fps.Store(int32(cfg.FPS))
	sh.scalePct.Store(100)
	sh.src.Store(src)
	return sh, nil
}

// SetScale 设置编码前的降采样比例（100 = 不缩放）。P1-5。
func (s *Sharer) SetScale(pct int) {
	if pct <= 0 || pct > 100 {
		pct = 100
	}
	s.scalePct.Store(int32(pct))
}

// Scale 返回当前降采样比例（百分数）。
func (s *Sharer) Scale() int { return int(s.scalePct.Load()) }

// CaptureFPS 返回当前实际使用的采集目标帧率（启动日志/HUD 用）。
func (s *Sharer) CaptureFPS() int { return int(s.fps.Load()) }

// desiredFPS 按当前区域/显示器与档位重算采集帧率。
// 用户用 -fps 显式指定时保持不动。
func (s *Sharer) desiredFPS(r capture.Rect, d capture.Display) int {
	if s.fpsExplicit > 0 {
		return s.fpsExplicit
	}
	return captureFPSFor(s.cfg.Preset, r, d)
}

// Source 返回底层采集源。
func (s *Sharer) Source() *capture.Source { return s.src.Load() }

// Close 停止采集。
func (s *Sharer) Close() error {
	if src := s.src.Load(); src != nil {
		return src.Close()
	}
	return nil
}

// SetRegion 热切换采集区域。
//
// 分辨率不变时编码器不重建 → 无感切换；但**帧率可能随面积变**
// （2K 全屏 30fps、1280×720 可以 60fps），所以面积变化导致帧率需要改变时，
// 要重建采集源 —— 采集流的 fps 在创建时固定，不能就地改。用"先建后换"，
// 观众在这几百毫秒里看到的仍是旧源，不会黑。
func (s *Sharer) SetRegion(r capture.Rect) {
	src := s.src.Load()
	if src == nil {
		return
	}
	s.mu.Lock()
	d := s.curDisplay
	s.mu.Unlock()

	// 只关心"面积档位"是否跨过阈值：同档位内换区域不需要重建。
	want := s.desiredFPS(r, d)
	if want == s.CaptureFPS() {
		src.SetRegion(r)
		s.enc.ForceKeyFrame()
		return
	}
	if err := s.rebuildSource(d, r, want); err != nil {
		// rebuildSource 已按旧参数把源恢复回来，这里只打日志，不再对已关闭的源动手。
		log.Printf("切换区域时重建采集源失败（%v），保持原区域", err)
	}
	s.enc.ForceKeyFrame()
}

// SetDisplay 热切换到另一台显示器（跨屏）。
//
// 为什么不能复用 SetRegion：采集流绑定在显示器上，换屏必须重建 capture.Source
// （新显示器可能分辨率、DPI 缩放都不同，光标坐标原点也跟着变）。
//
// 重建顺序见 rebuildSource：先关旧源再建新源（两路 DXGI 复制并存会让新源掉速）。
// 观众在这几百毫秒里看到的是上一帧，不会黑；预热也不能省 —— DXGI 首帧常是
// 未初始化黑帧（R25），新源的第一帧必须先丢掉。
func (s *Sharer) SetDisplay(ctx context.Context, d capture.Display, r capture.Rect) error {
	if err := s.rebuildSource(d, r, s.desiredFPS(r, d)); err != nil {
		return err
	}
	s.mu.Lock()
	s.curDisplay = d
	s.mu.Unlock()
	return nil
}

// rebuildSource 用新的显示器/区域/帧率重建采集源。
//
// ⚠️ 必须**先关旧源再建新源**。实测：先建新源、后关旧源时，同一个显示器上
// 会短暂出现两路 DXGI duplication，新源的交付率会掉到 ~19fps（动画内容下
// 本应 50~60fps），而且不会自己恢复 —— 表现是"切了区域之后变得很卡"。
// 反过来先关旧源，观众只是在这几百毫秒里看不到新帧（画面停在上一帧，不会黑）。
// 新建失败时用旧参数重建一次兜底，避免一次切换把分享彻底弄死。
func (s *Sharer) rebuildSource(d capture.Display, r capture.Rect, fps int) error {
	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	var oldRegion capture.Rect
	oldFPS := s.CaptureFPS()
	if old := s.src.Load(); old != nil {
		oldRegion = old.Region()
	}
	if old := s.src.Swap(nil); old != nil {
		_ = old.Close()
	}

	src, err := capture.NewSource(ctx, capture.Options{
		Display: d,
		Region:  r,
		FPS:     fps,
		Cursor:  s.cfg.Cursor,
	})
	if err != nil {
		// 兜底：按旧参数把源建回来，至少保持分享不断（帧率/区域退回旧值）。
		if o, e2 := capture.NewSource(ctx, capture.Options{
			Display: d, Region: oldRegion, FPS: oldFPS, Cursor: s.cfg.Cursor,
		}); e2 == nil {
			s.src.Store(o)
			log.Printf("重建采集源失败（%v），已按旧区域恢复", err)
		}
		return err
	}
	if s.cfg.Warmup >= 0 {
		_, _ = src.Warmup(ctx, 3, s.cfg.Warmup)
	}
	s.src.Store(src)
	// 尺寸大概率变了：全量帧让观众画布整体重建，避免 dirty 增量只盖一部分（R32 同类）。
	s.enc.ForceKeyFrame()
	s.fps.Store(int32(fps))
	// 新源的"画光标"默认值是 opt.Cursor（true）；按当前观众能力重新落一次。
	s.applyCursorMode()
	if oldFPS != fps {
		log.Printf("采集帧率 %d → %d fps（面积变化）", oldFPS, fps)
	}
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
	// 新观众默认"还没有能力信息"→ 继续把光标画进帧，等它发 CtlHello。
	s.updateCursorMode()
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
	// 走掉的可能正是最后一位旧观众：此时可以切到"光标走 ctl"的模式。
	s.updateCursorMode()
}

// SetPeerCursorCap 记录某位观众是否声明了"能自己合成光标"（CtlHello）。
func (s *Sharer) SetPeerCursorCap(p *rtc.Peer, on bool) {
	s.mu.Lock()
	slot, ok := s.peers[p]
	if ok && slot.cursor != on {
		slot.cursor = on
	}
	s.mu.Unlock()
	if ok {
		s.updateCursorMode()
	}
}

// updateCursorMode 重新判断"要不要把光标画进帧"。
//
// 规则：**只有当所有观众都声明了能力**时才关掉画进帧，改为走 ctl 光标流。
// 只要有一个旧观众端（或还没发 Hello 的新观众），就继续画进帧 ——
// 宁可让新观众短暂看到"帧内光标 + 自己的合成光标"，也不能让旧观众端没有光标。
func (s *Sharer) updateCursorMode() {
	s.mu.Lock()
	total := len(s.peers)
	capable := 0
	for _, slot := range s.peers {
		if slot.cursor {
			capable++
		}
	}
	s.mu.Unlock()

	on := total > 0 && capable == total
	if prev := s.cursorOn.Swap(on); prev != on {
		log.Printf("光标独立传输：%v（%d/%d 位观众能自己合成）", on, capable, total)
	}
	s.applyCursorMode()
}

// applyCursorMode 把 cursorOn 落到当前的采集源上（重建源之后也要调用）。
func (s *Sharer) applyCursorMode() {
	if src := s.src.Load(); src != nil {
		src.SetDrawCursor(!s.cursorOn.Load())
	}
}

// cursorPeers 取"声明了光标能力"的观众快照（不要在持锁时发消息）。
func (s *Sharer) cursorPeers() []*rtc.Peer {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*rtc.Peer, 0, len(s.peers))
	for p, slot := range s.peers {
		if slot.cursor {
			out = append(out, p)
		}
	}
	return out
}

// StressCount 返回上一秒有背压的观众数（P0-4，诊断用）。
func (s *Sharer) StressCount() int { return int(s.bpStress.Load()) }

// CursorStreaming 报告当前是否在走"光标独立传输"。
func (s *Sharer) CursorStreaming() bool { return s.cursorOn.Load() }

// FollowWindow 让采集区域跟随一个窗口（P1-2）。
//
// 为什么要 200ms 轮询而不是 WinEventHook：演示场景里拖窗口/改窗口大小是低频动作，
// 200ms 的滞后看不出来；而 hook 要处理跨进程消息循环、窗口销毁竞态，
// 复杂度远高于收益。
func (s *Sharer) FollowWindow(hwnd uintptr) {
	s.mu.Lock()
	if s.followStop != nil {
		close(s.followStop)
	}
	stop := make(chan struct{})
	s.followStop = stop
	s.mu.Unlock()
	go s.followLoop(hwnd, stop)
}

func (s *Sharer) followLoop(hwnd uintptr, stop chan struct{}) {
	t := time.NewTicker(200 * time.Millisecond)
	defer t.Stop()
	var last capture.Rect
	for {
		select {
		case <-stop:
			return
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		r, min, ok := capture.WindowRect(hwnd)
		if !ok {
			if !s.Paused() {
				log.Printf("跟随的窗口已关闭，暂停分享")
				s.SetPaused(true)
			}
			return
		}
		if min {
			if !s.Paused() {
				log.Printf("跟随的窗口已最小化，暂停分享")
				s.SetPaused(true)
			}
			continue
		}
		if s.Paused() {
			s.SetPaused(false)
		}
		s.mu.Lock()
		d := s.curDisplay
		s.mu.Unlock()
		rel := capture.Rect{X: r.X - d.X, Y: r.Y - d.Y, W: r.W, H: r.H}.Clamp(d)
		if rel.Empty() {
			continue
		}
		// 4px 的滞回：拖窗口时不要每 200ms 都重建采集源（重建有几百毫秒开销）。
		if absi(rel.X-last.X) <= 4 && absi(rel.Y-last.Y) <= 4 &&
			absi(rel.W-last.W) <= 4 && absi(rel.H-last.H) <= 4 {
			continue
		}
		s.SetRegion(rel)
		log.Printf("跟随窗口：区域 %dx%d @ (%d,%d)", rel.W, rel.H, rel.X, rel.Y)
		last = rel
	}
}

// StopFollow 停止跟随窗口。
func (s *Sharer) StopFollow() {
	s.mu.Lock()
	if s.followStop != nil {
		close(s.followStop)
		s.followStop = nil
	}
	s.mu.Unlock()
}

func absi(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// cursorLoop 以 ~60Hz 发光标位置（形状变化时补发位图）。
//
// 为什么必须独立于帧循环：桌面静止时 DXGI 不出帧，而鼠标还在动。
// 60Hz × 25 字节 ≈ 12 kbps，相比"每帧重编一条 32px 横带（2.6~3.6 Mbps）"
// 低了三个数量级。
func (s *Sharer) cursorLoop(ctx context.Context) {
	t := time.NewTicker(16 * time.Millisecond)
	defer t.Stop()
	const shapeResend = 2 * time.Second

	var (
		lastHandle  uint64          = ^uint64(0)
		rawShape    rtc.CursorShape // 原始位图（不缩放），比例变化时从它重算
		curShape    rtc.CursorShape // 按当前比例缩放后的版本（发出去的就是它）
		shapePct    int             // curShape 是按哪个比例算的
		haveShape   bool
		lastShapeAt time.Time
		lastHidden  bool
	)
	sendShape := func(peers []*rtc.Peer) {
		msg := rtc.MarshalCursorShape(curShape)
		lastShapeAt = time.Now()
		for _, p := range peers {
			_ = p.SendControl(rtc.CtlCursorShape, msg)
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if !s.cursorOn.Load() {
			continue
		}
		src := s.src.Load()
		if src == nil {
			continue
		}
		peers := s.cursorPeers()
		if len(peers) == 0 {
			continue
		}
		x, y, vis := capture.CursorPos()
		pct := s.Scale()
		h := uint64(capture.CursorHandle())
		// 形状变化才解码位图（解码要读 GDI 位图，不能 60Hz 做）。
		if vis && h != lastHandle {
			lastHandle = h
			if id, w, hh, hotX, hotY, pix, ok := capture.CursorBitmap(); ok && w > 0 && hh > 0 {
				rawShape = rtc.CursorShape{ID: id, W: w, H: hh, HotX: hotX, HotY: hotY, Pix: pix}
				curShape, shapePct = scaleShapeFor(rawShape, pct)
				haveShape = true
				sendShape(peers) // 形状立刻发；观看端按 id 缓存
			}
		}
		// 比例变化（自适应降分辨率）：同一个形状按新比例重算并立刻重发，
		// 否则观看端会拿"旧尺寸的光标"画在缩小后的画面上。
		if haveShape && pct != shapePct {
			curShape, shapePct = scaleShapeFor(rawShape, pct)
			sendShape(peers)
		}
		// 周期性重发：中途接入的观众端不需要额外的"请求形状"消息。
		if haveShape && time.Since(lastShapeAt) >= shapeResend {
			sendShape(peers)
		}
		if !vis {
			if lastHidden {
				continue
			}
			lastHidden = true
		} else {
			lastHidden = false
		}
		r := src.Region()
		s.mu.Lock()
		d := s.curDisplay
		s.mu.Unlock()
		// 坐标直接换算到**图像坐标**（降采样后的画面）：观看端只做减法，
		// 不需要知道缩放比例 —— 这样降分辨率不必改 ctl 线格式。
		sx := int64(x-(d.X+r.X)) * int64(pct) / 100
		sy := int64(y-(d.Y+r.Y)) * int64(pct) / 100
		pos := rtc.CursorPos{
			TS:  uint32(time.Now().UnixMilli()),
			ID:  curShape.ID,
			X:   int32(sx),
			Y:   int32(sy),
			RX:  0,
			RY:  0,
			Vis: vis && haveShape,
		}
		msg := rtc.MarshalCursorPos(pos)
		for _, p := range peers {
			_ = p.SendControl(rtc.CtlCursorPos, msg)
		}
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
			slot.drops.Add(1)
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
			// 有人顶不住了：记到**这位观众**头上（P0-4），全局计数只作为汇总。
			s.bpDrops.Add(1)
			slot.drops.Add(1)
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

// RequestTiles 请观众端"点名"的条带在下一帧补上（精确补发）。
//
// 与 RequestRepair 的分工：
//   - RequestTiles：观众报上来缺哪几条，就只补那几条 —— 一帧补齐、几十 KB；
//   - RequestRepair：不知道缺什么/缺一大片时，轮转把整屏铺一遍（十几帧）。
//
// 多个观众的请求会 OR 到一起（编码只做一次、扇出给所有人，
// 所以补发是"全局"的： someone 缺的条带，其他人会多收一份，代价可忽略）。
func (s *Sharer) RequestTiles(mask uint64) {
	if mask == 0 {
		return
	}
	// 全都要 = 等价于"整屏重来"，走轮转修复（摊到十几帧），
	// 别一发 2MB 突发把刚堵住的通道再堵一次。
	if n := s.enc.TileCount(); n > 0 && bits.OnesCount64(mask) >= n {
		s.RequestRepair()
		return
	}
	for {
		old := s.missingTiles.Load()
		if s.missingTiles.CompareAndSwap(old, old|mask) {
			return
		}
	}
}

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
	src := s.src.Load()
	if src == nil {
		return capture.Frame{}
	}
	r := src.Region()
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
	if src := s.src.Load(); src != nil {
		st.Stats = src.Stats()
	}
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
	// 光标流：静止桌面时媒体通道不产帧，鼠标位置只能靠这条 60Hz 的 ctl 通道。
	go s.cursorLoop(ctx)
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
		// P1-5：降采样比例也由自适应控制器给（0 档恒为 100，零成本）。
		if sc := int(s.adapt.wantScale.Load()); sc > 0 && sc != s.Scale() {
			s.SetScale(sc)
		}
		// 采集源可能正在被重建（换区域/换屏，且帧率需要跟着变）：这期间短暂没有源，
		// 观众看到的是上一帧（不会黑），等新源就绪即可。
		src := s.src.Load()
		if src == nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(20 * time.Millisecond):
			}
			continue
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
			_, _ = src.WaitFrame(c)
			cancel()
			continue
		}
		f, err := src.WaitFrame(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// 等待超时 = 桌面没变化。发一次保活心跳，让观众端知道连接还活着。
			if time.Since(lastBeat) >= s.cfg.IdleHeartbeat && s.PeerCount() > 0 {
				if hb := src.Last(); !hb.Empty() {
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
	// 观众端点名要补的条带（精确补发）：取走一次即清空，
	// 编码器会在这一帧里额外带上它们（见 codec.Encoder.RequestTiles）。
	if m := s.missingTiles.Swap(0); m != 0 {
		enc.RequestTiles(m)
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
	if pct := int(s.scalePct.Load()); pct > 0 && pct < 100 {
		if small, ow, oh, err := codec.DownscaleBGRA(f.Pix, f.W, f.H, pct, 100); err == nil {
			f.Pix, f.W, f.H = small, ow, oh
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

	if len(slots) == 0 {
		// 没人看的时候只剩本地回显要做。
		s.runPreview(out, f)
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
	// 本地回显**排在唤醒之后**（P1-6）：先把这一帧交给发送 goroutine，
	// 再做预览的缩放/绘制。预览 1~5ms 的耗时因此不再算进观众看到的延迟。
	s.runPreview(out, f)
	return nil
}

// runPreview 刷新分享端的本地回显（可选回调）。
func (s *Sharer) runPreview(out *codec.Frame, f capture.Frame) {
	if s.cfg.OnFrame != nil {
		s.cfg.OnFrame(out, f)
	}
}
