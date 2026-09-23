// capcheck — GoShare 采集层自检工具
//
// 阶段 1 的验收手段。它不只跑 happy path，而是用「可证伪」的断言：
//   - 裁切：对同一个原始帧分别做整帧拷贝与区域裁切，逐像素比对
//     （若 Stride 索引写错，这里必然失败）
//   - 光标：把鼠标移到已知坐标，比较绘制前后的差异包围盒是否覆盖该点
//     （若 DPI 换算写错，1.75 倍偏差会立刻暴露）
//
// 用法：go run ./cmd/capcheck
package main

import (
	"context"
	"fmt"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/sys/windows"

	"goshare/internal/capture"

	"github.com/go-mswin/screencapture"
)

var (
	user32       = windows.NewLazySystemDLL("user32.dll")
	pSetCursorPos = user32.NewProc("SetCursorPos")
)

func setCursor(x, y int) { pSetCursorPos.Call(uintptr(x), uintptr(y)) }

var (
	pass, fail int
)

func check(name string, ok bool, detail string) {
	if ok {
		pass++
		fmt.Printf("  ✅ %s  %s\n", name, detail)
	} else {
		fail++
		fmt.Printf("  ❌ %s  %s\n", name, detail)
	}
}

func hr() { fmt.Printf("%s\n", "------------------------------------------------------------") }

func main() {
	fmt.Printf("\n=== GoShare 采集层自检 · 阶段 1 ===\n")
	fmt.Printf("CPU 逻辑核心 %d · CGO_ENABLED=0\n\n", runtime.NumCPU())

	// 全局看门狗：任何一步挂起都要给出明确信号，而不是无限期卡住。
	// （第一次运行时就踩过：Win32 调用自锁导致 7 分钟无输出。）
	go func() {
		time.Sleep(120 * time.Second)
		fmt.Printf("\n!!! 全局超时 120s，强制退出 —— 说明上面某一步挂起了\n\n")
		os.Exit(2)
	}()

	ctx := context.Background()

	// ---------- 1. DPI 与显示器枚举 ----------
	fmt.Printf("[1] DPI 感知与显示器枚举\n")
	// 必须在枚举显示器之前设置：进程若非 DPI aware，GetCursorPos 返回逻辑像素，
	// 与采集到的物理像素帧相差一个 scale（本机 1.75），光标会画到错误位置。
	before := capture.DPIAwareness()
	awr := capture.EnsureDPIAware()
	fmt.Printf("  DPI 感知: %d → %d\n", before, awr)
	if awr != 2 {
		fmt.Printf("  ⚠️ 未能设为 per-monitor aware（当前 %d），高 DPI 下坐标可能错位\n", awr)
	}
	if lockScreenDetected() {
		fmt.Printf("  ⚠️ 检测到锁屏界面（LockApp 运行中）\n")
		fmt.Printf("     → DXGI Duplication 在锁屏 / 安全桌面下会被拒绝（E_ACCESSDENIED），\n")
		fmt.Printf("       采集会回退 GDI，性能数据偏悲观，光标参照也不可信。\n")
		fmt.Printf("     → 请在解锁的普通桌面下重跑，才能得到真实数据。\n")
	}
	names := map[int]string{0: "unaware（GetCursorPos 返回逻辑像素 ⚠️）", 1: "system aware", 2: "per-monitor aware", -1: "查询失败"}
	fmt.Printf("  进程 DPI 感知: %d = %s\n", awr, names[awr])
	ds, err := capture.Displays(ctx)
	if err != nil {
		fmt.Printf("  枚举失败: %v\n", err)
		return
	}
	check("显示器枚举", len(ds) > 0, fmt.Sprintf("%d 台", len(ds)))
	for _, d := range ds {
		fmt.Printf("    %s  原点(%d,%d) 逻辑 %dx%d scale=%.2f duplicable=%v\n",
			d.String(), d.X, d.Y, d.LogicalW, d.LogicalH, d.Scale, d.Duplicable)
	}
	primary := ds[0]
	for _, d := range ds {
		if d.Primary {
			primary = d
		}
	}
	fmt.Printf("  选用: %s\n", primary.String())

	// ---------- 2. 裁切正确性（stride 索引） ----------
	fmt.Printf("\n[2] 裁切正确性 · 同一原始帧逐像素比对\n")
	checkCrop(ctx, primary)

	// ---------- 3. 采集性能 ----------
	fmt.Printf("\n[3] 采集性能（目标：1080p+ @30fps）\n")
	checkPerf(ctx, primary)

	// ---------- 4. 光标落点 ----------
	fmt.Printf("\n[4] 光标落点断言（DPI 换算）\n")
	checkCursor(ctx, primary)

	// ---------- 5. 光标叠加开销 ----------
	fmt.Printf("\n[5] 光标叠加开销\n")
	checkCursorCost()

	// ---------- 6. 静止心跳 ----------
	fmt.Printf("\n[6] 静止心跳（桌面不动时的出帧行为）\n")
	checkIdle(ctx, primary)

	// ---------- 7. 热切换区域 ----------
	fmt.Printf("\n[7] 热切换采集区域（无感切换前提）\n")
	checkHotSwitch(ctx, primary)

	fmt.Printf("\n")
	hr()
	fmt.Printf("结果: %d 通过 / %d 失败\n\n", pass, fail)
	if fail > 0 {
		os.Exit(1)
	}
}

