// capbench — 阶段 0 · S0-1
// 验证 go-mswin/screencapture 在本机（Win10 LTSC 17763, 2880x1800 高DPI, Iris Xe）上：
//  1. 显示器枚举是否正确（含 DPI / 物理像素 vs 逻辑像素 / 是否可 DXGI 复制）
//  2. BackendAuto 实际落到哪个后端；DXGI 路径能否出帧
//  3. 高 DPI 下采到的帧是否等于真实物理像素（DPI awareness 是否正确）
//  4. Stride 是否 != Width*4（区域裁切必须按 stride 索引）
//  5. 桌面静止时是否完全不出帧（P0-4 心跳问题）
//  6. ShowsCursor 强制 GDI 后的性能代价
package main

import (
	"context"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/go-mswin/screencapture"
)

var (
	user32       = syscall.NewLazyDLL("user32.dll")
	setCursorPos = user32.NewProc("SetCursorPos")
)

func moveMouse(x, y int) { setCursorPos.Call(uintptr(x), uintptr(y)) }

func hr(n int) string { return strings.Repeat("-", n) }

func main() {
	fmt.Printf("\n=== screencapture 采集实测 · S0-1 ===\n")
	fmt.Printf("CPU 逻辑核心 %d · GOOS=windows · CGO_ENABLED=0\n", runtime.NumCPU())

	ctx := context.Background()

	// ---------- 1. 显示器枚举 ----------
	fmt.Printf("\n[1] 显示器枚举\n")
	ds, err := screencapture.Displays(ctx)
	if err != nil {
		fmt.Printf("Displays 失败: %v\n", err)
		return
	}
	fmt.Printf("显示器数量: %d\n", len(ds))
	for i, d := range ds {
		fmt.Printf("  [%d] %s\n", i, d.DeviceName)
		fmt.Printf("      物理像素 %dx%d  ·  逻辑(DIP) %dx%d  ·  DPI %d  ·  scale %.2f\n",
			d.PixelWidth, d.PixelHeight, d.Width, d.Height, d.DPI, d.Scale())
		fmt.Printf("      Bounds %s  ·  Work %s  ·  primary=%v\n", d.Bounds, d.Work, d.Primary)
		fmt.Printf("      Adapter=%d Output=%d  Duplicable=%v  Rotation=%s\n",
			d.AdapterIndex, d.OutputIndex, d.Duplicable(), d.Rotation)
	}
	if len(ds) == 0 {
		fmt.Printf("无显示器，终止\n")
		return
	}

	outDir := filepath.Join(".", "out")
	_ = os.MkdirAll(outDir, 0o755)

	// ---------- 2/3/4. 每个显示器：BackendAuto 采集 ----------
	for i, d := range ds {
		fmt.Printf("\n[2] 显示器[%d] BackendAuto 采集（静止 2s + 移动鼠标 1.5s）\n", i)
		probe(ctx, d, screencapture.Options{FPS: 30}, fmt.Sprintf("disp%d-auto", i), outDir)
	}

	// ---------- 5. 显式 GDI 对比 ----------
	fmt.Printf("\n[3] 显示器[0] 强制 BackendGDI（对比）\n")
	probe(ctx, ds[0], screencapture.Options{FPS: 30, Backend: screencapture.BackendGDI}, "disp0-gdi", outDir)

	// ---------- 6. ShowsCursor（会强制 GDI） ----------
	fmt.Printf("\n[4] 显示器[0] ShowsCursor=true（预期强制 GDI）\n")
	probe(ctx, ds[0], screencapture.Options{FPS: 30, ShowsCursor: true}, "disp0-cursor", outDir)

	fmt.Printf("\n%s\n", hr(70))
	fmt.Printf("产物目录: %s\n\n", outDir)
}

type phase struct {
	label  string
	frames int
	fresh  int
}

