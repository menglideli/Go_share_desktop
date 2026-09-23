// decprobe — 阶段 0 · S0-7 + S0-8
//
// 背景：已确认 go264 的 Media Foundation **编码**路径在本机永久挂起，只能走 CPU。
// 本轮回答：
//   S0-7 解码端 Backend() 是什么（D3D 硬解可用？），解码耗时多少，是否也挂起
//   S0-8 流中途分辨率变化时，同一个 Decoder 能否自动重配置；不行则走 Close+New 重建
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
	for j := 0; j < h; j++ {
		for i := 0; i < w; i++ {
			v := 245
			if (j%16) < 10 && ((i+shift)%13) < 7 {
				v = 30
			}
			y[j*w+i] = uint8(v)
		}
	}
	for i := w * h; i < len(buf); i++ {
		buf[i] = 128
	}
	return buf
}

type res struct {
	backend string
	ms      int64
	frames  int
	w, h    int
	err     error
}

func runSoft(label string, timeout time.Duration, fn func() res) (res, bool) {
	ch := make(chan res, 1)
	go func() { ch <- fn() }()
	select {
	case r := <-ch:
		return r, false
	case <-time.After(timeout):
		fmt.Printf("  ⛔ %s：超过 %v 未返回 → 判定为挂起(HANG)\n", label, timeout)
		return res{}, true
	}
}

// encodePackets 用 CPU 路径（slices 并行）编 n 帧，返回逐帧的 Annex-B 包
func encodePackets(w, h, n int) ([][]byte, string, error) {
	cfg := go264.EncoderConfig{
		Width: w, Height: h,
		FPSNum: 30, FPSDen: 1,
		GOPSize: 30, QP: 24, CABAC: true,
		Slices:        runtime.NumCPU(),
		ForceSoftware: true,
	}
	enc, err := go264.NewEncoder(cfg)
	if err != nil {
		return nil, "", err
	}
	defer enc.Close()
	out := make([][]byte, 0, n)
	for i := 0; i < n; i++ {
		pkt, err := enc.Encode(makeI420(w, h, i*3))
		if err != nil {
			return nil, "", err
		}
		out = append(out, pkt)
	}
	return out, enc.Backend(), nil
}

func totalBytes(pkts [][]byte) int {
	n := 0
	for _, p := range pkts {
		n += len(p)
	}
	return n
}