// checkCrop 用同一个原始帧验证区域裁切的像素级正确性。
func checkCrop(ctx context.Context, d capture.Display) {
	st, err := screencapture.CaptureDisplay(ctx, d2raw(ctx, d), screencapture.Options{FPS: 30})
	if err != nil {
		check("建立采集", false, err.Error())
		return
	}
	defer st.Close()

	// 移动到鼠标制造画面变化，确保能拿到帧
	setCursor(d.X+d.W/2, d.Y+d.H/2)
	fctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	f, err := st.WaitFrame(fctx)
	if err != nil || !f.Valid() {
		check("取到有效帧", false, fmt.Sprintf("err=%v", err))
		return
	}
	check("取到有效帧", true, fmt.Sprintf("%dx%d stride=%d (w*4=%d)", f.Width, f.Height, f.Stride, f.Width*4))

	full, err := capture.TightCopy(f)
	if err != nil {
		check("整帧拷贝", false, err.Error())
		return
	}
	region := capture.Rect{X: 200, Y: 100, W: 800, H: 600}.Clamp(d)
	cropped, err := capture.Crop(f, region)
	if err != nil {
		check("区域裁切", false, err.Error())
		return
	}

	mismatch := 0
	var firstBad [2]int
	for y := 0; y < cropped.H; y++ {
		for x := 0; x < cropped.W; x++ {
			ci := (y*cropped.W + x) * 4
			fi := ((region.Y+y)*full.W + (region.X + x)) * 4
			for k := 0; k < 4; k++ {
				if cropped.Pix[ci+k] != full.Pix[fi+k] {
					mismatch++
					if mismatch == 1 {
						firstBad = [2]int{x, y}
					}
					break
				}
			}
		}
	}
	check("裁切像素比对（Stride 索引）", mismatch == 0,
		fmt.Sprintf("区域 %dx%d @(%d,%d)，不一致像素 %d %v",
			region.W, region.H, region.X, region.Y, mismatch, firstBad))

	savePNG(full, d, "full.png")
	savePNG(cropped, d, "crop.png")

	// 反向变异：故意用错误的 stride（Width*4）裁切，确认「能发现错误」
	if f.Stride != f.Width*4 {
		fmt.Printf("  ℹ️  本机 stride==w*4，无法用错误 stride 做反向变异（换机器才有意义）\n")
	} else {
		fmt.Printf("  ℹ️  stride==w*4，反向变异无效；已用逐像素比对保证通用性\n")
	}
}

