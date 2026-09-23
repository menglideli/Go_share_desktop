// mfprobe — 阶段 0 · S0-2 定性探测
//
// 已确认的事实（本机 Win10 LTSC 17763 + Iris Xe）：
//   - go264.NewEncoder 硬件路径能拿到 backend=mediafoundation（约 392ms）
//   - 但 enc.Encode() 首帧永久挂起（>15s 不返回）
//   - CPU 路径可用，首帧 328ms（含初始化）
//
// 本轮要回答两个问题：
//   A. CPU 路径的「稳态」单帧耗时是多少（首帧不代表稳态）？并行 slices 能加速多少？
//   B. MF 挂起能否绕过？两个嫌疑：COM 线程亲和性（LockOSThread）、未做 COM 初始化（CoInitializeEx）
package main

import (
	"fmt"
	"os"
	"runtime"
	"syscall"
	"time"

	"github.com/oops1/go.264"
)

var (
	ole32    = syscall.NewLazyDLL("ole32.dll")
	procCoIn = ole32.NewProc("CoInitializeEx")
)

const COINIT_MULTITHREADED = 0x0

func makeI420(w, h int) []byte {
	buf := make([]byte, w*h*3/2)
	y := buf[:w*h]
	for j := 0; j < h; j++ {
		for i := 0; i < w; i++ {
			v := 245
			if (j%16) < 10 && ((i+j)%13) < 7 {
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

type stepResult struct {
	backend string
	ms      int64
	size    int
	frames  int
	err     error
}

// runSoft 超时后返回 hung=true，不杀进程（便于继续测下一个假设）
func runSoft(label string, timeout time.Duration, fn func() stepResult) (stepResult, bool) {
	ch := make(chan stepResult, 1)
	go func() { ch <- fn() }()
	select {
	case r := <-ch:
		return r, false
	case <-time.After(timeout):
		fmt.Printf("  ⛔ %s：超过 %v 未返回 → 判定为挂起(HANG)\n", label, timeout)
		return stepResult{}, true
	}
}

// steady 测稳态：跳过首帧，测 n 帧的平均耗时
func steady(cfg go264.EncoderConfig, frame []byte, n int) stepResult {
	enc, err := go264.NewEncoder(cfg)
	if err != nil {
		return stepResult{err: err}
	}
	defer enc.Close()
	be := enc.Backend()
	// 首帧（IDR，含初始化开销）单独丢弃
	if _, err := enc.Encode(frame); err != nil {
		return stepResult{backend: be, err: err}
	}
	t0 := time.Now()
	total := 0
	for i := 0; i < n; i++ {
		pkt, err := enc.Encode(frame)
		if err != nil {
			return stepResult{backend: be, err: err}
		}
		total += len(pkt)
	}
	d := time.Since(t0)
	return stepResult{backend: be, ms: d.Milliseconds() / int64(n), size: total / n, frames: n}
}

func main() {
	const W, H = 1920, 1080
	frame := makeI420(W, H)
	ncpu := runtime.NumCPU()
	fmt.Printf("\n=== go264 定性探测 · S0-2 ===\n")
	fmt.Printf("%dx%d · CPU 逻辑核心 %d · 每步超时 12s\n", W, H, ncpu)
	fmt.Printf("30fps 预算 33.3ms/帧，60fps 预算 16.6ms/帧\n\n")

	base := go264.EncoderConfig{
		Width:   W, Height: H,
		FPSNum: 30, FPSDen: 1,
		GOPSize: 30,
		QP:      24,
		CABAC:   true,
	}

	// ---------- A. CPU 稳态 ----------
	fmt.Printf("[A] CPU 路径稳态（去掉首帧）\n")
	sw := base
	sw.ForceSoftware = true
	swMs := int64(0)
	r, hung := runSoft("CPU slices=1 稳态10帧", 12*time.Second, func() stepResult {
		return steady(sw, frame, 10)
	})
	if !hung && r.err == nil {
		swMs = r.ms
		fmt.Printf("  backend=%s  %dms/帧  %.1ffps  平均帧大小 %d 字节\n",
			r.backend, r.ms, 1000.0/float64(r.ms), r.size)
	} else if r.err != nil {
		fmt.Printf("  失败: %v\n", r.err)
	}

	swN := sw
	swN.Slices = ncpu
	r, hung = runSoft(fmt.Sprintf("CPU slices=%d 稳态10帧", ncpu), 20*time.Second, func() stepResult {
		return steady(swN, frame, 10)
	})
	if !hung && r.err == nil {
		accel := float64(0)
		if r.ms > 0 && swMs > 0 {
			accel = float64(swMs) / float64(r.ms)
		}
		fmt.Printf("  backend=%s  %dms/帧  %.1ffps  平均帧大小 %d 字节（相对 slices=1 加速 %.2fx）\n",
			r.backend, r.ms, 1000.0/float64(r.ms), r.size, accel)
	}

	swZ := sw
	swZ.MotionSearch = go264.MotionSearchZero // 最省的运动搜索
	r, hung = runSoft("CPU slices=1 + MotionSearchZero 稳态10帧", 20*time.Second, func() stepResult {
		return steady(swZ, frame, 10)
	})
	if !hung && r.err == nil {
		fmt.Printf("  backend=%s  %dms/帧  %.1ffps  平均帧大小 %d 字节\n",
			r.backend, r.ms, 1000.0/float64(r.ms), r.size)
	}

	// ---------- B. MF 挂起能否绕过 ----------
	fmt.Printf("\n[B] Media Foundation 挂起绕过尝试\n")

	// B1: 原样（对照）
	_, hung = runSoft("MF 原样 Encode 首帧", 12*time.Second, func() stepResult {
		enc, err := go264.NewEncoder(base)
		if err != nil {
			return stepResult{err: err}
		}
		defer enc.Close()
		t0 := time.Now()
		pkt, err := enc.Encode(frame)
		return stepResult{backend: enc.Backend(), ms: time.Since(t0).Milliseconds(), size: len(pkt), err: err}
	})
	if !hung {
		fmt.Printf("  ✅ 未挂起（与上一轮结论不同，需重测）\n")
	}

	// B2: LockOSThread（COM 线程亲和性）
	_, hung = runSoft("MF + runtime.LockOSThread", 12*time.Second, func() stepResult {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		enc, err := go264.NewEncoder(base)
		if err != nil {
			return stepResult{err: err}
		}
		defer enc.Close()
		t0 := time.Now()
		pkt, err := enc.Encode(frame)
		return stepResult{backend: enc.Backend(), ms: time.Since(t0).Milliseconds(), size: len(pkt), err: err}
	})
	if !hung {
		fmt.Printf("  ✅ LockOSThread 后未挂起 → 线程亲和性问题，可绕过\n")
	}

	// B3: CoInitializeEx(MULTITHREADED)
	_, hung = runSoft("MF + CoInitializeEx(MTA)", 12*time.Second, func() stepResult {
		procCoIn.Call(0, COINIT_MULTITHREADED)
		enc, err := go264.NewEncoder(base)
		if err != nil {
			return stepResult{err: err}
		}
		defer enc.Close()
		t0 := time.Now()
		pkt, err := enc.Encode(frame)
		return stepResult{backend: enc.Backend(), ms: time.Since(t0).Milliseconds(), size: len(pkt), err: err}
	})
	if !hung {
		fmt.Printf("  ✅ CoInitializeEx 后未挂起 → COM 未初始化导致，可绕过\n")
	}

	fmt.Printf("\n=== 探测结束 ===\n\n")
	os.Exit(0)
}
