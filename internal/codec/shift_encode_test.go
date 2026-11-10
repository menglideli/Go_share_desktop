package codec

import "testing"

// TestEncodeShift 验证 P2-1 的发送端：整屏下移 40 行时，
// 编码器应识别出 Shift=40，并且只重编新露出的那几条带。
func TestEncodeShift(t *testing.T) {
	const w, h = 320, 600
	base := make([]byte, w*h*4)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := (y*w + x) * 4
			// 每行不同灰度 + 一点横向变化，避免不同行哈希相同。
			base[i], base[i+1], base[i+2], base[i+3] = byte(y), byte(y)^byte(x), byte(y/2), 255
		}
	}
	cur := make([]byte, len(base))
	const dy = 40
	copy(cur[dy*w*4:], base[:(h-dy)*w*4])
	for y := 0; y < dy; y++ {
		for x := 0; x < w; x++ {
			i := (y*w + x) * 4
			cur[i], cur[i+1], cur[i+2], cur[i+3] = 250, 240, 230, 255
		}
	}

	enc := NewEncoder(Config{Quality: 70, TileHeight: 32, Dirty: true})
	f1, err := enc.Encode(base, w, h)
	if err != nil || f1.Full != true {
		t.Fatalf("首帧应为全量: err=%v full=%v", err, f1.Full)
	}
	f2, err := enc.Encode(cur, w, h)
	if err != nil {
		t.Fatal(err)
	}
	if f2.Shift != dy {
		t.Fatalf("Shift = %d，期望 %d（检测不到滚动）", f2.Shift, dy)
	}
	if f2.DirtyTiles > 4 {
		t.Fatalf("滚动后重编了 %d 条带（期望 ≤4，说明参考帧没跟着滚）", f2.DirtyTiles)
	}

	// 端到端：观众的画布滚一下再贴条带，必须与源逐像素一致。
	dec := NewDecoder(0)
	if _, err := dec.Decode(f1); err != nil {
		t.Fatal(err)
	}
	// 首帧是全量 JPEG（有损），所以基准要用"解码后的首帧"，不能拿原始源比。
	img := dec.Image()
	before := make([]byte, len(img.Pix))
	copy(before, img.Pix)
	if _, err := dec.Decode(f2); err != nil {
		t.Fatal(err)
	}
	// 只对"滚动搬过来的部分"做逐像素断言：新露出的顶部两条带是 JPEG 重编的（有损）。
	for y := 64; y < h-dy; y++ {
		for x := 0; x < w; x++ {
			o := y*img.Stride + x*4
			ob := (y - dy) * img.Stride
			if img.Pix[o] != before[ob+x*4] || img.Pix[o+1] != before[ob+x*4+1] || img.Pix[o+2] != before[ob+x*4+2] {
				t.Fatalf("(%d,%d) 滚动搬移后不一致：got %v want %v", x, y,
					img.Pix[o:o+3], before[ob+x*4:ob+x*4+3])
			}
		}
	}
}
