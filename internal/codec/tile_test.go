package codec

import "testing"

// TestTileHeight 钉住"按目标条带高度切条带"的语义（P1-1）。
//
// 为什么值得单测：条带高度决定"光标闪一下要重编多少像素"。默认按 CPU 核数
// 分条带时，1080p 与 2K 会共用 ~20 条，2K 下一条横带就有 96px 高、28 万像素，
// 而实际变化可能只有 1000 像素（重编放大 400 倍）。这个换算一旦被改回
// "按核数"，表现只是"码率偏高"，肉眼和现有自检都很难发现。
func TestTileHeight(t *testing.T) {
	cases := []struct {
		h, target, wantTiles, wantTileH int
	}{
		{1800, 32, 57, 32}, // 2K
		{1080, 32, 34, 32}, // 1080p
		{2160, 32, 45, 48}, // 4K：条带数被 uint64 位图上限（64）夹到 48px
	}
	for _, c := range cases {
		enc := NewEncoder(Config{Quality: 75, TileHeight: c.target, Dirty: true})
		img := make([]byte, 320*c.h*4)
		f, err := enc.Encode(img, 320, c.h)
		if err != nil {
			t.Fatalf("h=%d 编码失败: %v", c.h, err)
		}
		if enc.TileCount() != c.wantTiles || f.TileH != c.wantTileH {
			t.Errorf("h=%d: 条带 %d 个×%dpx，期望 %d 个×%dpx",
				c.h, enc.TileCount(), f.TileH, c.wantTiles, c.wantTileH)
		}
		if f.TileH%16 != 0 {
			t.Errorf("h=%d: 条带高度 %d 未对齐到 MCU（16）", c.h, f.TileH)
		}
	}
}

// TestDirtyTileGranularity 验证"24px 的光标变化只脏 1~2 条 32px 的条带"。
func TestDirtyTileGranularity(t *testing.T) {
	const w, h = 320, 1800
	enc := NewEncoder(Config{Quality: 75, TileHeight: 32, Dirty: true})
	img := make([]byte, w*h*4)
	for i := 3; i < len(img); i += 4 {
		img[i] = 255
	}
	if _, err := enc.Encode(img, w, h); err != nil {
		t.Fatalf("首帧失败: %v", err)
	}
	// 在 y=805 处画一个 24×24 的白块：只落在条带 25（800~831）。
	for y := 805; y < 829; y++ {
		for x := 100; x < 124; x++ {
			p := (y*w + x) * 4
			img[p], img[p+1], img[p+2] = 255, 255, 255
		}
	}
	f, err := enc.Encode(img, w, h)
	if err != nil {
		t.Fatalf("增量帧失败: %v", err)
	}
	if n := len(f.Tiles); n == 0 || n > 2 {
		t.Fatalf("24px 变化脏了 %d 条条带，期望 1~2 条", n)
	}
	if got := f.Tiles[0].Index; got != 805/32 {
		t.Fatalf("脏条带下标 = %d，期望 %d", got, 805/32)
	}
}
