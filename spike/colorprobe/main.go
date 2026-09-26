// colorprobe 用纯色块过一遍完整编解码链路，验证颜色通道是否保真。
//
// 背景：用户实测观看端"红色显示成绿色、绿色显示成红色"，
// 而 codeccheck 14/14 全绿 —— 因为既有像素证据全部只看亮度/非黑占比，
// 对 R/G 通道对调完全无感（色盲）。本探针直接断言每个色块的 dominant channel。
package main

import (
	"fmt"
	"os"

	"goshare/internal/codec"
)

func main() {
	const w, h = 512, 64 // 8 个 64x64 色块，条带高 64 正好每块落在同一条带

	type swatch struct {
		name    string
		r, g, b uint8
	}
	swatches := []swatch{
		{"纯红", 255, 0, 0},
		{"纯绿", 0, 255, 0},
		{"纯蓝", 0, 0, 255},
		{"纯黄", 255, 255, 0},
		{"纯青", 0, 255, 255},
		{"品红", 255, 0, 255},
		{"深红", 200, 16, 16}, // 桌面常见的"接近纯红"的图标色
		{"纯白", 255, 255, 255},
	}

	// 源帧：紧凑 BGRA
	bgra := make([]byte, w*h*4)
	for i := 0; i < w*h; i++ {
		x := i % w
		s := swatches[x/64]
		p := i * 4
		bgra[p], bgra[p+1], bgra[p+2], bgra[p+3] = s.b, s.g, s.r, 255
	}

	enc := codec.NewEncoder(codec.Config{Quality: 95, Tiles: 1, Dirty: false})
	f, err := enc.Encode(bgra, w, h)
	if err != nil {
		fmt.Println("编码失败:", err)
		os.Exit(1)
	}
	fmt.Printf("编码：%d 条带 %d 字节（全量=%v）\n", len(f.Tiles), f.Bytes, f.Full)

	// 走一遍线格式再回来，模拟真实链路
	wire, err := f.Marshal()
	if err != nil {
		fmt.Println("序列化失败:", err)
		os.Exit(1)
	}
	f2, err := codec.UnmarshalFrame(wire)
	if err != nil {
		fmt.Println("反序列化失败:", err)
		os.Exit(1)
	}

	dec := codec.NewDecoder(0)
	img, err := dec.Decode(f2)
	if err != nil {
		fmt.Println("解码失败:", err)
		os.Exit(1)
	}

	fail := 0
	for k, s := range swatches {
		// 取色块中心像素
		cx, cy := k*64+32, 32
		_ = cy
		p := (cy*w + cx) * 4
		r, g, b := img.Pix[p], img.Pix[p+1], img.Pix[p+2]
		// dominant channel 检查：期望通道必须显著高于另外两个
		ok := true
		want := map[string]int{"r": int(s.r), "g": int(s.g), "b": int(s.b)}
		got := map[string]int{"r": int(r), "g": int(g), "b": int(b)}
		for ch, wv := range want {
			diff := got[ch] - wv
			if diff < -24 || diff > 24 { // JPEG q95 允许小误差，通道对调差 255 必然超阈
				ok = false
			}
		}
		mark := "OK "
		if !ok {
			mark = "FAIL"
			fail++
		}
		fmt.Printf("%s %s：期望 RGB(%3d,%3d,%3d) 实际 RGB(%3d,%3d,%3d)\n",
			mark, s.name, s.r, s.g, s.b, r, g, b)
	}
	if fail > 0 {
		fmt.Printf("\n结果：%d/%d 色块颜色不对 —— 编解码链路存在通道级缺陷\n", fail, len(swatches))
		os.Exit(1)
	}
	fmt.Printf("\n结果：%d/%d 色块颜色保真 —— 编解码链路通道级无缺陷\n", len(swatches), len(swatches))
}