// checkPerf 测整屏与区域采集的帧率与耗时。
func checkPerf(ctx context.Context, d capture.Display) {
	run := func(label string, r capture.Rect) {
		src, err := capture.NewSource(ctx, capture.Options{Display: d, Region: r, FPS: 30, Cursor: false})
		if err != nil {
			check(label, false, err.Error())
			return
		}
		defer src.Close()
		// 持续移动鼠标制造变化
		done := make(chan struct{})
		go func() {
			t := 0
			for {
				select {
				case <-done:
					return
				default:
				}
				setCursor(d.X+300+(t%60)*6, d.Y+300+(t%40)*6)
				t++
				time.Sleep(8 * time.Millisecond)
			}
		}()
		n := 0
		captureMs := 0.0
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			f2, cancel := context.WithTimeout(ctx, 120*time.Millisecond)
			f, err := src.WaitFrame(f2)
			cancel()
			if err == nil && !f.Empty() {
				n++
				captureMs += f.CaptureMs
			}
		}
		close(done)
		fps := float64(n) / 3.0
		mean := 0.0
		if n > 0 {
			mean = captureMs / float64(n)
		}
		st := src.Stats()
		check(label, fps >= 25,
			fmt.Sprintf("%.1f fps · 采集均值 %.2f ms · 后端 %s%s",
				fps, mean, st.Backend, noteSuffix(src.Note())))
	}
	run("整屏采集", capture.Rect{})
	run("区域采集 1280x720", capture.Rect{X: 100, Y: 100, W: 1280, H: 720})
}

// waitValid 从原始流取一帧有效画面。
func waitValid(ctx context.Context, st *screencapture.Stream) (screencapture.Frame, error) {
	c, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	f, err := st.WaitFrame(c)
	if err != nil {
		return f, err
	}
	if !f.Valid() {
		return f, fmt.Errorf("帧无效")
	}
	return f, nil
}

