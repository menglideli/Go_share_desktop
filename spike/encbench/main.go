// encbench — 阶段 0 · S0-2 + S0-6
// 目的：实测 go264 在本机（i7-13700H + Iris Xe, Win10 LTSC 17763）上
// ① 能否拿到 Media Foundation 硬件编码后端
// ② 1080p 与 2K 编码的单帧耗时 / 吞吐 / 码率 / CPU 核当量
// ③ 1800 这种非 16 倍数高度是否会导致分辨率被 pad（解码侧验证）
package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/oops1/go.264"
)

// ---------- CPU 时间（Windows, 100ns 单位） ----------

func cpuNow() int64 {
	var creation, exit, kernel, user syscall.Filetime
	h, err := syscall.GetCurrentProcess()
	if err != nil {
		return 0
	}
	if err := syscall.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return 0
	}
	return ftToNS(user) + ftToNS(kernel)
}

func ftToNS(ft syscall.Filetime) int64 {
	return (int64(ft.HighDateTime)<<32 | int64(ft.LowDateTime)) * 100
}

// ---------- 测试画面：模拟屏幕（白底 + 文本行 + UI 灰块），逐帧水平滚动 ----------

func makeI420(w, h, shift int) []byte {
	buf := make([]byte, w*h*3/2)
	y := buf[:w*h]
	cb := buf[w*h : w*h+w*h/4]
	cr := buf[w*h+w*h/4 : w*h*3/2]
	for j := 0; j < h; j++ {
		row := j * w
		// 每 16 行一个文本行，其中前 10 行有"字"
		textRow := (j % 16) < 10
		for i := 0; i < w; i++ {
			v := 245
			switch {
			case textRow && ((i+shift)%13) < 7:
				v = 30 // 笔画
			case j > h-120 && i < 400:
				v = 200 // 底部状态栏
			case j < 60:
				v = 225 // 顶栏
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
	// 一小块彩色区域，让色度平面有非零内容
	for j := 100; j < 300; j++ {
		for i := 500; i < 700; i++ {
			ci := (j/2)*(w/2) + (i / 2)
			cb[ci] = 90
			cr[ci] = 160
		}
	}
	return buf
}

func genFrames(w, h, n int) [][]byte {
	frames := make([][]byte, n)
	for k := 0; k < n; k++ {
		frames[k] = makeI420(w, h, k*3) // 每帧滚动 3px
	}
	return frames
}

// ---------- 单用例 ----------

type result struct {
	name     string
	backend  string
	msPerFrm float64
	fps      float64
	kbps     float64
	cpuCores float64
	nonEmpty int
	firstPkt int
	err      error
	decW     int
	decH     int
}

func runCase(name string, cfg go264.EncoderConfig, frames [][]byte, encodeN int) result {
	r := result{name: name}
	w, h := cfg.Width, cfg.Height

	enc, err := go264.NewEncoder(cfg)
	if err != nil {
		r.err = err
		return r
	}
	defer enc.Close()
	r.backend = enc.Backend()

	n := len(frames)
	// 预热
	for i := 0; i < 5 && i < n; i++ {
		_, _ = enc.Encode(frames[i])
	}

	var packets [][]byte
	wall0 := time.Now()
	cpu0 := cpuNow()
	total := 0
	for i := 0; i < encodeN; i++ {
		pkt, err := enc.Encode(frames[i%n])
		if err != nil {
			r.err = err
			break
		}
		if len(pkt) > 0 {
			r.nonEmpty++
			total += len(pkt)
			if r.firstPkt == 0 {
				r.firstPkt = len(pkt)
			}
			if len(packets) < 24 {
				packets = append(packets, append([]byte(nil), pkt...))
			}
		}
	}
	wall := time.Since(wall0)
	cpu := cpuNow() - cpu0

	cnt := r.nonEmpty
	if cnt == 0 {
		cnt = 1
	}
	r.msPerFrm = float64(wall.Microseconds()) / 1000.0 / float64(encodeN)
	r.fps = 1000.0 / r.msPerFrm
	r.cpuCores = float64(cpu) / float64(wall.Nanoseconds())
	secs := float64(encodeN) / 30.0
	r.kbps = float64(total) * 8 / 1000.0 / secs

	// 解码验证：确认分辨率没被 pad 改变
	if len(packets) > 0 {
		dec := go264.NewDecoder()
		for _, p := range packets {
			fs, err := dec.Decode(p)
			if err != nil {
				break
			}
			for _, f := range fs {
				if r.decW == 0 {
					r.decW, r.decH = f.Width, f.Height
				}
			}
		}
		dec.Close()
	}
	_ = w
	_ = h
	return r
}

func main() {
	ncpu := runtime.NumCPU()
	fmt.Printf("\n=== go264 编码实测 · S0-2 / S0-6 ===\n")
	fmt.Printf("CPU 逻辑核心 %d · GOOS=windows · CGO_ENABLED=0\n", ncpu)
	fmt.Printf("预算：30fps=33.3ms/帧，60fps=16.6ms/帧\n")

	type spec struct {
		label string
		w, h  int
	}
	specs := []spec{
		{"1080p", 1920, 1080},
		{"2K(2880x1800)", 2880, 1800},
	}

	sep := strings.Repeat("-", 118)
	fmt.Printf("\n%-42s %-17s %9s %8s %10s %8s %6s  %s\n",
		"用例", "backend", "ms/帧", "fps", "kbps", "CPU核当量", "出帧", "解码尺寸")
	fmt.Printf("%s\n", sep)

	var all []result
	for _, sp := range specs {
		frames := genFrames(sp.w, sp.h, 24)
		base := go264.EncoderConfig{
			Width:   sp.w,
			Height:  sp.h,
			FPSNum:  30,
			FPSDen:  1,
			GOPSize: 30,
			QP:      24,
			BFrames: 0,
			CABAC:   true,
			Slices:  1,
		}
		fmt.Fprintf(os.Stdout, "\n--- %s ---\n", sp.label)

		// 2K 纯 CPU 路径极慢，用更少的帧即可得出均值
		nFast, nSlow := 20, 20
		if sp.w > 2000 {
			nFast, nSlow = 20, 4
		}

		cases := []struct {
			name string
			cfg  go264.EncoderConfig
			n    int
		}{
			{sp.label + " 硬件优先 QP24", base, nFast},
			{sp.label + " 强制CPU QP24 slices=1", func() go264.EncoderConfig {
				c := base
				c.ForceSoftware = true
				return c
			}(), nSlow},
			{sp.label + fmt.Sprintf(" 强制CPU QP24 slices=%d", ncpu), func() go264.EncoderConfig {
				c := base
				c.ForceSoftware = true
				c.Slices = ncpu
				return c
			}(), nSlow},
			{sp.label + " 硬件优先 码率8000k", func() go264.EncoderConfig {
				c := base
				c.QP = 0
				c.BitrateKbps = 8000
				return c
			}(), nFast},
			{sp.label + " RateFactor23(应强制CPU)", func() go264.EncoderConfig {
				c := base
				c.QP = 0
				c.RateFactor = 23
				return c
			}(), nSlow},
		}

		for _, c := range cases {
			fmt.Fprintf(os.Stdout, "  ... %s\n", c.name)
			r := runCase(c.name, c.cfg, frames, c.n)
			all = append(all, r)
			printRow(r)
		}
		frames = nil
		runtime.GC()
	}

	fmt.Printf("%s\n", sep)
	fmt.Printf("说明：CPU核当量 = 进程CPU时间/墙钟时间，1.0 表示吃满一个核；!! 超 30fps 预算(33.3ms)，! 偏紧(16.6ms)\n\n")
}

func printRow(r result) {
	if r.err != nil {
		fmt.Printf("!!%-40s  ERROR: %v\n", r.name, r.err)
		return
	}
	dec := fmt.Sprintf("%dx%d", r.decW, r.decH)
	mark := "  "
	if r.msPerFrm > 33.3 {
		mark = "!!"
	} else if r.msPerFrm > 16.6 {
		mark = " !"
	}
	fmt.Printf("%s%-40s %-17s %9.2f %8.1f %10.0f %8.2f %6d  %s\n",
		mark, r.name, r.backend, r.msPerFrm, r.fps, r.kbps, r.cpuCores, r.nonEmpty, dec)
}
