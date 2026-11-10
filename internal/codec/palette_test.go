package codec

import "testing"

// TestPaletteTileRoundTrip 验证 P2-2 第二步：≤16 色的条带用"调色板+RLE"编码，
// 解码后必须**逐像素**还原（这是无损编码，不允许有误差）。
func TestPaletteTileRoundTrip(t *testing.T) {
	const w, h = 320, 64
	img := make([]byte, w*h*4)
	cols := [][4]byte{{0, 0, 0, 255}, {255, 255, 255, 255}, {30, 120, 200, 255}, {200, 60, 30, 255}}
	for y := 0; y < h; y++ {
		c := cols[(y/16)%len(cols)]
		for x := 0; x < w; x++ {
			i := (y*w + x) * 4
			img[i], img[i+1], img[i+2], img[i+3] = c[0], c[1], c[2], c[3]
		}
	}
	enc := NewEncoder(Config{Quality: 75, TileHeight: 32, Dirty: true})
	f, err := enc.Encode(img, w, h)
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	used := false
	for _, tl := range f.Tiles {
		if tl.Enc == encPalette {
			used = true
		}
	}
	if !used {
		t.Fatal("4 色条带没有走调色板编码（预期 H.264? 不，预期 encPalette）")
	}
	dec := NewDecoder(0)
	out, err := dec.Decode(f)
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := (y*w + x) * 4
			o := y*out.Stride + x*4
			if out.Pix[o] != img[i+2] || out.Pix[o+1] != img[i+1] || out.Pix[o+2] != img[i] {
				t.Fatalf("像素 (%d,%d) 不一致: got %v want %v", x, y,
					[]byte{out.Pix[o], out.Pix[o+1], out.Pix[o+2]}, []byte{img[i+2], img[i+1], img[i]})
			}
		}
	}
}
