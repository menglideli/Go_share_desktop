// encscan — 阶段 0 · S0-6 决定性测试
//
// 前提（已实测确认）：go264 的 Media Foundation **硬件编码在本机永久挂起**，
// 只能使用 CPU 路径 + slices 并行。因此"默认发原生 2K"不再成立，
// 必须实测各分辨率下的编码耗时，才能定 v1 的分辨率与帧率策略。
//
// 全部使用 ForceSoftware=true + Slices=NumCPU（唯一可用且最快的组合）。
package main

import (
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/oops1/go.264"
)

func makeI420(w, h, shift int) []byte {
	buf := make([]byte, w*h*3/2)
	y := buf[:w*h]
	cb := buf[w*h : w*h+w*h/4]
	cr := buf[w*h+w*h/4 : w*h*3/2]
	for j := 0; j < h; j++ {
		row := j * w
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
			y[row+i] = uint8(v)
		}
	}
	for i := range cb {
		cb[i] = 128
	}
	for i := range cr {
		cr[i] = 128
	}
	return buf
}

func genFrames(w, h, n int) [][]byte {
	fs := make([][]byte, n)
	for k := 0; k < n; k++ {
		fs[k] = makeI420(w, h, k*3)
	}
	return fs
}

func main() {
	ncpu := runtime.NumCPU()
	fmt.Printf("\n=== go264 CPU 编码分辨率扫描 · S0-6 ===\n")
	fmt.Printf("CPU 逻辑核心 %d · 全部 ForceSoftware + Slices=%d（硬件 MF 已确认挂起，不可用）\n\n", ncpu, ncpu)
	fmt.Printf("预算：30fps=33.3ms/帧   20fps=50ms/帧   15fps=66.7ms/帧\n\n")

	type spec struct {
		w, h int
		note string
	}
	specs := []spec{
		{2880, 1800, "原生 2K（本机屏）"},
		{1920, 1080, "1080p"},
		{1600, 1000, "1600x1000"},
		{1440, 900, "2K 半分辨率（16:10）"},
		{1280, 720, "720p"},
	}

	fmt.Printf("%-16s %-22s %10s %9s %12s %8s  %s\n",
		"分辨率", "说明", "ms/帧", "可达fps", "平均帧大小", "kbps", "评价")
	fmt.Printf("%s\n", "------------------------------------------------------------------------------------------")

	for _, sp := range specs {
		cfg := go264.EncoderConfig{
			Width: sp.w, Height: sp.h,
			FPSNum: 30, FPSDen: 1,
			GOPSize: 30, QP: 24, CABAC: true,
			Slices:        ncpu,
			ForceSoftware: true,
		}
		frames := genFrames(sp.w, sp.h, 6)

		enc, err := go264.NewEncoder(cfg)
		if err != nil {
			fmt.Printf("%-16s  创建失败: %v\n", fmt.Sprintf("%dx%d", sp.w, sp.h), err)
			continue
		}
		// 预热（首帧含 IDR + 初始化，丢弃）
		if _, err := enc.Encode(frames[0]); err != nil {
			fmt.Printf("%-16s  首帧编码失败: %v\n", fmt.Sprintf("%dx%d", sp.w, sp.h), err)
			enc.Close()
			continue
		}
		t0 := time.Now()
		total := 0
		for i := 1; i < len(frames); i++ {
			pkt, err := enc.Encode(frames[i])
			if err != nil {
				break
			}
			total += len(pkt)
		}
		ms := float64(time.Since(t0).Milliseconds()) / float64(len(frames)-1)
		enc.Close()

		avg := total / (len(frames) - 1)
		fps := 1000.0 / ms
		kbps := float64(avg) * 8 / 1000.0 * fps
		verdict := "✅ 30fps 可达"
		if ms > 66.7 {
			verdict = "❌ <15fps，不可用"
		} else if ms > 50 {
			verdict = "⚠️ 仅 ~15fps"
		} else if ms > 33.3 {
			verdict = "⚠️ 仅 ~20fps"
		}
		fmt.Printf("%-16s %-22s %10.1f %9.1f %12d %8.0f  %s\n",
			fmt.Sprintf("%dx%d", sp.w, sp.h), sp.note, ms, fps, avg, kbps, verdict)

		frames = nil
		runtime.GC()
	}

	fmt.Printf("%s\n", "------------------------------------------------------------------------------------------")
	fmt.Printf("注：测试画面为高频文本条纹（最难编码的内容之一）；真实桌面大面积静态时耗时显著更低。\n\n")
	os.Exit(0)
}
