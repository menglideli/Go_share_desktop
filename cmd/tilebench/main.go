// tilebench — 条带粒度基准。
//
// 回答一个问题：同一段桌面操作，条带高度从"1080p/2K 共用 ~20 条"改成
// "按 40px 高度切"之后，每帧字节数与编码耗时各变多少？
//
// 为什么需要它：增量帧的代价 = 变化条带的高度 × 屏宽，与"实际变了多少像素"
// 无关。光标闪一下只有 ~1000 像素，20 条时却要重编一整条 96px 高的横带
// （2K 下 27 万像素），差三个数量级。这个工具把差距量出来，避免凭感觉调参。
package main

import (
	"flag"
	"fmt"
	"os"

	"goshare/internal/codec"
)

var (
	benchW = flag.Int("w", 2880, "画面宽")
	benchH = flag.Int("h", 1800, "画面高")
	frames = flag.Int("frames", 60, "每个场景的帧数")
	tiles  = flag.Int("tiles", 20, "条带数（-tile-h 为 0 时生效）")
	tileH  = flag.Int("tile-h", 0, "目标条带高度（像素），>0 时优先于 -tiles")
	qual   = flag.Int("quality", 75, "JPEG 质量")
)

// makeDesktop 生成一张"像桌面"的图：标题栏、文本行、色块。
// 不要用随机噪声 —— JPEG 对噪声的码率完全不代表真实桌面（见 netcheck 的注释）。
func makeDesktop(w, h int) []byte {
	pix := make([]byte, w*h*4)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var r, g, b uint8 = 245, 245, 245
			switch {
			case y < 64:
				r, g, b = 32, 34, 40 // 标题栏
			case y%26 < 18 && (x/9)%23 != 0 && x > 120:
				r, g, b = 40, 42, 48 // 文本行
			case y > h-360 && x < w/2:
				r, g, b = 176, 120, 96 // 一块暖色区域
			}
			p := (y*w + x) * 4
			pix[p], pix[p+1], pix[p+2], pix[p+3] = b, g, r, 255
		}
	}
	return pix
}

func fill(pix []byte, w, x0, y0, x1, y1 int, r, g, b uint8) {
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			p := (y*w + x) * 4
			pix[p], pix[p+1], pix[p+2], pix[p+3] = b, g, r, 255
		}
	}
}

func main() {
	flag.Parse()
	w, h := *benchW, *benchH
	base := makeDesktop(w, h)
	cur := make([]byte, len(base))

	// 每个场景返回 (改动函数, 每帧真实变化像素数)。
	scenes := []struct {
		name   string
		change func(k int)
		pixels int
	}{
		{"光标闪动", func(k int) {
			// 擦掉上一帧的光标位置，再画到新位置：真实变化 ≈ 2 个光标。
			if k > 0 {
				px := 200 + ((k-1)*37)%(w-300)
				py := 200 + ((k-1)*53)%(h-300)
				for yy := py; yy < py+24; yy++ {
					copy(cur[(yy*w+px)*4:(yy*w+px+24)*4], base[(yy*w+px)*4:(yy*w+px+24)*4])
				}
			}
			x := 200 + (k*37)%(w-300)
			y := 200 + (k*53)%(h-300)
			fill(cur, w, x, y, x+24, y+24, 20, 20, 20)
		}, 24 * 24 * 2},
		{"打字", func(k int) {
			// 一个字符格（14×20）在两种灰度间闪。
			x := 320 + (k%40)*14
			y := 130
			v := uint8(40)
			if k%2 == 0 {
				v = 90
			}
			fill(cur, w, x, y, x+14, y+20, v, v, v)
		}, 14 * 20},
		{"滚动", func(k int) {
			// 整屏上移 8 行，底部补新内容：所有像素都变了。
			copy(cur, cur[w*4*8:])
			for y := h - 8; y < h; y++ {
				for x := 0; x < w; x++ {
					v := uint8(30 + (x+y+k)%200)
					p := (y*w + x) * 4
					cur[p], cur[p+1], cur[p+2] = v, v, v
				}
			}
		}, w * 8},
	}

	fmt.Printf("条带基准：%dx%d · 质量 %d · 每场景 %d 帧\n", w, h, *qual, *frames)
	if *tileH > 0 {
		fmt.Printf("配置：目标条带高度 %dpx\n\n", *tileH)
	} else {
		fmt.Printf("配置：条带数 %d\n\n", *tiles)
	}

	for _, sc := range scenes {
		copy(cur, base)
		enc := codec.NewEncoder(codec.Config{
			Quality: *qual, Tiles: *tiles, TileHeight: *tileH, Dirty: true,
		})
		if _, err := enc.Encode(cur, w, h); err != nil { // 首帧全量，建立 prev
			fmt.Printf("编码失败: %v\n", err)
			os.Exit(1)
		}
		var totalBytes int
		var totalMs float64
		var dirtyTiles int
		var tilePx int
		for k := 0; k < *frames; k++ {
			sc.change(k)
			f, err := enc.Encode(cur, w, h)
			if err != nil {
				fmt.Printf("编码失败: %v\n", err)
				os.Exit(1)
			}
			totalBytes += f.Bytes
			totalMs += f.EncodeMs
			dirtyTiles += len(f.Tiles)
			tilePx = f.TileH * w
		}
		avgB := float64(totalBytes) / float64(*frames)
		avgMs := totalMs / float64(*frames)
		dirty := float64(dirtyTiles) / float64(*frames)
		expand := dirty * float64(tilePx) / float64(sc.pixels)
		fmt.Printf("%-6s 条带 %2d×%dpx · 每帧 %7.1f KB · 编码 %5.2f ms · 脏条带 %4.1f 个（重编像素是实际变化的 %.0fx）\n",
			sc.name, enc.TileCount(), tilePx/w, avgB/1024, avgMs, dirty, expand)
	}
}