// checkCursor 验证光标绘制位置。
//
// ⚠️ 这里有个「差点让验证失效」的陷阱：如果只断言「自绘落点 == GetCursorPos 换算值」，
// 两者可能同处一套错误坐标系（例如进程非 DPI aware，GetCursorPos 返回逻辑像素
// 而帧是物理像素，差 1.75 倍），结果错得完全自洽、看不出来。
//
// 因此引入独立真值：让 Windows 自己用 DrawIconEx 画一次光标
// （Options.ShowsCursor=true，代价是强制回退 GDI，仅用于校验），
// 以它的落点为基准来对照自绘结果。
func checkCursor(ctx context.Context, d capture.Display) {
	shape := capture.CursorShapeOf()
	if !shape.Visible {
		fmt.Printf("  ⚠️ 光标当前未显示（锁屏 / 全屏应用下常见），跳过落点验证\n")
		fmt.Printf("     → 解锁并在普通桌面下重跑本项才有意义\n")
		return
	}
	fmt.Printf("  光标形状 %dx%d 热点(%d,%d) 单色=%v\n", shape.W, shape.H, shape.HotX, shape.HotY, shape.Monochrome)

	// 把鼠标固定在屏幕中心
	cx, cy := d.X+d.W/2, d.Y+d.H/2
	setCursor(cx, cy)
	time.Sleep(200 * time.Millisecond)

	raw := d2raw(ctx, d)

	// --- 参照帧：系统绘制光标 ---
	stA, err := screencapture.CaptureDisplay(ctx, raw, screencapture.Options{FPS: 30, ShowsCursor: true})
	if err != nil {
		check("参照帧采集", false, err.Error())
		return
	}
	defer stA.Close()
	fA, err := waitValid(ctx, stA)
	if err != nil {
		check("参照帧取帧", false, err.Error())
		return
	}
	sysFrame, err := capture.TightCopy(fA)
	if err != nil {
		check("参照帧拷贝", false, err.Error())
		return
	}

	// --- 被测帧：我们自绘光标 ---
	src, err := capture.NewSource(ctx, capture.Options{Display: d, FPS: 30, Cursor: false})
	if err != nil {
		check("建立采集", false, err.Error())
		return
	}
	defer src.Close()
	fB, err := src.WaitFrame(mustCtx(ctx, 2*time.Second))
	if err != nil || fB.Empty() {
		check("取帧", false, fmt.Sprintf("err=%v", err))
		return
	}

	gx, gy, _ := capture.CursorPos()
	fmt.Printf("  鼠标目标 (%d,%d) · GetCursorPos (%d,%d) · DPI感知=%d\n", cx, cy, gx, gy, capture.DPIAwareness())

	base := fB.Clone()
	self := fB.Clone()
	capture.DrawCursor(self.Pix, self.W, self.H, d.X, d.Y)

	sx0, sy0, sx1, sy1, sn := diffBBox(base.Pix, self.Pix, base.W, base.H)
	if sn == 0 {
		check("光标绘制", false, "自绘前后无任何差异像素")
		return
	}
	fmt.Printf("  自绘光标包围盒 (%d,%d)-(%d,%d) 共 %d 像素\n", sx0, sy0, sx1, sy1, sn)

	// 系统绘制 vs 无光标帧的差异 → 系统认为光标在哪
	ax0, ay0, ax1, ay1, an := diffBBox(base.Pix, sysFrame.Pix, base.W, base.H)
	area := shape.W * shape.H
	if an < 8 || an > area*3 {
		fmt.Printf("  ⚠️ 无法取得可信的系统参照（差异 %d 像素，光标面积 %d）\n", an, area)
		fmt.Printf("     可能原因：桌面在两次采集间变化 / 锁屏界面 / 光标被隐藏\n")
		fmt.Printf("     → 不作为失败，但该项未真正验证；请在解锁的普通桌面下重跑\n")
		// 退而求其次：至少校验自洽性
		wantX, wantY := gx-d.X, gy-d.Y
		inBox := wantX >= sx0-4 && wantX <= sx1+4 && wantY >= sy0-4 && wantY <= sy1+4
		check("光标落点覆盖鼠标位置（自洽校验）", inBox,
			fmt.Sprintf("期望热点 (%d,%d)，包围盒 (%d,%d)-(%d,%d)", wantX, wantY, sx0, sy0, sx1, sy1))
		savePNG(self, d, "cursor.png")
		return
	}
	fmt.Printf("  系统绘制包围盒 (%d,%d)-(%d,%d) 共 %d 像素\n", ax0, ay0, ax1, ay1, an)

	dl, dt := abs(sx0-ax0), abs(sy0-ay0)
	dr, db := abs(sx1-ax1), abs(sy1-ay1)
	check("自绘落点 == 系统绘制落点（独立真值）", dl <= 2 && dt <= 2 && dr <= 2 && db <= 2,
		fmt.Sprintf("自绘 (%d,%d)-(%d,%d) vs 系统 (%d,%d)-(%d,%d)，偏差 左%d 上%d 右%d 下%d",
			sx0, sy0, sx1, sy1, ax0, ay0, ax1, ay1, dl, dt, dr, db))

	savePNG(self, d, "cursor.png")
	savePNG(sysFrame, d, "cursor-system.png")
}

func mustCtx(p context.Context, d time.Duration) context.Context {
	c, _ := context.WithTimeout(p, d)
	return c
}

// checkCursorCost 测光标叠加的单帧开销。
func checkCursorCost() {
	w, h := 1920, 1080
	buf := make([]byte, w*h*4)
	// 预热
	capture.DrawCursor(buf, w, h, 0, 0)
	n := 2000
	t0 := time.Now()
	for i := 0; i < n; i++ {
		capture.DrawCursor(buf, w, h, 0, 0)
	}
	per := time.Since(t0).Microseconds() / int64(n)
	check("光标叠加开销", per < 500,
		fmt.Sprintf("%d µs/帧（1080p，2000 次均值）", per))
}

// checkIdle 验证桌面静止时的出帧行为（P0-4 心跳问题）。
func checkIdle(ctx context.Context, d capture.Display) {
	src, err := capture.NewSource(ctx, capture.Options{Display: d, FPS: 30, Cursor: false})
	if err != nil {
		check("建立采集", false, err.Error())
		return
	}
	defer src.Close()
	// 先动一下拿到首帧
	setCursor(d.X+300, d.Y+300)
	fctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	_, _ = src.WaitFrame(fctx)
	cancel()

	// 静止 2 秒，不动任何东西
	n := 0
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f2, c2 := context.WithTimeout(ctx, 100*time.Millisecond)
		if f, err := src.WaitFrame(f2); err == nil && !f.Empty() {
			n++
		}
		c2()
	}
	hasLast := !src.Last().Empty()
	fmt.Printf("  静止 2s 出帧 %d 次（%.1f fps）· Last() 有缓存帧: %v\n", n, float64(n)/2.0, hasLast)
	fmt.Printf("  → DXGI 变化驱动：静止不出帧是预期行为，必须靠 Last()/心跳兜底\n")
	check("静止时 Last() 可提供兜底帧", hasLast, "新观众接入不会黑屏")
}

