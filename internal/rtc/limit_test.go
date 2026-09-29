package rtc

import "testing"

// TestSendLimit 钉住背压阈值的语义：按帧大小自适应，约一帧半。
//
// 为什么值得单测：这里曾经是固定 8MB（≈4 个 2K 全量帧）。这个数字决定了
// "允许落后几帧"，也就是卡顿时延迟的上限 —— 它一旦被改回大值，
// 表现是"平时看不出、一拥塞就延迟几百毫秒"，靠人眼是回归不出来的。
func TestSendLimit(t *testing.T) {
	cases := []struct {
		name  string
		frame int
		want  uint64
	}{
		{"小帧走下限", 8 << 10, minBuffered},
		{"增量帧走下限", 200 << 10, minBuffered},
		{"典型增量（1 帧半仍小于下限）", 300 << 10, minBuffered},
		{"接近下限的帧取一帧半", 400 << 10, 600 << 10},
		{"2K 全量取一帧半", 2 << 20, 3 << 20},
		{"超大帧走上限", 8 << 20, maxBuffered},
	}
	for _, c := range cases {
		if got := SendLimit(c.frame); got != c.want {
			t.Errorf("%s: SendLimit(%d) = %d, 期望 %d", c.name, c.frame, got, c.want)
		}
	}
	// 上限必须远小于旧的 8MB：这条断言就是"别再改回大值"的守卫。
	if maxBuffered >= 8<<20 {
		t.Fatalf("maxBuffered = %d，已回到量级错误的区间（旧值 8MB ≈ 落后 4 帧）", maxBuffered)
	}
}
