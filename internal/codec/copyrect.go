// 滚动位移检测（P2-1 CopyRect 的第一块拼图）。
//
// 滚屏时画面上绝大部分像素只是"往上/下挪了几行"，但增量编码会把整屏条带重编一遍。
// 如果能认出这个位移，只需要发"位移量 + 新露出的那几条带"，码率能掉一个数量级。
//
// 这里只做**检测**：按行哈希找出最可能的 dy 与匹配比例。真正的"只发位移"需要
// 线格式支持（帧头带 shift 字段），是下一步。
package codec

import "hash/fnv"

// RowHashes 返回每隔 step 行的哈希（step<1 取 1）。
//
// 逐行哈希 2K 一帧要扫 20MB；按 4 行抽样只要 5MB，而判定位移时少几行不影响准确率。
func RowHashes(pix []byte, w, h, step int) []uint64 {
	if step < 1 {
		step = 1
	}
	if w <= 0 || h <= 0 || len(pix) < w*h*4 {
		return nil
	}
	out := make([]uint64, 0, h/step+1)
	hsh := fnv.New64a()
	for y := 0; y < h; y += step {
		row := pix[y*w*4 : (y+1)*w*4]
		hsh.Reset()
		_, _ = hsh.Write(row)
		out = append(out, hsh.Sum64())
	}
	return out
}

// DetectShift 在 cur 相对 prev 的行哈希里找竖直位移 dy（正数 = 内容下移）。
//
// maxShift 与返回值都是**真实像素行**；step 必须与 RowHashes 用的一致。
// 返回的 ratio 是"匹配上的抽样行占比"。判据建议：ratio ≥ 0.85 且 |dy|≥2 才认
// （小于 2 行的位移直接重编更划算；阈值太低会把"画面刚好相似"误判成滚动）。
func DetectShift(prevH, curH []uint64, maxShift, step int) (dy int, ratio float64, ok bool) {
	n := len(prevH)
	if step < 1 {
		step = 1
	}
	if n == 0 || len(curH) != n || maxShift < 1 {
		return 0, 0, false
	}
	maxD := maxShift / step
	if maxD < 1 {
		maxD = 1
	}
	bestDy, bestHit := 0, 0
	for d := -maxD; d <= maxD; d++ {
		hit, total := 0, 0
		for i := 0; i < n; i++ {
			src := i - d // cur[i] 应当等于 prev[i-d]
			if src < 0 || src >= n {
				continue
			}
			total++
			if curH[i] == prevH[src] {
				hit++
			}
		}
		if total == 0 {
			continue
		}
		if hit > bestHit {
			bestHit, bestDy = hit, d
		}
	}
	return bestDy * step, float64(bestHit) / float64(n), bestHit > 0
}
