// sharedemo — 阶段 2 · 完整链路演示
//
// 采集主屏 → MJPEG 条带编码 → pion DataChannel → 重组 → 解码 → 副屏显示。
// 观看窗刻意放到副屏：本机是 Win10 LTSC 17763，没有 WDA_EXCLUDEFROMCAPTURE，
// 只能靠物理隔离避免"自己采集自己"的无限套娃（P0-1）。
package main

import (
	"context"
	"flag"
	"fmt"
	"image"
	"log"
	"math"
	"os"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
	"goshare/internal/capture"
	"goshare/internal/codec"
	"goshare/internal/pipeline"
	"goshare/internal/rtc"
	"goshare/internal/ui"
)

// atomicBits 用原子量存一个 float64（位级转换），避免解码线程与 UI 线程数据竞争。
type atomicBits uint64

func (a *atomicBits) store(v float64) {
	atomic.StoreUint64((*uint64)(a), math.Float64bits(v))
}

func (a *atomicBits) load() float64 {
	return math.Float64frombits(atomic.LoadUint64((*uint64)(a)))
}

func main() {
	displayIdx := flag.Int("display", -1, "采集哪个显示器（-1 = 主屏）")
	preset := flag.String("preset", "最大", "质量档位：最大 / 流畅60 / 流畅30 / 急速")
	exitAfter := flag.Duration("exit", 0, "自动退出时长（如 20s）")
	logPath := flag.String("log", "", "日志文件")
	forcePrimary := flag.Bool("primary", false, "强制采集主屏（单/双屏下都会自摄入，仅用于对比）")
	diag := flag.Bool("diag", false, "输出逐帧像素诊断（非黑占比/亮度），用于定位画面全黑类故障")
	flag.Parse()

	if *logPath != "" {
		if f, err := os.Create(*logPath); err == nil {
			defer f.Close()
			log.SetOutput(f)
		}
	}

	if lvl := capture.EnsureDPIAware(); lvl != 2 {
		log.Printf("警告: DPI 感知级别 %d（期望 2）", lvl)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ds, err := capture.Displays(ctx)
	if err != nil || len(ds) == 0 {
		fmt.Fprintf(os.Stderr, "枚举显示器失败: %v\n", err)
		os.Exit(1)
	}
	// 采集目标的选择有个坑：Gio 窗口一旦被外部 SetWindowPos 移动，
	// 就不再产生帧事件（实测：移动后停在 10 帧，不移动则跑到 407 帧）。
	// 所以不去挪观看窗，改为「采集副屏、观看窗留在主屏」——
	// 同样能避开自摄入，而且完全不动窗口。
	sel := ds[0]
	for _, d := range ds {
		if d.Primary {
			sel = d
			break
		}
	}
	if len(ds) > 1 && !*forcePrimary {
		// 有副屏时默认采集副屏，观看窗（出现在主屏）就不会被采进去
		for _, d := range ds {
			if !d.Primary {
				sel = d
				break
			}
		}
	} else {
		log.Printf("警告: 只有一块显示器，观看窗会被自己采集进去（自摄入）")
	}
	if *displayIdx >= 0 && *displayIdx < len(ds) {
		sel = ds[*displayIdx]
	}
	log.Printf("采集: %s", sel.String())

	// ---------- 建立 P2P ----------
	//
	// ⚠️ 解码器的画布是持久复用的：解码线程写、渲染线程读同一块内存，
	// 会撕裂甚至让 Gio 后端崩溃。这里用「三张画布轮转 + 渲染完归还」解决：
	// 解码后拷进一张空闲画布再送出去，渲染用完还回池子，全程零共享写。
	frames := make(chan *image.NRGBA, 2)
	recycle := make(chan *image.NRGBA, 3)
	dec := codec.NewDecoder(0)
	var recvFrames, recvBytes uint64
	var lastLatency atomicBits

	// 像素诊断：全黑类故障必须能一眼看出是"没收到帧"还是"收到的帧是黑的"。
	// 三个观测点：源帧（采集）→ 编码条带 → 解码画布。
	var srcFrames uint64
	// lum 的第 2、3 个参数是 R、B 的下标：BGRA 用 (2,0)，NRGBA/RGBA 用 (0,2)。
	// 顺序搞反不会报错，只会让亮度统计轻微失真 —— 正是那种"自洽但错"的坑，所以显式分开。
	pixStat := func(pix []byte, w, h, ri, bi int) (nonBlack float64, meanLum float64) {
		step := 1
		if w*h > 400000 {
			step = 4
		}
		n, sum, nb := 0, 0, 0
		for j := 0; j < h; j += step {
			row := j * w * 4
			for i := 0; i < w; i += step {
				p := row + i*4
				lum := (77*int(pix[p+ri]) + 150*int(pix[p+1]) + 29*int(pix[p+bi])) >> 8
				sum += lum
				if lum > 8 {
					nb++
				}
				n++
			}
		}
		if n == 0 {
			return 0, 0
		}
		return float64(nb) / float64(n), float64(sum) / float64(n)
	}

	viewer, err := rtc.NewPeer(rtc.Config{
		OnFrame: func(f *codec.Frame) {
			img, err := dec.Decode(f)
			if err != nil {
				return
			}
			recvFrames++
			recvBytes += uint64(f.Bytes)
			lastLatency.store(dec.DecodeMs)
			if *diag && (recvFrames <= 5 || recvFrames%60 == 0) {
				nb, lum := pixStat(img.Pix, img.Bounds().Dx(), img.Bounds().Dy(), 0, 2)
				log.Printf("diag 解码 #%d seq=%d full=%v 条带=%d/%d %d字节 非黑=%.1f%% 亮度=%.1f",
					recvFrames, f.Seq, f.Full, len(f.Tiles), f.TotalTiles, f.Bytes, nb*100, lum)
			}

			var dst *image.NRGBA
			select {
			case dst = <-recycle:
				if dst.Bounds().Dx() != img.Bounds().Dx() || dst.Bounds().Dy() != img.Bounds().Dy() {
					dst = image.NewNRGBA(img.Bounds())
				}
			default:
				dst = image.NewNRGBA(img.Bounds())
			}
			copy(dst.Pix, img.Pix)
			select {
			case frames <- dst:
			default:
				// 渲染没跟上就丢这一帧：宁可偶尔掉帧，也不能阻塞传输回调
				select {
				case recycle <- dst:
				default:
				}
			}
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "观看端失败: %v\n", err)
		os.Exit(1)
	}
	defer viewer.Close()

	host, err := rtc.NewPeer(rtc.Config{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "分享端失败: %v\n", err)
		os.Exit(1)
	}
	defer host.Close()

	host.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			_ = viewer.AddICECandidate(c.ToJSON())
		}
	})
	viewer.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			_ = host.AddICECandidate(c.ToJSON())
		}
	})

	viewer.WaitChannels()
	if err := host.SetupMedia(); err != nil {
		fmt.Fprintf(os.Stderr, "创建通道失败: %v\n", err)
		os.Exit(1)
	}
	offer, _ := host.PC().CreateOffer(nil)
	_ = host.PC().SetLocalDescription(offer)
	_ = viewer.PC().SetRemoteDescription(*host.PC().LocalDescription())
	answer, _ := viewer.PC().CreateAnswer(nil)
	_ = viewer.PC().SetLocalDescription(answer)
	_ = host.PC().SetRemoteDescription(*viewer.PC().LocalDescription())

	if err := host.WaitReady(20 * time.Second); err != nil {
		fmt.Fprintf(os.Stderr, "通道未就绪: %v\n", err)
		os.Exit(1)
	}
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); {
		if viewer.Ready() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	log.Printf("P2P 就绪")

	// ---------- 启动分享管线 ----------
	p := pipeline.PresetByName(*preset)
	sh, err := pipeline.NewSharer(ctx, pipeline.Config{
		Display: sel,
		FPS:     30,
		Preset:  p,
		Cursor:  true,
		OnFrame: func(f *codec.Frame, src capture.Frame) {
			srcFrames++
			if *diag && (srcFrames <= 5 || srcFrames%60 == 0) {
				nb, lum := pixStat(src.Pix, src.W, src.H, 2, 0)
				log.Printf("diag 源帧 #%d %dx%d 非黑=%.1f%% 亮度=%.1f | 编码 full=%v 条带=%d/%d %d字节",
					srcFrames, src.W, src.H, nb*100, lum, f.Full, len(f.Tiles), f.TotalTiles, f.Bytes)
			}
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "启动采集失败: %v\n", err)
		os.Exit(1)
	}
	defer sh.Close()
	sh.AddPeer(host)
	go func() {
		if err := sh.Run(ctx); err != nil && ctx.Err() == nil {
			log.Printf("分享循环结束: %v", err)
		}
	}()

	// ---------- 观看窗 ----------
	started := time.Now()
	title := fmt.Sprintf("GoShare · 观看端（%s档）", p.Name)
	err = ui.RunViewer(ctx, ui.ViewerConfig{
		Title:     title,
		Frames:    frames,
		ExitAfter: *exitAfter,
		Status: func() string {
			st := sh.Stats()
			el := time.Since(started).Seconds()
			fps := float64(recvFrames) / el
			mbps := float64(recvBytes) * 8 / el / 1e6
			return fmt.Sprintf("后端 %s · 采集 %.1f fps · 接收 %.1f fps · %.0f Mbps · 编码 %.1f ms · 解码 %.1f ms · 采集处理 %.2f ms",
				st.Backend, st.FPS, fps, mbps, st.EncodeMs, lastLatency.load(),
				float64(st.MeanProc.Microseconds())/1000.0)
		},
		OnReady: func() {
			// 故意不动窗口：见上方说明，移动会让 Gio 停止出帧
			log.Printf("观看窗就绪（不做移动）")
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "观看窗失败: %v\n", err)
		os.Exit(1)
	}
	log.Printf("演示结束：共接收 %d 帧", recvFrames)
}