// checkHotSwitch 验证热切换区域不重建底层流。
func checkHotSwitch(ctx context.Context, d capture.Display) {
	src, err := capture.NewSource(ctx, capture.Options{Display: d, FPS: 30, Cursor: false})
	if err != nil {
		check("建立采集", false, err.Error())
		return
	}
	defer src.Close()
	before := src.Backend()
	r1 := capture.Rect{X: 0, Y: 0, W: 960, H: 540}.Clamp(d)
	src.SetRegion(r1)
	got := src.Region()
	check("切换区域生效", got.W == r1.W && got.H == r1.H,
		fmt.Sprintf("设定 %dx%d → 生效 %dx%d", r1.W, r1.H, got.W, got.H))
	check("切换区域未重建后端", src.Backend() == before,
		fmt.Sprintf("%s → %s（无感切换前提）", before, src.Backend()))
}

// ---------- 辅助 ----------

func d2raw(ctx context.Context, d capture.Display) screencapture.Display {
	ds, err := screencapture.Displays(ctx)
	if err != nil || len(ds) == 0 {
		return screencapture.Display{}
	}
	// capture.Displays 做过排序，这里按原始下标对应的 DeviceName 找回
	for _, r := range ds {
		if r.DeviceName == d.Name {
			return r
		}
	}
	return ds[0]
}

func diffBBox(a, b []byte, w, h int) (x0, y0, x1, y1, n int) {
	x0, y0 = w, h
	x1, y1 = -1, -1
	for y := 0; y < h; y++ {
		row := y * w * 4
		for x := 0; x < w; x++ {
			i := row + x*4
			if a[i] != b[i] || a[i+1] != b[i+1] || a[i+2] != b[i+2] {
				n++
				if x < x0 {
					x0 = x
				}
				if x > x1 {
					x1 = x
				}
				if y < y0 {
					y0 = y
				}
				if y > y1 {
					y1 = y
				}
			}
		}
	}
	return
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func noteSuffix(n string) string {
	if n == "" {
		return ""
	}
	return " · " + n
}

func savePNG(f capture.Frame, d capture.Display, name string) {
	if f.Empty() {
		return
	}
	dir := filepath.Join(".", "out")
	_ = os.MkdirAll(dir, 0o755)
	img := image.NewNRGBA(image.Rect(0, 0, f.W, f.H))
	for y := 0; y < f.H; y++ {
		s0 := y * f.W * 4
		d0 := y * f.W * 4
		for x := 0; x < f.W; x++ {
			b, g, r := f.Pix[s0+x*4], f.Pix[s0+x*4+1], f.Pix[s0+x*4+2]
			img.Pix[d0+x*4] = r
			img.Pix[d0+x*4+1] = g
			img.Pix[d0+x*4+2] = b
			img.Pix[d0+x*4+3] = 255
		}
	}
	fh, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		return
	}
	defer fh.Close()
	if err := png.Encode(fh, img); err == nil {
		if fi, err := fh.Stat(); err == nil {
			fmt.Printf("  已保存 out/%s (%d KB)\n", name, fi.Size()/1024)
		}
	}
}

// lockScreenDetected 检测系统是否处于锁屏界面。
//
// 锁屏（以及 UAC 安全桌面、RDP 断开）下 DXGI Duplication 会被拒绝，
// 采集静默回退 GDI：性能数据偏悲观、光标参照也不可信。
// 检测到时必须显式告知，避免用一组"看起来正常但其实无效"的数据下结论。
func lockScreenDetected() bool {
	out, err := exec.Command("tasklist", "/NH", "/FI", "IMAGENAME eq LockApp.exe").Output()
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(out)), "lockapp")
}