func main() {
	fmt.Printf("\n=== go264 解码 / 分辨率切换探测 · S0-7 + S0-8 ===\n")
	fmt.Printf("CPU 逻辑核心 %d\n\n", runtime.NumCPU())

	// ---------- 准备码流 ----------
	fmt.Printf("[准备] 用 CPU 编码器生成码流\n")
	p1080, encBE, err := encodePackets(1920, 1080, 8)
	if err != nil {
		fmt.Printf("  1080p 编码失败: %v\n", err)
		return
	}
	fmt.Printf("  1080p 8 帧 → %d 个包，共 %d 字节（编码后端 %s）\n", len(p1080), totalBytes(p1080), encBE)
	p720, _, err := encodePackets(1280, 720, 8)
	if err != nil {
		fmt.Printf("  720p 编码失败: %v\n", err)
		return
	}
	fmt.Printf("  720p  8 帧 → %d 个包，共 %d 字节\n", len(p720), totalBytes(p720))

	// ---------- S0-7 解码（逐包喂 + Flush） ----------
	fmt.Printf("\n[S0-7] 解码（D3D11 硬解，逐包喂 + Flush 收尾）\n")
	r, hung := runSoft("Decode(1080p) 逐包", 20*time.Second, func() res {
		dec := go264.NewDecoder()
		defer dec.Close()
		be := dec.Backend()
		total, firstW, firstH := 0, 0, 0
		t0 := time.Now()
		for _, p := range p1080 {
			fs, err := dec.Decode(p)
			if err != nil {
				return res{backend: be, frames: total, err: err}
			}
			for _, f := range fs {
				if firstW == 0 {
					firstW, firstH = f.Width, f.Height
				}
				total++
			}
		}
		fs, err := dec.Flush()
		if err == nil {
			for _, f := range fs {
				if firstW == 0 {
					firstW, firstH = f.Width, f.Height
				}
				total++
			}
		}
		return res{backend: be, ms: time.Since(t0).Milliseconds(), frames: total, w: firstW, h: firstH}
	})
	if !hung {
		if r.err != nil {
			fmt.Printf("  解码失败: %v\n", r.err)
		} else {
			per := float64(r.ms)
			if r.frames > 0 {
				per = float64(r.ms) / float64(r.frames)
			}
			fmt.Printf("  Backend=%s  解出 %d/8 帧  总耗时 %dms  → %.1fms/帧  尺寸 %dx%d\n",
				r.backend, r.frames, r.ms, per, r.w, r.h)
			if r.frames == 0 {
				fmt.Printf("  ⚠️  一帧都没解出 —— 硬解路径有问题，需回退 CPU 解码\n")
			}
		}
	}

	// ---------- S0-8 分辨率切换 ----------
	fmt.Printf("\n[S0-8] 流中途分辨率变化（1080p → 720p）\n")

	// 方案 A：同一个 Decoder 继续喂新分辨率
	rA, hung := runSoft("同一 Decoder 切换分辨率", 20*time.Second, func() res {
		dec := go264.NewDecoder()
		defer dec.Close()
		be := dec.Backend()
		sizes := []string{}
		count := func(fs []*go264.Frame) {
			for _, f := range fs {
				sizes = append(sizes, fmt.Sprintf("%dx%d", f.Width, f.Height))
			}
		}
		for _, p := range p1080 {
			fs, err := dec.Decode(p)
			if err != nil {
				return res{backend: be, err: fmt.Errorf("1080p 段失败: %w", err)}
			}
			count(fs)
		}
		for _, p := range p720 {
			fs, err := dec.Decode(p)
			if err != nil {
				return res{backend: be, err: fmt.Errorf("720p 段失败: %w", err)}
			}
			count(fs)
		}
		fs, _ := dec.Flush()
		count(fs)
		return res{backend: be, frames: len(sizes), w: 0, h: 0,
			err: fmt.Errorf("尺寸序列=%v", sizes)}
	})
	if !hung {
		// 用 err 字段承载尺寸序列（仅用于展示）
		fmt.Printf("  同一解码器：%v\n", rA.err)
		if rA.err != nil {
			// 区分真失败与展示
		}
	}

	// 方案 B：Close + NewDecoder 重建
	rB, hung := runSoft("Close + NewDecoder 重建", 20*time.Second, func() res {
		dec := go264.NewDecoder()
		n1 := 0
		for _, p := range p1080 {
			fs, err := dec.Decode(p)
			if err != nil {
				return res{err: err}
			}
			n1 += len(fs)
		}
		fs, _ := dec.Flush()
		n1 += len(fs)
		dec.Close()

		dec2 := go264.NewDecoder()
		defer dec2.Close()
		n2, w2, h2 := 0, 0, 0
		for _, p := range p720 {
			fs, err := dec2.Decode(p)
			if err != nil {
				return res{err: err}
			}
			for _, f := range fs {
				if w2 == 0 {
					w2, h2 = f.Width, f.Height
				}
				n2++
			}
		}
		fs2, _ := dec2.Flush()
		for _, f := range fs2 {
			if w2 == 0 {
				w2, h2 = f.Width, f.Height
			}
			n2++
		}
		return res{backend: dec2.Backend(), frames: n1 + n2, w: w2, h: h2}
	})
	if !hung {
		if rB.err != nil {
			fmt.Printf("  ⚠️  重建路径失败: %v\n", rB.err)
		} else {
			fmt.Printf("  重建路径：共解出 %d 帧（1080p段+%d），720p 段输出 %dx%d（backend=%s）\n",
				rB.frames, rB.frames-8, rB.w, rB.h, rB.backend)
		}
	}

	fmt.Printf("\n=== 探测结束 ===\n\n")
	os.Exit(0)
}
