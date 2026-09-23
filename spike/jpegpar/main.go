// jpegpar — 阶段 0 · 编码器备选方案验证
//
// 已确认：go264 的 H.264（无论硬件 MF 挂起还是 CPU 路径）在真实运动内容下
// 1080p 仅 12.9fps、2K 仅 6.4fps，不足以支撑低延迟屏幕共享。
//
// Motion JPEG 的优势恰好命中内网场景：
//   - 每帧独立 → 无 GOP，新观众天然秒开（这正是原方案要靠"GOP 环形缓存"才勉强解决的）
//   - 无 P/B 帧 → 延迟最低，丢一帧只花一帧，不会累积花屏
//   - 标准库实现 → 零外部依赖、零 cgo、无 bug 风险
// 唯一短板是单线程。本轮验证：按水平条带并行编码后能否压进 33ms 预算。
//
// 注意：每个条带是独立 JPEG，接收端并行解码后按行拼接即可。
package main

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"runtime"
	"sync"
	"time"
)

func makeBGRA(w, h, shift int) []byte {
	pix := make([]byte, w*h*4)
	for j := 0; j < h; j++ {
		textRow := (j % 16) < 10
		for i := 0; i < w; i++ {
			v := 245
			switch {
			case textRow && ((i+shift)%13) < 7:
				v = 30
			case j > h-120 && i < 400:
				v = 200
			case j < 60:
				v = 225
			}
			p := (j*w + i) * 4
			pix[p], pix[p+1], pix[p+2], pix[p+3] = uint8(v), uint8(v), uint8(v), 255
		}
	}
	return pix
}

func bgraToI420(dst, src []byte, w, h, workers int) {
	ySize := w * h
	cw, ch := w/2, h/2
	cSize := cw * ch
	y := dst[:ySize]
	cb := dst[ySize : ySize+cSize]
	cr := dst[ySize+cSize : ySize+2*cSize]

	var wg sync.WaitGroup
	for k := 0; k < workers; k++ {
		y0, y1 := k*h/workers, (k+1)*h/workers
		if y0 >= y1 {
			continue
		}
		wg.Add(1)
		go func(y0, y1 int) {
			defer wg.Done()
			for j := y0; j < y1; j++ {
				row := j * w * 4
				dstRow := j * w
				for i := 0; i < w; i++ {
					p := row + i*4
					b, g, r := int(src[p]), int(src[p+1]), int(src[p+2])
					y[dstRow+i] = uint8((66*r+129*g+25*b+128)>>8 + 16)
				}
			}
		}(y0, y1)
	}
	for k := 0; k < workers; k++ {
		j0, j1 := k*ch/workers, (k+1)*ch/workers
		if j0 >= j1 {
			continue
		}
		wg.Add(1)
		go func(j0, j1 int) {
			defer wg.Done()
			for j := j0; j < j1; j++ {
				for i := 0; i < cw; i++ {
					var r, g, b int
					for dy := 0; dy < 2; dy++ {
						for dx := 0; dx < 2; dx++ {
							p := ((j*2+dy)*w + (i*2+dx)) * 4
							b += int(src[p])
							g += int(src[p+1])
							r += int(src[p+2])
						}
					}
					r, g, b = r>>2, g>>2, b>>2
					ci := j*cw + i
					cb[ci] = uint8((-38*r-74*g+112*b+128)>>8 + 128)
					cr[ci] = uint8((112*r-94*g-18*b+128)>>8 + 128)
				}
			}
		}(j0, j1)
	}
	wg.Wait()
}

func toYCbCr(i420 []byte, w, h int) *image.YCbCr {
	ySize := w * h
	cSize := ySize / 4
	return &image.YCbCr{
		Y:              i420[:ySize:ySize],
		Cb:             i420[ySize : ySize+cSize : ySize+cSize],
		Cr:             i420[ySize+cSize : ySize+2*cSize : ySize+2*cSize],
		YStride:        w,
		CStride:        w / 2,
		SubsampleRatio: image.YCbCrSubsampleRatio420,
		Rect:           image.Rect(0, 0, w, h),
	}
}

// encodeTiles 按水平条带并行 JPEG 编码，返回每个条带的字节与总字节数
func encodeTiles(img *image.YCbCr, w, h, tiles, q int) ([][]byte, int) {
	if tiles <= 1 {
		var b bytes.Buffer
		_ = jpeg.Encode(&b, img, &jpeg.Options{Quality: q})
		return [][]byte{b.Bytes()}, b.Len()
	}
	const mcu = 16 // 4:2:0 下 MCU 高 16
	mcuRows := (h + mcu - 1) / mcu
	rowsPer := ((mcuRows + tiles - 1) / tiles) * mcu

	outs := make([][]byte, tiles)
	var wg sync.WaitGroup
	for t := 0; t < tiles; t++ {
		y0 := t * rowsPer
		y1 := y0 + rowsPer
		if y0 >= h {
			continue
		}
		if y1 > h {
			y1 = h
		}
		wg.Add(1)
		go func(t, y0, y1 int) {
			defer wg.Done()
			sub := img.SubImage(image.Rect(0, y0, w, y1))
			var b bytes.Buffer
			if err := jpeg.Encode(&b, sub, &jpeg.Options{Quality: q}); err == nil {
				outs[t] = b.Bytes()
			}
		}(t, y0, y1)
	}
	wg.Wait()
	total := 0
	for _, o := range outs {
		total += len(o)
	}
	return outs, total
}

