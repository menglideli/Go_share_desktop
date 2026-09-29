// 自适应质量控制器的确定性校验。
//
// 为什么不走真实 e2e：背压要在真实弱网/多人下才偶发出现，没法当回归门禁。
// 这里直接驱动 adaptTick 注入"背压丢帧数 / 缓冲水位"，验证升降档策略本身：
// 降档要快（有压力立刻降、首次不等冷却）、回升要慢（连续 10 秒干净才升一格）、
// 到顶/到底不越界、水位处于中间地带时既不降也不升（避免临界点震荡）。
package pipeline

import (
	"testing"
	"time"
)

func TestAdaptMaxLevel(t *testing.T) {
	cases := []struct {
		name  string
		base  Preset
		want  int
		wantQ int // 顶格参数
		wantF int
	}{
		// 75→67→59→51→43（4 级质量，再降 35<42 停）+ 不限→30→15（2 级帧率）
		{"最大", PresetMax, 6, 43, 15},
		// 80→72→64→56→48（4 级，40<42 停）+ 30→15（1 级）
		{"流畅30", Preset30, 5, 48, 15},
		// 同最大档质量 4 级 + 60→30→15（2 级）
		{"流畅60", Preset60, 6, 43, 15},
		// 65→57→49（2 级，41<42 停）+ 不限→30→15（2 级）
		{"急速", PresetTurbo, 4, 49, 15},
	}
	for _, c := range cases {
		if got := adaptMaxLevel(c.base); got != c.want {
			t.Errorf("%s: adaptMaxLevel = %d, 期望 %d", c.name, got, c.want)
		}
		q, f := adaptParams(c.base, c.want)
		if q != c.wantQ || f != c.wantF {
			t.Errorf("%s: 顶格参数 = (q%d, f%d), 期望 (q%d, f%d)", c.name, q, f, c.wantQ, c.wantF)
		}
		// 超过顶格不许再变（幂等）
		q2, f2 := adaptParams(c.base, c.want+3)
		if q2 != c.wantQ || f2 != c.wantF {
			t.Errorf("%s: 超顶格参数 = (q%d, f%d), 应保持在 (q%d, f%d)", c.name, q2, f2, c.wantQ, c.wantF)
		}
	}
	// 0 档必须恒等于基准档位（满血）
	for _, p := range Presets {
		q, f := adaptParams(p, 0)
		if q != p.Quality || f != p.MaxFPS {
			t.Errorf("%s: 0 档参数 = (q%d, f%d), 应等于基准 (q%d, f%d)", p.Name, q, f, p.Quality, p.MaxFPS)
		}
	}
}

// newAdaptTestSharer 构造一个不带采集源、只用于驱动控制器的 Sharer。
func newAdaptTestSharer(base Preset) *Sharer {
	sh := &Sharer{cfg: Config{Preset: base}}
	sh.resetAdapt(base)
	return sh
}

func TestAdaptTickDownUp(t *testing.T) {
	sh := newAdaptTestSharer(PresetMax)

	// 有背压：立即降一档
	sh.adaptTick(3, 0)
	if sh.adapt.level != 1 {
		t.Fatalf("首次背压后 level = %d, 期望 1", sh.adapt.level)
	}
	if q, _ := adaptParams(PresetMax, 1); int(sh.adapt.wantQuality.Load()) != q {
		t.Fatalf("降档后 wantQuality = %d, 期望 %d", sh.adapt.wantQuality.Load(), q)
	}

	// 冷却期内继续背压：不许连降
	sh.adaptTick(3, 0)
	if sh.adapt.level != 1 {
		t.Fatalf("冷却期内 level = %d, 期望仍为 1", sh.adapt.level)
	}

	// 冷却过后继续背压：再降
	sh.adaptMu.Lock()
	sh.adapt.lastDown = time.Now().Add(-3 * time.Second)
	sh.adaptMu.Unlock()
	sh.adaptTick(3, 0)
	if sh.adapt.level != 2 {
		t.Fatalf("冷却后 level = %d, 期望 2", sh.adapt.level)
	}

	// 无背压 9 秒：不升；第 10 秒：升一格
	for i := 0; i < adaptUpAfterTicks-1; i++ {
		sh.adaptTick(0, 0)
	}
	if sh.adapt.level != 2 {
		t.Fatalf("无背压 %d 秒后 level = %d, 期望仍为 2", adaptUpAfterTicks-1, sh.adapt.level)
	}
	sh.adaptTick(0, 0)
	if sh.adapt.level != 1 {
		t.Fatalf("无背压 %d 秒后 level = %d, 期望回升到 1", adaptUpAfterTicks, sh.adapt.level)
	}

	// 回升途中又来背压：立即打断回升节奏（cleanTicks 清零），并按冷却再降。
	// 真实时序里"回升"距离上次降档至少 10 秒（10 个 clean tick），
	// 测试是快放的，要把 lastDown 手动拨回去模拟这个时间间隔。
	sh.adaptMu.Lock()
	sh.adapt.lastDown = time.Now().Add(-3 * time.Second)
	sh.adaptMu.Unlock()
	sh.adaptTick(1, 0)
	if sh.adapt.cleanTicks != 0 {
		t.Fatalf("背压后 cleanTicks = %d, 期望清零", sh.adapt.cleanTicks)
	}
	if sh.adapt.level != 2 {
		t.Fatalf("背压打断后 level = %d, 期望降回 2", sh.adapt.level)
	}
}

