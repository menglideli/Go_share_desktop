package rtc

import "testing"

// TestClockOffset 用手工构造的来回时间戳验证偏移/RTT 的算法。
//
// 场景：分享端时钟比本端快 3000ms，去程 2ms、回程 2ms、对端处理 1ms。
// 正确的偏移必须是 +3000，RTT 必须是 4ms（不含对端处理时间）。
func TestClockOffset(t *testing.T) {
	var c Clock
	t0 := uint32(1000)  // 本端发出
	t1 := uint32(4002)  // 分享端收到 = 1000 + 2 + 3000
	t2 := uint32(4003)  // 分享端回复
	t3 := uint32(1005)  // 本端收到 = 1000 + 2 + 1 + 2

	if _, ok := c.Offset(); ok {
		t.Fatal("只有一个样本时不应判定为已同步")
	}
	c.Observe(t0, t1, t2, t3)
	c.Observe(t0, t1, t2, t3)
	off, ok := c.Offset()
	if !ok {
		t.Fatal("两个样本后应当判定为已同步")
	}
	if off != 3000 {
		t.Fatalf("偏移 = %d，期望 3000", off)
	}
	if rtt := c.RTT(); rtt != 4 {
		t.Fatalf("RTT = %d，期望 4", rtt)
	}
}

// TestClockOffsetWraps 验证"本端时钟接近 2^32 回绕"时偏移依然正确。
// 32 位毫秒每 49.7 天回绕一次，长时间挂着共享正好会碰到。
func TestClockOffsetWraps(t *testing.T) {
	var c Clock
	t0 := uint32(0xFFFFFFF0) // 本端：再过 16ms 就回绕
	t1 := uint32(0x00000022) // 分享端收到（换算后与 t0 相差 0x32 = 50ms）
	t2 := uint32(0x00000023)
	t3 := uint32(0xFFFFFFF5) // 本端收到：t0 + 5

	c.Observe(t0, t1, t2, t3)
	c.Observe(t0, t1, t2, t3)
	off, ok := c.Offset()
	if !ok {
		t.Fatal("应当判定为已同步")
	}
	// ((0x32) + (0x23 - 0xFFFFFFF5=0x2E)) / 2 = (50+46)/2 = 48（去回程不对称 2ms）
	if off < 47 || off > 51 {
		t.Fatalf("回绕场景偏移 = %d，期望 48±2", off)
	}
}

// TestWaitMS 验证"还要等多久"的换算与回绕。
func TestWaitMS(t *testing.T) {
	cases := []struct {
		name     string
		now, ts  uint32
		off      uint32
		d        int64
		wantSign int // -1 已晚 / 0 正好 / 1 还要等
	}{
		{"本端与分享端同钟，正点", 1000, 920, 0, 80, 0},
		{"早到 60ms 需要等", 1000, 920, 0, 140, 1},
		{"晚到 40ms 立即上屏", 1000, 920, 0, 60, -1},
		{"分享端快 3000ms", 1000, 3920, 3000, 80, 0},
		{"跨 2^32 回绕", 0xFFFFFFF8, 0xFFFFFFF0, 0, 100, 1},
		{"回绕后已过", 0x00000008, 0xFFFFFFF0, 0, 0, -1},
	}
	for _, c := range cases {
		got := waitMS(c.now, c.ts, c.off, c.d)
		sign := 0
		switch {
		case got > 0:
			sign = 1
		case got < 0:
			sign = -1
		}
		if sign != c.wantSign {
			t.Errorf("%s: waitMS = %d，期望符号 %d", c.name, got, c.wantSign)
		}
	}
}