// decodeTiles 并行解码条带（模拟观看端）
func decodeTiles(outs [][]byte) time.Duration {
	var wg sync.WaitGroup
	t0 := time.Now()
	for i := range outs {
		if len(outs[i]) == 0 {
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = jpeg.Decode(bytes.NewReader(outs[i]))
		}(i)
	}
	wg.Wait()
	return time.Since(t0)
}

func main() {
	ncpu := runtime.NumCPU()
	fmt.Printf("\n=== Motion JPEG 条带并行验证 · 阶段 0 ===\n")
	fmt.Printf("CPU 逻辑核心 %d · 画面为高频文本条纹（最难编码的一类）\n", ncpu)
	fmt.Printf("预算：30fps=33.3ms/帧   60fps=16.6ms/帧\n\n")

	type spec struct {
		w, h int
		name string
	}
	specs := []spec{{2880, 1800, "2K"}, {1920, 1080, "1080p"}}

	fmt.Printf("%-10s %-8s %6s %10s %8s %12s %10s  %s\n",
		"分辨率", "质量", "条带", "ms/帧", "fps", "帧字节", "Mbps@30", "评价")
	fmt.Printf("%s\n", "----------------------------------------------------------------------------------")

	for _, sp := range specs {
		w, h := sp.w, sp.h
		i420 := make([]byte, w*h*3/2)
		srcs := make([][]byte, 8)
		for k := range srcs {
			srcs[k] = makeBGRA(w, h, k*3)
		}

		for _, q := range []int{75, 85} {
			for _, tiles := range []int{1, 4, 8, 16, ncpu} {
				if tiles > 1 && tiles != ncpu && tiles != 4 && tiles != 8 && tiles != 16 {
					continue
				}
				// 去重（ncpu 可能与 16 相同）
				label := fmt.Sprintf("%d", tiles)

				// 预热
				bgraToI420(i420, srcs[0], w, h, ncpu)
				encodeTiles(toYCbCr(i420, w, h), w, h, tiles, q)

				t0 := time.Now()
				total := 0
				for k := 1; k < len(srcs); k++ {
					bgraToI420(i420, srcs[k], w, h, ncpu)
					_, n := encodeTiles(toYCbCr(i420, w, h), w, h, tiles, q)
					total += n
				}
				ms := float64(time.Since(t0).Milliseconds()) / float64(len(srcs)-1)
				fps := 1000.0 / ms
				avg := total / (len(srcs) - 1)
				mbps := float64(avg) * 8 * 30 / 1e6

				v := "✅ 30fps+"
				if ms > 33.3 {
					v = "⚠️ <30fps"
				}
				if ms > 66.7 {
					v = "❌ <15fps"
				}
				fmt.Printf("%-10s %-8s %6s %10.1f %8.1f %12d %10.1f  %s\n",
					fmt.Sprintf("%dx%d", w, h), fmt.Sprintf("q=%d", q), label,
					ms, fps, avg, mbps, v)
			}
		}
		// --- 观看端解码耗时（S0-7 对应项） ---
		for _, tiles := range []int{1, 8, ncpu} {
			bgraToI420(i420, srcs[1], w, h, ncpu)
			outs, _ := encodeTiles(toYCbCr(i420, w, h), w, h, tiles, 75)
			_ = decodeTiles(outs) // 预热
			var sum time.Duration
			for k := 2; k < 6; k++ {
				bgraToI420(i420, srcs[k], w, h, ncpu)
				o, _ := encodeTiles(toYCbCr(i420, w, h), w, h, tiles, 75)
				sum += decodeTiles(o)
			}
			ms := float64(sum.Milliseconds()) / 4.0
			fps := 1000.0 / ms
			v := "✅ 轻松"
			if ms > 33.3 {
				v = "⚠️ 偏慢"
			}
			fmt.Printf("%-10s %-8s %6s %10.1f %8.1f %12s %10s  %s\n",
				fmt.Sprintf("%dx%d", w, h), "解码", fmt.Sprintf("%d", tiles),
				ms, fps, "-", "-", v)
		}

		srcs = nil
		runtime.GC()
	}

	fmt.Printf("%s\n", "----------------------------------------------------------------------------------")
	fmt.Printf("注：耗时含 BGRA→I420 转换 + JPEG 编码。Mbps@30 为 30fps 满载估算，千兆内网上限约 940Mbps。\n")
	fmt.Printf("    真实桌面（大面积静态背景）帧大小会显著低于本测试的高频文本画面。\n\n")
	os.Exit(0)
}
