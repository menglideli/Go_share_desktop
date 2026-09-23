// codecbench — 阶段 0 · 编码器选型决策
//
// 已实测确认 go264 CPU 编码器在真实运动内容下：2K 6.4fps / 1080p 13.6fps / 720p 17.9fps，
// 且 Media Foundation 硬件编码永久挂起。H.264 路线已不足以支撑"低延迟屏幕共享"。
//
// 内网场景的关键特征：带宽不是瓶颈（千兆），延迟和 CPU 才是。
// 因此本轮对比两条出路：
//   A/B. go264 H.264 极限参数调优（关 CABAC、最小运动搜索、最少参考帧、快速模式决策）
//   C/D. Motion JPEG（标准库，每帧独立 → 无 GOP 依赖、新观众天然秒开、丢帧不花屏）
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

	"github.com/oops1/go.264"
)

// ---------- 测试画面（高频文本条纹，最难编码的一类） ----------

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
			pix[p] = uint8(v)
			pix[p+1] = uint8(v)
			pix[p+2] = uint8(v)
			pix[p+3] = 255
		}
	}
	return pix
}

// bgraToI420 并行版（沿用 scalebench 已验证实现）
func bgraToI420(dst, src []byte, w, h, workers int) {
	ySize := w * h
	cw, ch := w/2, h/2
	cSize := cw * ch
	y := dst[:ySize]
	cb := dst[ySize : ySize+cSize]
	cr := dst[ySize+cSize : ySize+2*cSize]

	var wg sync.WaitGroup
	for k := 0; k < workers; k++ {
		y0 := k * h / workers
		y1 := (k + 1) * h / workers
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
					b := int(src[p])
					g := int(src[p+1])
					r := int(src[p+2])
					y[dstRow+i] = uint8((66*r+129*g+25*b+128)>>8 + 16)
				}
			}
		}(y0, y1)
	}
	for k := 0; k < workers; k++ {
		j0 := k * ch / workers
		j1 := (k + 1) * ch / workers
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
					r >>= 2
					g >>= 2
					b >>= 2
					ci := j*cw + i
					cb[ci] = uint8((-38*r-74*g+112*b+128)>>8 + 128)
					cr[ci] = uint8((112*r-94*g-18*b+128)>>8 + 128)
				}
			}
		}(j0, j1)
	}
	wg.Wait()
}

func toYCbCR(i420 []byte, w, h int) *image.YCbCr {
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

func main() {
	ncpu := runtime.NumCPU()
	fmt.Printf("\n=== 编码器选型决策 · 阶段 0 ===\n")
	fmt.Printf("CPU 逻辑核心 %d\n", ncpu)
	fmt.Printf("预算：30fps=33.3ms/帧   15fps=66.7ms/帧\n\n")

	type spec struct{ w, h int }
	specs := []spec{{2880, 1800}, {1920, 1080}}

	fmt.Printf("%-12s %-34s %9s %8s %11s %9s\n", "分辨率", "方案", "ms/帧", "fps", "平均帧字节", " Mbps")
	fmt.Printf("%s\n", "---------------------------------------------------------------------------------")

	for _, sp := range specs {
		w, h := sp.w, sp.h
		i420 := make([]byte, w*h*3/2)
		srcs := make([][]byte, 6)
		for k := range srcs {
			srcs[k] = makeBGRA(w, h, k*3)
		}

		// --- A. H.264 默认（slices 并行） ---
		benchH264(fmt.Sprintf("%dx%d", w, h), "H.264 默认(QP24,CABAC,slices=20)", w, h, srcs, i420,
			go264.EncoderConfig{Width: w, Height: h, FPSNum: 30, FPSDen: 1, GOPSize: 30, QP: 24,
				CABAC: true, Slices: ncpu, ForceSoftware: true})

		// --- B. H.264 极限速度配置 ---
		benchH264(fmt.Sprintf("%dx%d", w, h), "H.264 极速(CAVLC,零运动搜索,fast)", w, h, srcs, i420,
			go264.EncoderConfig{Width: w, Height: h, FPSNum: 30, FPSDen: 1, GOPSize: 30, QP: 24,
				CABAC: false, Slices: ncpu, ForceSoftware: true,
				MotionSearch: go264.MotionSearchZero, ModeDecision: go264.ModeDecisionFast,
				RefFrames: 1, Transform8x8: false, ScalingMatrix: go264.ScalingMatrixFlat})

		// --- C/D. Motion JPEG ---
		for _, q := range []int{80, 92} {
			// 预热
			bgraToI420(i420, srcs[0], w, h, ncpu)
			img := toYCbCR(i420, w, h)
			var bb bytes.Buffer
			_ = jpeg.Encode(&bb, img, &jpeg.Options{Quality: q})

			t0 := time.Now()
			total := 0
			for k := 1; k < len(srcs); k++ {
				bgraToI420(i420, srcs[k], w, h, ncpu)
				img := toYCbCR(i420, w, h)
				bb.Reset()
				if err := jpeg.Encode(&bb, img, &jpeg.Options{Quality: q}); err != nil {
					fmt.Printf("  jpeg 失败: %v\n", err)
					break
				}
				total += bb.Len()
			}
			ms := float64(time.Since(t0).Milliseconds()) / float64(len(srcs)-1)
			fps := 1000.0 / ms
			avg := total / (len(srcs) - 1)
			mbps := float64(avg) * 8 * fps / 1e6
			fmt.Printf("%-12s %-34s %9.1f %8.1f %11d %9.1f\n",
				fmt.Sprintf("%dx%d", w, h), fmt.Sprintf("Motion JPEG q=%d（含BGRA→I420）", q),
				ms, fps, avg, mbps)
		}
		srcs = nil
		runtime.GC()
	}

	fmt.Printf("%s\n", "---------------------------------------------------------------------------------")
	fmt.Printf("注：画面为高频文本条纹（最难编码的一类）；真实桌面大面积静态时两者都会更快。\n")
	fmt.Printf("    Mbps 为 30fps 满载估算；千兆内网上限约 940 Mbps。\n\n")
	os.Exit(0)
}

func benchH264(tag, name string, w, h int, srcs [][]byte, i420 []byte, cfg go264.EncoderConfig) {
	enc, err := go264.NewEncoder(cfg)
	if err != nil {
		fmt.Printf("%-12s %-34s  创建失败: %v\n", tag, name, err)
		return
	}
	defer enc.Close()

	frames := make([][]byte, len(srcs))
	for k, s := range srcs {
		buf := make([]byte, w*h*3/2)
		bgraToI420(buf, s, w, h, runtime.NumCPU())
		frames[k] = buf
	}
	if _, err := enc.Encode(frames[0]); err != nil {
		fmt.Printf("%-12s %-34s  首帧失败: %v\n", tag, name, err)
		return
	}
	t0 := time.Now()
	total := 0
	for k := 1; k < len(frames); k++ {
		pkt, err := enc.Encode(frames[k])
		if err != nil {
			break
		}
		total += len(pkt)
	}
	ms := float64(time.Since(t0).Milliseconds()) / float64(len(frames)-1)
	fps := 1000.0 / ms
	avg := total / (len(frames) - 1)
	mbps := float64(avg) * 8 * fps / 1e6
	fmt.Printf("%-12s %-34s %9.1f %8.1f %11d %9.1f\n", tag, name, ms, fps, avg, mbps)
}