// TestAdaptWatermark 验证"缓冲水位"这条连续信号：它是"提前降档"的依据。
//
// 为什么值得单测：只看丢帧数的旧策略，要等阈值被击穿才反应 ——
// 用户的感受就是"卡住了它才慢慢限速"。水位过半就该降，且首次不受冷却限制。
func TestAdaptWatermark(t *testing.T) {
	sh := newAdaptTestSharer(PresetMax)

	// 水位过半、一次丢帧都没有：也必须降档
	sh.adaptTick(0, adaptPressurePct)
	if sh.adapt.level != 1 {
		t.Fatalf("水位 %d%% 后 level = %d, 期望 1（水位过半即降档）", adaptPressurePct, sh.adapt.level)
	}

	// 中间地带（>=clean、<pressure）：既不降也不升
	mid := (adaptPressurePct + adaptCleanPct) / 2
	for i := 0; i < adaptUpAfterTicks*2; i++ {
		sh.adaptTick(0, mid)
	}
	if sh.adapt.level != 1 {
		t.Fatalf("水位持续 %d%% 时 level = %d, 期望保持不变（临界点不震荡）", mid, sh.adapt.level)
	}

	// 落到低位才算干净秒，连续够久才回升
	for i := 0; i < adaptUpAfterTicks-1; i++ {
		sh.adaptTick(0, adaptCleanPct-1)
	}
	if sh.adapt.level != 1 {
		t.Fatalf("低位 %d 秒后 level = %d, 期望仍为 1", adaptUpAfterTicks-1, sh.adapt.level)
	}
	sh.adaptTick(0, 0)
	if sh.adapt.level != 0 {
		t.Fatalf("低位 %d 秒后 level = %d, 期望回到 0", adaptUpAfterTicks, sh.adapt.level)
	}
}

func TestAdaptTickCeilingAndFloor(t *testing.T) {
	sh := newAdaptTestSharer(PresetMax)

	// 持续背压打到顶格，不许越界
	for i := 0; i < sh.adapt.maxLevel+3; i++ {
		sh.adaptMu.Lock()
		sh.adapt.lastDown = time.Now().Add(-3 * time.Second)
		sh.adaptMu.Unlock()
		sh.adaptTick(9, 0)
	}
	if sh.adapt.level != sh.adapt.maxLevel {
		t.Fatalf("持续背压后 level = %d, 期望顶格 %d", sh.adapt.level, sh.adapt.maxLevel)
	}
	q, f := int(sh.adapt.wantQuality.Load()), int(sh.adapt.wantMaxFPS.Load())
	wq, wf := adaptParams(PresetMax, sh.adapt.maxLevel)
	if q != wq || f != wf {
		t.Fatalf("顶格 want = (q%d, f%d), 期望 (q%d, f%d)", q, f, wq, wf)
	}

	// 持续无背压回到 0 档，不许变负
	for i := 0; i < sh.adapt.maxLevel+2; i++ {
		for j := 0; j < adaptUpAfterTicks; j++ {
			sh.adaptTick(0, 0)
		}
	}
	if sh.adapt.level != 0 {
		t.Fatalf("持续无背压后 level = %d, 期望回到 0", sh.adapt.level)
	}
	if q := int(sh.adapt.wantQuality.Load()); q != PresetMax.Quality {
		t.Fatalf("回到 0 档后 wantQuality = %d, 期望 %d", q, PresetMax.Quality)
	}
}

func TestAdaptResetOnPresetChange(t *testing.T) {
	sh := newAdaptTestSharer(PresetMax)
	sh.adaptTick(3, 0)
	sh.adaptTick(0, 0) // 确认不在 0 档
	if sh.adapt.level == 0 {
		t.Fatal("前置条件失败：应已降档")
	}
	// 换档位必须重置：旧档序号套新基准是错误参数
	sh.resetAdapt(Preset30)
	if sh.adapt.level != 0 || sh.adapt.maxLevel != adaptMaxLevel(Preset30) {
		t.Fatalf("resetAdapt 后 level=%d maxLevel=%d, 期望 0 / %d",
			sh.adapt.level, sh.adapt.maxLevel, adaptMaxLevel(Preset30))
	}
	if q := int(sh.adapt.wantQuality.Load()); q != Preset30.Quality {
		t.Fatalf("resetAdapt 后 wantQuality = %d, 期望 %d", q, Preset30.Quality)
	}
}

// TestReportWatermarkKeepsPeak 验证水位聚合是"取这一秒内的峰值"而不是"最后一次"。
//
// 为什么重要：扇出侧每个观众各报一次水位，采样点在发送前后都有。
// 若用"最后一次"覆盖，一次偶然的低水位就能把刚刚过半的压力抹掉，
// 控制器就永远慢半拍。
func TestReportWatermarkKeepsPeak(t *testing.T) {
	sh := &Sharer{}
	sh.reportWatermark(30)
	sh.reportWatermark(80)
	sh.reportWatermark(10)
	if got := sh.bpWatermark.Swap(0); got != 80 {
		t.Fatalf("水位聚合 = %d, 期望保留峰值 80", got)
	}
	// 越界要夹住
	sh.reportWatermark(250)
	if got := sh.bpWatermark.Swap(0); got != 100 {
		t.Fatalf("水位上界 = %d, 期望 100", got)
	}
	sh.reportWatermark(-5)
	if got := sh.bpWatermark.Swap(0); got != 0 {
		t.Fatalf("水位下界 = %d, 期望 0", got)
	}
}
