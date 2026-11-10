package codec

import "testing"

// TestShiftScroll 验证 P2-1 的接收端行为：帧带 Shift 时先把画布整体滚动，
// 再贴本帧条带。用"每行一个灰度"的画面，滚 8 行后第 10 行应出现在第 18 行。
func TestShiftScroll(t *testing.T) {
	const w, h = 64, 96
	img := make([]byte, w*h*4)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := (y*w + x) * 4
			img[i], img[i+1], img[i+2], img[i+3] = byte(y), byte(y), byte(y), 255
		}
	}
	enc := NewEncoder(Config{Quality: 75, TileHeight: 32, Dirty: true})
	f1, err := enc.Encode(img, w, h)
	if err != nil {
		t.Fatal(err)
	}
	dec := NewDecoder(0)
	if _, err := dec.Decode(f1); err != nil {
		t.Fatal(err)
	}
	canvas := dec.Image()
	stride := canvas.Stride
	rowOf := func(y int) []byte { return canvas.Pix[y*stride : y*stride+w*4] }
	before := append([]byte(nil), rowOf(10)...)

	// 第二帧只声明位移（不带条带）：画布应整体下移 8 行。
	if _, err := dec.Decode(&Frame{Seq: f1.Seq + 1, W: w, H: h, TileH: 32, TotalTiles: f1.TotalTiles, Shift: 8}); err != nil {
		t.Fatal(err)
	}
	after := rowOf(18)
	for i := 0; i < w*4; i += 4 {
		if after[i] != before[i] || after[i+1] != before[i+1] {
			t.Fatalf("滚动画布后第 18 行与原来的第 10 行不一致（偏移 %d）", i)
		}
	}
}