// probe 采集一个显示器的两阶段：静止 → 移动鼠标
func probe(ctx context.Context, d screencapture.Display, opt screencapture.Options, tag, outDir string) {
	s, err := screencapture.CaptureDisplay(ctx, d, opt)
	if err != nil {
		fmt.Printf("  CaptureDisplay 失败: %v\n", err)
		return
	}
	defer s.Close()

	fmt.Printf("  Backend=%s  Note=%q  Source=%s\n", s.Backend(), s.Note(), s.Source())

	// 阶段 A：完全静止 2 秒
	a := collect(ctx, s, 2*time.Second, false, nil)
	fmt.Printf("  [静止 2s]   取帧 %d 次，其中新鲜 %d 帧  →  %.1f fps\n",
		a.frames, a.fresh, float64(a.fresh)/2.0)

	// 阶段 B：移动鼠标制造变化 1.5 秒
	var first screencapture.Frame
	var prev screencapture.Frame
	havePrev := false
	diffPixels := 0
	b := collect(ctx, s, 1500*time.Millisecond, true, func(f screencapture.Frame) {
		if !first.Valid() {
			first = f
		}
		if havePrev {
			if n, ok := f.Differs(prev); ok {
				diffPixels += n
			}
		}
		prev = f
		havePrev = true
	})
	fmt.Printf("  [动鼠标 1.5s] 取帧 %d 次，其中新鲜 %d 帧  →  %.1f fps\n",
		b.frames, b.fresh, float64(b.fresh)/1.5)

	// 帧质量校验
	if first.Valid() {
		fmt.Printf("  帧尺寸 %dx%d  ·  Stride %d  ·  Width*4=%d  ·  stride!=w*4: %v\n",
			first.Width, first.Height, first.Stride, first.Width*4, first.Stride != first.Width*4)
		fmt.Printf("  显示器物理像素 %dx%d  →  采集尺寸 %s\n",
			d.PixelWidth, d.PixelHeight,
			map[bool]string{true: "匹配（DPI 正确）", false: "不匹配 ⚠️ DPI awareness 有问题"}[
				first.Width == d.PixelWidth && first.Height == d.PixelHeight])
		if u, uniform := first.Uniform(); uniform {
			fmt.Printf("  ⚠️  帧为单一颜色 %v —— 内容无效（黑屏/未采到）\n", u)
		} else {
			fmt.Printf("  内容非单色 ✓（有真实画面）\n")
		}
		if diffPixels > 0 {
			fmt.Printf("  帧间有变化 ✓（累计差异像素 %d）\n", diffPixels)
		} else {
			fmt.Printf("  ⚠️  帧间无变化\n")
		}
		// 存 PNG 供肉眼核对
		savePNG(filepath.Join(outDir, tag+".png"), first)
	} else {
		fmt.Printf("  ⚠️  未取到任何有效帧\n")
	}

	st := s.Stats()
	fmt.Printf("  Stats: FPS=%.1f  MeanCapture=%v  MeanWait=%v\n", st.FPS(), st.MeanCapture(), st.MeanWait())
	if err := s.Err(); err != nil {
		fmt.Printf("  Stream Err: %v\n", err)
	}
}

// collect 在 dur 内持续取帧；moveIt 为真时同时移动鼠标制造画面变化
func collect(ctx context.Context, s *screencapture.Stream, dur time.Duration, moveIt bool, onFrame func(screencapture.Frame)) phase {
	p := phase{}
	deadline := time.Now().Add(dur)
	tick := 0
	for time.Now().Before(deadline) {
		fctx, cancel := context.WithTimeout(ctx, 120*time.Millisecond)
		f, err := s.WaitFrame(fctx)
		cancel()
		if err == nil {
			p.frames++
			if f.Valid() {
				p.fresh++
				if onFrame != nil {
					onFrame(f)
				}
			}
		}
		if moveIt {
			// 在一个小范围内来回移动鼠标，制造画面变化
			x := 400 + (tick%40)*8
			y := 300 + (tick%30)*8
			moveMouse(x, y)
		}
		tick++
	}
	return p
}

func savePNG(path string, f screencapture.Frame) {
	img, err := f.NRGBAOpaque()
	if err != nil {
		fmt.Printf("  (PNG 失败: %v)\n", err)
		return
	}
	fh, err := os.Create(path)
	if err != nil {
		fmt.Printf("  (PNG 创建失败: %v)\n", err)
		return
	}
	defer fh.Close()
	if err := png.Encode(fh, img); err != nil {
		fmt.Printf("  (PNG 编码失败: %v)\n", err)
		return
	}
	if fi, err := fh.Stat(); err == nil {
		fmt.Printf("  已保存 %s (%d KB)\n", path, fi.Size()/1024)
	}
}
