// uicheck — 界面离屏渲染校验。
//
// 为什么需要它：截屏（cmd/shot）依赖真实桌面，锁屏 / 无头 / 远程会话下拿到的
// 是锁屏画面，像素证据无效（阶段 3 踩过）。Gio 支持离屏渲染，不需要窗口、
// 不需要桌面合成，锁屏照样出图 —— 于是"界面到底画出来没有"这类问题
// 可以在任何环境下被验证，而不是只能靠人眼看。
//
// 它检查的是：每个页面渲染后画布上的非黑像素占比、以及几个关键色块是否存在。
// 全黑 = 版面铺满了背景色而内容没画出来（R24 那类事故的典型症状）。
package main

import (
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"log"
	"os"
	"path/filepath"
	"time"

	"gioui.org/gpu/headless"
	"gioui.org/io/input"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/unit"
	"gioui.org/widget/material"

	"goshare/internal/capture"
	"goshare/internal/ui"
)

func main() {
	outDir := flag.String("out", "out", "PNG 输出目录")
	w := flag.Int("w", 1180, "画布宽（逻辑像素）")
	h := flag.Int("h", 780, "画布高")
	// 基线放在 testdata 下（out/ 被 .gitignore 忽略，不能当基线仓），
	// 内容是合成画面 + 假数据，不含真实屏幕内容，可以入库。
	goldenDir := flag.String("golden", "testdata/ui-golden", "基线目录：非空时逐页与基线图做像素比对（R31）")
	saveGolden := flag.Bool("save-golden", false, "把本次渲染结果写成基线（界面有故意改动时才用，并说明改了什么）")
	tol := flag.Float64("tol", 0.5, "允许的像素差异百分比（默认 0.5%）")
	flag.Parse()

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		log.Fatalf("创建输出目录失败: %v", err)
	}
	if *goldenDir != "" {
		if err := os.MkdirAll(*goldenDir, 0o755); err != nil {
			log.Fatalf("创建基线目录失败: %v", err)
		}
	}

	win, err := headless.NewWindow(*w, *h)
	if err != nil {
		fmt.Fprintf(os.Stderr, "离屏渲染初始化失败: %v\n", err)
		os.Exit(1)
	}
	defer win.Release()

	th := ui.NewTheme()
	pass, fail := 0, 0
	check := func(name string, ok bool, detail string) {
		mark := "PASS"
		if !ok {
			mark = "FAIL"
			fail++
		} else {
			pass++
		}
		fmt.Printf("[%s] %-22s %s\n", mark, name, detail)
	}

	for _, c := range cases() {
		img, err := render(win, th, *w, *h, c.name, c.setup)
		if err != nil {
			check(c.name, false, "渲染失败: "+err.Error())
			continue
		}
		path := filepath.Join(*outDir, "ui-"+c.name+".png")
		if err := savePNG(path, img); err != nil {
			check(c.name, false, "保存失败: "+err.Error())
			continue
		}
		st := stat(img)
		// 判据一：非黑占比必须够高（版面确实画了东西），且主色不能是纯背景。
		ok := st.nonBlack > 0.02 && st.distinct > 8
		detail := fmt.Sprintf("非黑 %.1f%% · 不同色 %d 种 · 亮部 %.1f%%", st.nonBlack*100, st.distinct, st.bright*100)

		// 判据二（R31）：与基线图逐像素比对。
		// 判据一太粗 —— R30 那种"按钮整列消失"只改动 2~4% 的像素，
		// 非黑/色数/亮部三项几乎不动，粗判据照样 8/8 通过。必须逐像素兜住。
		if *goldenDir != "" {
			gp := filepath.Join(*goldenDir, "ui-"+c.name+".png")
			if *saveGolden {
				if err := savePNG(gp, img); err != nil {
					check(c.name, false, "写基线失败: "+err.Error())
					continue
				}
				detail += " · 已写基线"
			} else {
				g, err := loadRGBA(gp)
				if err != nil {
					check(c.name, false, "基线缺失: "+gp)
					continue
				}
				d := diffRGBA(g, img)
				if d.pct > *tol {
					ok = false
					detail += fmt.Sprintf(" · 与基线差 %.2f%%（阈值 %.1f%%）· 差异区 %v %dx%d",
						d.pct, *tol, d.box.Min, d.box.Dx(), d.box.Dy())
				} else {
					detail += fmt.Sprintf(" · 与基线差 %.3f%%", d.pct)
				}
			}
		}
		detail += " · → " + path
		check(c.name, ok, detail)
	}

	fmt.Printf("\n合计 %d 通过 / %d 失败\n", pass, fail)
	if fail > 0 {
		os.Exit(1)
	}
}

// diffResult 是两张图的像素差异。
type diffResult struct {
	pct float64      // 差异像素占比（%）
	box image.Rectangle // 差异包围盒
}

