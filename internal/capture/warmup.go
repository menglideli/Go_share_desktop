package capture

import (
	"context"
	"time"
)

// Warmup 丢弃采集流建立初期可能未初始化的黑帧（R25）。
//
// 实测：DXGI Duplication 建立后的首帧 3 次里有 2 次是纯黑（非黑占比 0.0%、颜色数 1），
// 第二帧起立刻正常 —— 是流的初始化竞态，不是采集坏了。
//
// ⚠️ 不能"无条件丢首帧"：DXGI 是**变化驱动**的，桌面静止时可能几秒才出一帧，
// 丢掉它就等于让观众一直黑着等到桌面有变化为止（实测静止 2s 只出 1 帧）。
// 所以策略是「最多丢 maxDrop 帧 + 有限超时」，兜底原则是：
// **宁可给一帧可能是黑的，也不能不给**。
//
// 返回 true 表示等到了一帧确定非黑的画面。
func (s *Source) Warmup(ctx context.Context, maxDrop int, timeout time.Duration) (bool, error) {
	if maxDrop <= 0 {
		maxDrop = 3
	}
	if timeout <= 0 {
		timeout = 600 * time.Millisecond
	}
	deadline := time.Now().Add(timeout)
	s.warm = true
	defer func() { s.warm = false }()

	for dropped := 0; dropped < maxDrop; dropped++ {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		remain := time.Until(deadline)
		if remain <= 0 {
			return false, nil
		}
		wctx, cancel := context.WithTimeout(ctx, remain)
		f, err := s.WaitFrame(wctx)
		cancel()
		if err != nil {
			// 等不到帧 = 桌面静止（或 ctx 结束）。这不是故障，直接结束预热，
			// 剩下的交给心跳：静止时由 Last() 保活。
			return false, nil
		}
		if !mostlyBlack(f.Pix) {
			return true, nil
		}
		dropped++
	}
	return false, nil
}

// IsBlack 报告这一帧是否几乎全黑（预热判定与诊断都用它，所以导出）。
func (f Frame) IsBlack() bool { return mostlyBlack(f.Pix) }

// mostlyBlack 判断一帧是否几乎全黑。
//
// 判据是"非黑像素占比 < 0.5%"而不是"平均亮度低"：深色壁纸/夜间模式桌面平均亮度也低，
// 但它有大量非黑像素，用亮度判会把它误判成坏帧。
func mostlyBlack(pix []byte) bool {
	if len(pix) < 4 {
		return true
	}
	// 抽样：整帧几百万像素没必要全扫，均匀取 ~2000 个点足够区分
	// "纯黑"与"深色但有内容"。素数步长避免与行结构共振。
	pixels := len(pix) / 4
	step := pixels / 2000
	if step < 1 {
		step = 1
	}
	n, nb := 0, 0
	for p := 0; p < pixels; p += step {
		i := p * 4
		lum := (77*int(pix[i+2]) + 150*int(pix[i+1]) + 29*int(pix[i])) >> 8
		if lum > 8 {
			nb++
		}
		n++
	}
	if n == 0 {
		return true
	}
	return float64(nb)/float64(n) < 0.005
}
