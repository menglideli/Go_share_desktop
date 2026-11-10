package codec

import "testing"

// TestDetectShift 验证"整屏下移 k 行"能被认出来，且普通内容变化不会被误认。
func TestDetectShift(t *testing.T) {
	const w, h = 320, 200
	base := make([]byte, w*h*4)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := (y*w + x) * 4
			base[i], base[i+1], base[i+2], base[i+3] = byte(y), byte(x), byte(y^16), 255
		}
	}
	shift := func(src []byte, k int) []byte {
		out := make([]byte, len(src))
		// 内容下移 k 行：上面补新内容（用固定图案，模拟新露出的区域）。
		for y := 0; y < h; y++ {
			sy := y - k
			if sy < 0 || sy >= h {
				continue
			}
			copy(out[y*w*4:(y+1)*w*4], src[sy*w*4:(sy+1)*w*4])
		}
		for y := 0; y < k && y < h; y++ {
			for x := 0; x < w; x++ {
				i := (y*w + x) * 4
				out[i], out[i+1], out[i+2], out[i+3] = 9, 8, 7, 255
			}
		}
		return out
	}
	prev := RowHashes(base, w, h, 1)
	for _, k := range []int{2, 5, 12} {
		cur := RowHashes(shift(base, k), w, h, 1)
		dy, ratio, ok := DetectShift(prev, cur, 40, 1)
		if !ok || dy != k || ratio < 0.85 {
			t.Errorf("下移 %d 行：dy=%d ratio=%.2f ok=%v，期望 dy=%d ratio≥0.85", k, dy, ratio, ok, k)
		}
	}
	// 完全不同的内容：不应给出高匹配。
	other := make([]byte, len(base))
	for i := range other {
		other[i] = byte(i * 31)
	}
	_, ratio, _ := DetectShift(prev, RowHashes(other, w, h, 1), 40, 1)
	if ratio > 0.5 {
		t.Errorf("随机内容匹配率 %.2f，过高（会误判成滚动）", ratio)
	}
}