// diffRGBA 逐像素比较。阈值取四通道差之和 > 24（单通道平均差 6 以上才算），
// 用来吃掉抗锯齿带来的 ±1～2 抖动。
func diffRGBA(a, b *image.RGBA) diffResult {
	if a.Bounds() != b.Bounds() {
		return diffResult{pct: 100, box: a.Bounds()}
	}
	w, h := a.Bounds().Dx(), a.Bounds().Dy()
	diff, first := 0, true
	var box image.Rectangle
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := y*a.Stride + x*4
			j := y*b.Stride + x*4
			d := absi(int(a.Pix[i])-int(b.Pix[j])) +
				absi(int(a.Pix[i+1])-int(b.Pix[j+1])) +
				absi(int(a.Pix[i+2])-int(b.Pix[j+2])) +
				absi(int(a.Pix[i+3])-int(b.Pix[j+3]))
			if d > 24 {
				diff++
				p := image.Pt(x, y)
				if first {
					box = image.Rect(p.X, p.Y, p.X+1, p.Y+1)
					first = false
				} else {
					box = box.Union(image.Rect(p.X, p.Y, p.X+1, p.Y+1))
				}
			}
		}
	}
	return diffResult{pct: float64(diff) / float64(w*h) * 100, box: box}
}

func absi(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// loadRGBA 读一张 PNG 并统一成 *image.RGBA，方便与渲染结果逐像素比。
func loadRGBA(path string) (*image.RGBA, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	src, _, err := image.Decode(f)
	if err != nil {
		return nil, err
	}
	dst := image.NewRGBA(src.Bounds())
	draw.Draw(dst, dst.Bounds(), src, src.Bounds().Min, draw.Src)
	return dst, nil
}

type testCase struct {
	name  string
	setup func(*ui.Shell)
}

func cases() []testCase {
	// 假状态：只为把版面填满，验证"该画的地方都画出来了"
	fakeAddrs := []string{"192.168.250.180:9000", "192.168.11.248:9000", "172.17.51.33:9000"}
	now := time.Now()

	return []testCase{
		{name: "home", setup: nil},
		{name: "setup", setup: nil},
		{name: "sharing", setup: func(s *ui.Shell) {
			s.SetShareState(ui.ShareState{
				Active: true, Code: "482913", Addrs: fakeAddrs, Port: 9000,
				PresetName: "最大", Paused: false,
				Viewers: []ui.ViewerRow{
					{Name: "GKYN20230046", IP: "192.168.250.180", Since: now.Add(-75 * time.Second)},
					{Name: "DESKTOP-7K2", IP: "192.168.250.191", Since: now.Add(-12 * time.Second)},
				},
				HUD:        "发送 29.8 fps · 7.4 Mbps\n编码 9.9 ms · 采集 duplication\n已发 1842 帧 · 观众 2 人\n共享 整屏",
				RegionText: "整屏",
			})
			s.SetSetupNote("")
			s.PushPreview(synth(480, 270))
		}},
		{name: "sharing-paused", setup: func(s *ui.Shell) {
			s.SetShareState(ui.ShareState{
				Active: true, Code: "482913", Addrs: fakeAddrs, Port: 9000,
				PresetName: "流畅30", Paused: true,
				Viewers:    []ui.ViewerRow{{Name: "GKYN20230046", IP: "192.168.250.180", Since: now.Add(-40 * time.Second)}},
				HUD:        "发送 0.8 fps · 0.1 Mbps\n编码 6.2 ms · 采集 duplication\n已发 1901 帧 · 观众 1 人\n共享 区域 1280×720",
				RegionText: "区域 1280×720",
			})
			s.PushPreview(synth(480, 270))
		}},
		{name: "join", setup: func(s *ui.Shell) {
			s.SetJoinState(ui.JoinState{
				Status: "发现 2 个分享，点击填入地址",
				Found: []ui.FoundRow{
					{Name: "GKYN20230046", Addr: "192.168.250.180:9000"},
					{Name: "DESKTOP-7K2", Addr: "192.168.250.191:9000"},
				},
			})
		}},
		{name: "join-error", setup: func(s *ui.Shell) {
			s.SetJoinState(ui.JoinState{
				Err: "接入失败：连接 192.168.1.10:9000 失败（地址与授权码都对，但 UDP 打不通 —— 检查对方防火墙是否放行 UDP）",
			})
		}},
		{name: "viewing", setup: func(s *ui.Shell) {
			s.SetJoinState(ui.JoinState{Status: "GKYN20230046 · 29.4 fps · 7.1 Mbps · 解码 4.9 ms"})
			s.SetLastFrame(synth(960, 540))
		}},
		{name: "viewing-zoom", setup: func(s *ui.Shell) {
			s.SetJoinState(ui.JoinState{Status: "GKYN20230046 · 29.4 fps · 7.1 Mbps · 解码 4.9 ms"})
			// 1:1 模式：画面比窗口大（只显示局部、可平移），原始像素不缩放
			s.SetLastFrame(synth(1920, 1080))
			s.SetZoom100(true)
		}},
		{name: "region", setup: func(s *ui.Shell) {
			s.SetSnapshot(synth(1440, 900))
			// 模拟"拖到一半"的状态：拖拽框 + 已算出的桌面区域
			s.SetDragPreview(image.Pt(760, 150), image.Pt(1080, 420))
			s.SetRegionRect(capture.Rect{X: 613, Y: 121, W: 640, H: 511})
		}},
	}
}

// render 把某个页面离屏渲染成 RGBA 图。
func render(win *headless.Window, th *material.Theme, w, h int, name string, setup func(*ui.Shell)) (*image.RGBA, error) {
	s := ui.NewShell(ui.ShellConfig{
		Title:    "uicheck",
		Displays: fakeDisplays(),
		Presets:  []string{"最大", "流畅60", "流畅30", "急速"},
	})
	if setup != nil {
		setup(s)
	}
	s.Go(routeOf(name))

	var ops op.Ops
	r := new(input.Router)
	gtx := layout.Context{
		Ops:         &ops,
		Metric:      unit.Metric{PxPerDp: 1, PxPerSp: 1},
		Constraints: layout.Exact(image.Pt(w, h)),
		Now:         time.Now(),
		Source:      r.Source(),
	}
	s.Layout(gtx, th)
	r.Frame(&ops)

	if err := win.Frame(&ops); err != nil {
		return nil, err
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	if err := win.Screenshot(img); err != nil {
		return nil, err
	}
	return img, nil
}

func routeOf(name string) ui.Route {
	switch name {
	case "setup":
		return ui.RouteSetup
	case "sharing", "sharing-paused":
		return ui.RouteSharing
	case "join", "join-error":
		return ui.RouteJoin
	case "viewing", "viewing-zoom":
		return ui.RouteViewing
	case "region":
		return ui.RouteRegion
	default:
		return ui.RouteHome
	}
}

func fakeDisplays() []capture.Display {
	return []capture.Display{
		{ID: 0, Name: "DISPLAY1", W: 2880, H: 1800, LogicalW: 1645, LogicalH: 1028, DPI: 168, Scale: 1.75, Primary: true, Duplicable: true},
		{ID: 1, Name: "DISPLAY2", W: 1920, H: 1080, LogicalW: 1920, LogicalH: 1080, DPI: 96, Scale: 1, Duplicable: true},
	}
}

// synth 造一张结构化测试图（渐变 + 色块 + 网格），用作"画面"。
// 不用随机噪声：JPEG/压缩类统计对噪声没意义，且视觉上无法分辨内容是否真的画出来。
func synth(w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := y*img.Stride + x*4
			img.Pix[i] = uint8(30 + 200*x/max1(w))
			img.Pix[i+1] = uint8(40 + 180*y/max1(h))
			img.Pix[i+2] = uint8(90 + 120*(x+y)/max1(w+h))
			img.Pix[i+3] = 255
		}
	}
	// 几个高对比色块，便于判断内容确实被绘制
	blocks := []struct {
		x, y, w, h int
		c          color.NRGBA
	}{
		{w / 8, h / 8, w / 5, h / 5, color.NRGBA{R: 235, G: 245, B: 255, A: 255}},
		{w / 2, h / 3, w / 4, h / 6, color.NRGBA{R: 250, G: 190, B: 40, A: 255}},
		{w / 3, h * 2 / 3, w / 3, h / 7, color.NRGBA{R: 60, G: 200, B: 120, A: 255}},
	}
	for _, b := range blocks {
		for y := b.y; y < b.y+b.h && y < h; y++ {
			for x := b.x; x < b.x+b.w && x < w; x++ {
				i := y*img.Stride + x*4
				img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = b.c.R, b.c.G, b.c.B, b.c.A
			}
		}
	}
	return img
}

