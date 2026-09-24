// pixprobe — 像素探针：定位"画面全黑"到底发生在哪一环。
//
// 它把链路拆成三个观测点分别统计非黑像素占比：
//   1. 采集源原始 BGRA
//   2. MJPEG 编码后的条带（字节数、条带数）
//   3. 解码后的 NRGBA 画布
// 谁先变黑，问题就在谁那里。同时把关键点存成 PNG 供肉眼核对。
package main

import (
	"context"
	"flag"
	"fmt"
	"image"
	"image/png"
	"log"
	"os"
	"path/filepath"
	"time"

	"goshare/internal/capture"
	"goshare/internal/codec"
)

// pixStats 统计一帧的"内容度"。全黑画面的 nonBlack≈0、uniq≈1。
type pixStats struct {
	NonBlack float64 // 亮度 > 8 的像素占比（0~1）
	MeanLum  float64 // 平均亮度 0~255
	Uniq     int     // 采样得到的不同颜色数（量化到 5 bit）
}

func statBGRA(pix []byte, w, h int) pixStats {
	var lumSum float64
	nonBlack := 0
	seen := map[uint32]struct{}{}
	total := w * h
	step := 1
	if total > 400000 {
		step = 4 // 大画面抽样，够用且快
	}
	n := 0
	for j := 0; j < h; j += step {
		row := j * w * 4
		for i := 0; i < w; i += step {
			p := row + i*4
			b, g, r := int(pix[p]), int(pix[p+1]), int(pix[p+2])
			lum := (77*r + 150*g + 29*b) >> 8
			lumSum += float64(lum)
			if lum > 8 {
				nonBlack++
			}
			key := uint32(r>>3)<<10 | uint32(g>>3)<<5 | uint32(b>>3)
			seen[key] = struct{}{}
			n++
		}
	}
	if n == 0 {
		return pixStats{}
	}
	return pixStats{
		NonBlack: float64(nonBlack) / float64(n),
		MeanLum:  lumSum / float64(n),
		Uniq:     len(seen),
	}
}

func statNRGBA(pix []byte, w, h int) pixStats {
	return statBGRA(pix, w, h) // NRGBA 与 BGRA 只是通道顺序不同，亮度统计等价
}

func toNRGBA(pix []byte, w, h int) *image.NRGBA {
	out := image.NewNRGBA(image.Rect(0, 0, w, h))
	d := out.Pix
	for i := 0; i < w*h; i++ {
		s := i * 4
		d[s], d[s+1], d[s+2], d[s+3] = pix[s+2], pix[s+1], pix[s], 255
	}
	return out
}

func savePNG(path string, img image.Image) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}

func main() {
	all := flag.Bool("all", true, "遍历所有显示器")
	idx := flag.Int("display", -1, "只测指定显示器（-1 = 全部）")
	outDir := flag.String("out", "out", "PNG 输出目录")
	nFrames := flag.Int("frames", 3, "每个显示器取几帧")
	flag.Parse()
	_ = all

	capture.EnsureDPIAware()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ds, err := capture.Displays(ctx)
	if err != nil || len(ds) == 0 {
		fmt.Fprintf(os.Stderr, "枚举显示器失败: %v\n", err)
		os.Exit(1)
	}
	for _, d := range ds {
		fmt.Printf("显示器 %s 原点(%d,%d)\n", d.String(), d.X, d.Y)
	}

	list := ds
	if *idx >= 0 && *idx < len(ds) {
		list = ds[*idx : *idx+1]
	}

	fail := 0
	for _, d := range list {
		fmt.Printf("\n===== 采集 %s =====\n", d.String())
		src, err := capture.NewSource(ctx, capture.Options{Display: d, FPS: 30, Cursor: true})
		if err != nil {
			fmt.Printf("  ✗ 启动采集失败: %v\n", err)
			fail++
			continue
		}
		fmt.Printf("  后端=%s note=%q\n", src.Backend(), src.Note())

		// 取 3 帧：DXGI 变化驱动，超时是预期行为，多试几次
		var frames []capture.Frame
		for k := 0; k < *nFrames; k++ {
			c, cc := context.WithTimeout(ctx, 2*time.Second)
			f, err := src.WaitFrame(c)
			cc()
			if err != nil {
				fmt.Printf("  第 %d 次取帧: %v\n", k+1, err)
				continue
			}
			frames = append(frames, f.Clone())
			time.Sleep(120 * time.Millisecond)
		}
		src.Close()

		if len(frames) == 0 {
			fmt.Printf("  ✗ 一帧都没拿到 —— 采集层就没内容\n")
			fail++
			continue
		}
		fmt.Printf("  取到 %d 帧\n", len(frames))

		for i, f := range frames {
			st := statBGRA(f.Pix, f.W, f.H)
			fmt.Printf("  [帧%d] 采集源 %dx%d  非黑=%.1f%%  平均亮度=%.1f  颜色数=%d\n",
				i+1, f.W, f.H, st.NonBlack*100, st.MeanLum, st.Uniq)
			if i < 2 || i == len(frames)-1 {
				p := filepath.Join(*outDir, fmt.Sprintf("probe-d%d-src.png", d.ID))
				if err := savePNG(p, toNRGBA(f.Pix, f.W, f.H)); err == nil {
					fmt.Printf("         已保存 %s\n", p)
				}
			}

			// 走一遍真实编解码
			enc := codec.NewEncoder(codec.Config{Quality: 75, Dirty: true})
			out, err := enc.Encode(f.Pix, f.W, f.H)
			if err != nil {
				fmt.Printf("          ✗ 编码失败: %v\n", err)
				fail++
				continue
			}
			fmt.Printf("         编码: 条带 %d/%d  全量=%v  %d 字节  %.1f ms\n",
				len(out.Tiles), out.TotalTiles, out.Full, out.Bytes, out.EncodeMs)

			dec := codec.NewDecoder(0)
			img, err := dec.Decode(out)
			if err != nil {
				fmt.Printf("          ✗ 解码失败: %v\n", err)
				fail++
				continue
			}
			ds2 := statNRGBA(img.Pix, img.Bounds().Dx(), img.Bounds().Dy())
			fmt.Printf("         解码: 非黑=%.1f%%  平均亮度=%.1f  颜色数=%d  %.1f ms\n",
				ds2.NonBlack*100, ds2.MeanLum, ds2.Uniq, dec.DecodeMs)
			if i < 2 || i == len(frames)-1 {
				p := filepath.Join(*outDir, fmt.Sprintf("probe-d%d-dec.png", d.ID))
				if err := savePNG(p, img); err == nil {
					fmt.Printf("         已保存 %s\n", p)
				}
			}
		}
	}
	log.Printf("pixprobe 完成，失败项 %d", fail)
	if fail > 0 {
		os.Exit(1)
	}
}