func max1(v int) int {
	if v < 1 {
		return 1
	}
	return v
}

type stats struct {
	nonBlack float64 // 非黑像素占比
	bright   float64 // 亮度 > 100 的占比
	distinct int     // 抽样得到的不同颜色数
}

// stat 统计画面内容。判据是"画布上确实有内容"，全黑即失败。
func stat(img *image.RGBA) stats {
	b := img.Bounds()
	n, nb, br := 0, 0, 0
	seen := map[uint32]struct{}{}
	step := 2
	for y := b.Min.Y; y < b.Max.Y; y += step {
		for x := b.Min.X; x < b.Max.X; x += step {
			i := y*img.Stride + x*4
			r, g, bl := int(img.Pix[i]), int(img.Pix[i+1]), int(img.Pix[i+2])
			lum := (77*r + 150*g + 29*bl) >> 8
			if lum > 8 {
				nb++
			}
			if lum > 100 {
				br++
			}
			// 量化到 5 位一格，避免抗锯齿带来的伪多样
			key := uint32(r>>3)<<12 | uint32(g>>3)<<6 | uint32(bl>>3)
			seen[key] = struct{}{}
			n++
		}
	}
	if n == 0 {
		return stats{}
	}
	return stats{
		nonBlack: float64(nb) / float64(n),
		bright:   float64(br) / float64(n),
		distinct: len(seen),
	}
}

func savePNG(path string, img image.Image) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}
