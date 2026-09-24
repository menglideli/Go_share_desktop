// Command multiview 是无窗口的多观众压测工具：一次拉起 N 个观众接入同一个分享端，
// 各自独立统计，用来回答阶段 5 的几个问题：
//
//  1. 多人能不能都接进来（UDP 端口池够不够、授权码/人数上限有没有误伤）
//  2. 每个人拿到的画面是不是都完整 —— 尤其第 2、3 个观众是在分享端"已经稳态"
//     之后接入的，R32 的三条恢复路径在多人下还成不成立
//  3. 公不公平：弱网背压只影响自己，不该拖累别人（帧数极差应该很小）
//  4. 踢人/掉线的影响面：一个人断掉，其他人的帧率不该抖
//  5. 分享端负载：编码只做一次，N 个人只是多发 N 份，带宽线性增长但 fps 不该掉
//
// 为什么不用 goshare.exe -auto watch 开 N 个：那样会开 N 个 Gio 窗口，
// 窗口本身进了采集画面（自摄入），既污染像素统计也白占一堆 GPU。
//
// 用法：
//
//	go run ./spike/multiview -addr 127.0.0.1:9600 -code ABC123 -n 3 -dur 25s
//	go run ./spike/multiview -addr 127.0.0.1:9600 -code ABC123 -n 4 -drop 12:2
package main

import (
	"context"
	"flag"
	"fmt"
	"image"
	"image/png"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"goshare/internal/codec"
	"goshare/internal/rtc"
	"goshare/internal/signal"
)

// pliMinInterval 是观众请求全量帧的最小间隔（与 goshare 主程序一致）。
//
// 不限流会在"通道还没就绪"时每帧发一次，把可靠的 ctl 通道也堵住。
const pliMinInterval = 300 * time.Millisecond

// retryWait 是接入失败后的重试间隔，与 goshare 主程序 joinRetryWait 一致。
const retryWait = 4 * time.Second

// 分带数与判定阈值与 spike/pixstat 保持一致，方便两边对照。
const (
	bands      = 16
	blackLum   = 5
	sampleStep = 8
)

// vstat 是一个观众的独立统计。
type vstat struct {
	idx int

	mu sync.Mutex

	// 接入
	joined    bool
	joinErr   string
	connectMs float64
	joinAt    time.Time // 第一次尝试接入的时刻（用来算"用户总共等了多久"）
	// connectedAt 是真正接通的时刻。
	//
	// ⚠️ 帧率/首帧延迟必须用它当分母：重试成功的人 joinAt 可能早十几秒，
	// 拿它当分母会把 30 fps 摊薄成 3 fps，看上去像"补位进来的人被亏待了"。
	connectedAt time.Time

	// 首帧
	firstAt        time.Time
	firstFull      bool
	firstNonBlack  float64
	firstLum       float64
	firstBlackBnds int

	// 累计
	frames     uint64
	bytes      uint64
	fullFrames uint64
	decMs      float64
	fragLost   uint64
	lastSeq    uint64

	// 完整性恢复：第几帧首次做到"16 带全不黑"
	completeAtFrame uint64
	completeAfterMs float64

	// 掉线
	dropped   bool
	droppedAt time.Time

	nonBlack float64
	lum      float64
	blackB   int

	// PLI 自救（真实观众端的行为，压测必须带上，否则测的是"残缺观众"）
	needFull  atomic.Bool
	peerBox   atomic.Pointer[rtc.Peer]
	lastPLI   atomic.Int64
	pliSent   atomic.Uint64
	pliAtFr   atomic.Uint64 // 首次发 PLI 时是第几帧
	pliRecvFr atomic.Uint64 // 发过 PLI 后第几帧收到全量
}

// sendPLI 请分享端补一帧全量。带 300ms 限流。
func (v *vstat) sendPLI() {
	p := v.peerBox.Load()
	if p == nil {
		return
	}
	now := time.Now().UnixMilli()
	last := v.lastPLI.Load()
	if last != 0 && now-last < pliMinInterval.Milliseconds() {
		return
	}
	if !v.lastPLI.CompareAndSwap(last, now) {
		return
	}
	if err := p.SendControl(rtc.CtlPLI, nil); err != nil {
		return
	}
	v.pliSent.Add(1)
}

func (v *vstat) snap(now time.Time) (fps, mbps, decMs float64, frames uint64, nb float64, bb int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	el := now.Sub(v.connectedAt).Seconds()
	if el <= 0 || !v.joined {
		return 0, 0, 0, 0, 0, 0
	}
	fps = float64(v.frames) / el
	mbps = float64(v.bytes) * 8 / el / 1e6
	if v.frames > 0 {
		decMs = v.decMs / float64(v.frames)
	}
	return fps, mbps, decMs, v.frames, v.nonBlack, v.blackB
}

func main() {
	addr := flag.String("addr", "", "分享端地址 host:port（必填）")
	code := flag.String("code", "", "授权码（必填）")
	n := flag.Int("n", 3, "观众数")
	dur := flag.Duration("dur", 25*time.Second, "每个观众观看多久")
	stagger := flag.Duration("stagger", 1500*time.Millisecond, "观众之间错峰接入间隔（0 = 同时接入）")
	drop := flag.String("drop", "", "在第几秒踢掉第几个观众，如 12:2（1-based，0 = 不踢）")
	tick := flag.Duration("tick", 5*time.Second, "汇总打印间隔")
	noPLI = flag.Bool("nopli", false, "关闭观众侧 PLI 自救（用来单独验证分享端 burst 兜底是否成立）")
	retry = flag.Int("retry", 1, "接入失败重试次数（真实观众端是 3 次 × 4s）")
	hardDrop = flag.Bool("hard", false, "-drop 时用硬断开：关 Peer 但不发 Bye，模拟进程被强杀")
	dumpPath = flag.String("dump-frame", "", "调试：把第 N 帧落盘成 PNG（配合 -dump-at）")
	dumpAt = flag.Uint64("dump-at", 45, "-dump-frame 指定存第几帧")
	flag.Parse()

	if *addr == "" || *code == "" {
		fmt.Fprintln(os.Stderr, "用法: multiview -addr host:port -code CODE [-n 3] [-dur 25s] [-drop 12:2]")
		os.Exit(2)
	}

	dropSec, dropIdx := 0, 0
	if *drop != "" {
		parts := strings.Split(*drop, ":")
		if len(parts) != 2 {
			fmt.Fprintln(os.Stderr, "-drop 格式应为 秒:序号，如 12:2")
			os.Exit(2)
		}
		dropSec, _ = strconv.Atoi(parts[0])
		dropIdx, _ = strconv.Atoi(parts[1])
	}

	ctx, cancel := context.WithTimeout(context.Background(), *dur+40*time.Second)
	defer cancel()

	var (
		wg    sync.WaitGroup
		stats = make([]*vstat, *n)
	)
	t0 := time.Now()
	for i := 0; i < *n; i++ {
		stats[i] = &vstat{idx: i + 1}
	}
	for i := 0; i < *n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			runViewer(ctx, *addr, *code, i+1, *stagger, *dur, dropSec, dropIdx, stats[i])
		}(i)
	}

	// 汇总打印
	go func() {
		tk := time.NewTicker(*tick)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
			}
			now := time.Now()
			fmt.Printf("\n[%.0fs] 逐观众：\n", now.Sub(t0).Seconds())
			for _, v := range stats {
				fps, mbps, dec, fr, nb, bb := v.snap(now)
				tag := ""
				if v.dropped {
					tag = " (已断开)"
				}
				if v.joinErr != "" {
					fmt.Printf("  #%d 接入失败: %s\n", v.idx, v.joinErr)
					continue
				}
				if !v.joined {
					fmt.Printf("  #%d 接入中…\n", v.idx)
					continue
				}
				fmt.Printf("  #%d  %5.1f fps · %5.2f Mbps · 解码 %4.1f ms · 帧 %4d · 非黑 %5.1f%% · 黑带 %d%s\n",
					v.idx, fps, mbps, dec, fr, nb*100, bb, tag)
			}
		}
	}()

	wg.Wait()
	report(stats, t0)
}

// noPLI 由 -nopli 控制：关掉观众侧自救，用来分离"分享端 burst"与"观众 PLI"
// 两条恢复路径各自的贡献。
var (
	noPLI    *bool
	retry    *int
	hardDrop *bool
	dumpPath *string
	dumpAt   *uint64
)

// saveNRGBA 把一帧落盘。
//
// ⚠️ img 指向解码器的复用缓冲（下一帧就把它覆盖掉），所以必须在回调里同步编码完，
// 不能存指针留着以后再写 —— 那样写出来的是最后那一帧。
func saveNRGBA(path string, img *image.NRGBA) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}

func runViewer(ctx context.Context, addr, code string, idx int, stagger, dur time.Duration, dropSec, dropIdx int, v *vstat) {
	// 错峰：模拟真实场景里观众不是一个一个同时点的，
	// 也让"第 2、3 个观众在分享端稳态后接入"这个关键场景真的发生。
	if stagger > 0 && idx > 1 {
		select {
		case <-time.After(time.Duration(idx-1) * stagger):
		case <-ctx.Done():
			return
		}
	}

	v.mu.Lock()
	v.joinAt = time.Now()
	v.mu.Unlock()
	// 刚接入时画布是空的，第一帧必须是全量；否则一路发 PLI 直到拿到全量。
	if !*noPLI {
		v.needFull.Store(true)
	}

	// 每个观众一个解码器：解码器内部有复用缓冲，多人共用会互相踩。
	dec := codec.NewDecoder(0)
	var sess *signal.Session

	// 掉线/踢人：两种断开方式要分开测，因为分享端回收名额靠的是不同路径：
	//   -hard=false（默认）：走 Bye，服务端立刻 Detach，名额马上释放
	//   -hard=true：直接关 PeerConnection 且**不发 Bye**，模拟观众进程被强杀 /
	//     笔记本合盖 / 网线被拔。服务端收不到 Bye，只能靠 disconnected 超时兜底。
	//   后者才是真实故障，也是"幽灵观众白占名额"最容易漏掉的一条路。
	var dropOnce sync.Once
	dropSelf := func() {
		dropOnce.Do(func() {
			v.mu.Lock()
			v.dropped = true
			v.droppedAt = time.Now()
			v.mu.Unlock()
			if sess == nil {
				return
			}
			if *hardDrop {
				// 只关 PeerConnection，绝不发 Bye —— 发 Bye 就变成正常路径了，
				// 测不到"服务端只能靠 disconnected 超时兜底"这条真实故障路径。
				fmt.Printf("\n  >> 观众 #%d 硬断开（关 Peer 不发 Bye，模拟进程被强杀）\n", idx)
				_ = sess.Peer.Close()
			} else {
				fmt.Printf("\n  >> 观众 #%d 主动断开（模拟掉线/被踢）\n", idx)
				_ = sess.ByeClose()
			}
		})
	}

	mkDial := func() (*signal.Session, error) {
		return signal.Dial(ctx, signal.DialConfig{
			Addr: addr,
			Code: code,
		Name: fmt.Sprintf("压测-%d", idx),
		Peer: rtc.Config{
			UDPPortMin: 50000,
			UDPPortMax: 50100,
			OnFrame: func(f *codec.Frame) {
				img, err := dec.Decode(f)
				if err != nil {
					return
				}
				w, h := img.Bounds().Dx(), img.Bounds().Dy()
				nb, lum, bb := bandStat(img.Pix, w, h)
				now := time.Now()

				v.mu.Lock()
				if v.frames == 0 {
					v.firstAt = now
					v.firstFull = f.Full
					v.firstNonBlack, v.firstLum, v.firstBlackBnds = nb, lum, bb
				}
				v.frames++
				v.bytes += uint64(f.Bytes)
				if f.Full {
					v.fullFrames++
				}
				v.decMs += dec.DecodeMs
				v.lastSeq = f.Seq
				v.nonBlack, v.lum, v.blackB = nb, lum, bb
				if v.completeAtFrame == 0 && bb == 0 && v.frames > 0 {
					v.completeAtFrame = v.frames
					v.completeAfterMs = float64(now.Sub(v.connectedAt).Milliseconds())
				}
				fn := v.frames
				// 画面完整性（R32）：只有全量帧能填满画布。没拿到就主动要，
				// 不能指望桌面碰巧大幅变化去触发（实测等过 12 秒）。
				if f.Full {
					v.needFull.Store(false)
					if v.pliAtFr.Load() > 0 && v.pliRecvFr.Load() == 0 {
						v.pliRecvFr.Store(fn)
					}
				}
				wantPLI := false
				if !f.Full && v.needFull.Load() {
					wantPLI = true
					if v.pliAtFr.Load() == 0 {
						v.pliAtFr.Store(fn)
					}
				}
				v.mu.Unlock()

				if wantPLI {
					v.sendPLI()
				}

				if fn <= 3 || f.Full {
					fmt.Printf("  #%d 帧#%d seq=%d 全量=%v 条带 %d/%d %dx%d 非黑 %.1f%% 黑带 %d\n",
						idx, fn, f.Seq, f.Full, len(f.Tiles), f.TotalTiles, w, h, nb*100, bb)
				}
				if *dumpPath != "" && idx == 1 && fn == *dumpAt {
					if err := saveNRGBA(*dumpPath, img); err != nil {
						fmt.Printf("  #%d 落盘失败: %v\n", idx, err)
					} else {
						fmt.Printf("  #%d 第 %d 帧（%dx%d）已落盘到 %s\n", idx, fn, w, h, *dumpPath)
					}
				}
			},
		},
		})
	}

	// 重试：真实观众端是 3 次 × 4s。默认 1 次（不重试），
	// 但验证"人数满了之后有人退出、名额是否真的释放"时必须重试才有意义。
	var s *signal.Session
	var err error
	for attempt := 1; attempt <= *retry; attempt++ {
		s, err = mkDial()
		if err == nil {
			break
		}
		fmt.Printf("  #%d 第 %d/%d 次接入失败: %v\n", idx, attempt, *retry, err)
		if attempt < *retry {
			select {
			case <-time.After(retryWait):
			case <-ctx.Done():
				v.mu.Lock()
				v.joinErr = err.Error()
				v.mu.Unlock()
				return
			}
		}
	}
	if err != nil {
		v.mu.Lock()
		v.joinErr = err.Error()
		v.mu.Unlock()
		return
	}
	sess = s
	v.mu.Lock()
	v.joined = true
	v.connectMs = s.ConnectMs
	v.connectedAt = time.Now()
	v.mu.Unlock()
	// Dial 内部才创建 peer，回调里拿不到，只能先装箱。
	v.peerBox.Store(s.Peer)
	fmt.Printf("  #%d 接入成功 %s（耗时 %.0f ms）\n", idx, s.Info.Name, s.ConnectMs)

	// 到点退出 / 到点被踢
	end := time.After(dur)
	var dropTimer <-chan time.Time
	if dropSec > 0 && dropIdx == idx {
		dropTimer = time.After(time.Duration(dropSec) * time.Second)
	}
	select {
	case <-ctx.Done():
	case <-dropTimer:
		dropSelf()
		return
	case <-end:
	}
	_ = s.ByeClose()
}

func report(stats []*vstat, t0 time.Time) {
	fmt.Printf("\n===== 多观众压测汇总（%d 人）=====\n", len(stats))
	fmt.Printf("%-4s %-8s %-8s %-8s %-8s %-9s %-9s %-7s %-8s %-6s %-6s\n",
		"#", "握手ms", "等待ms", "首帧ms", "首帧全量", "首帧非黑", "首帧黑带", "帧数", "全量帧", "fps", "Mbps")
	var ok, minF, maxF uint64
	minF = ^uint64(0)
	fpsList := []float64{}
	for _, v := range stats {
		v.mu.Lock()
		if v.joinErr != "" {
			fmt.Printf("%-4d 接入失败: %s\n", v.idx, v.joinErr)
			v.mu.Unlock()
			continue
		}
		el := time.Since(v.connectedAt).Seconds()
		if v.dropped {
			el = v.droppedAt.Sub(v.connectedAt).Seconds()
		}
		fps := 0.0
		mbps := 0.0
		if el > 0 {
			fps = float64(v.frames) / el
			mbps = float64(v.bytes) * 8 / el / 1e6
		}
		firstMs := 0.0
		if !v.firstAt.IsZero() {
			firstMs = v.firstAt.Sub(v.connectedAt).Seconds() * 1000
		}
		// 重试过的人"总共等了多久"另算：从第一次尝试到真正接通。
		waitMs := 0.0
		if !v.connectedAt.IsZero() {
			waitMs = v.connectedAt.Sub(v.joinAt).Seconds() * 1000
		}
		full := "否"
		if v.firstFull {
			full = "是"
		}
		fmt.Printf("%-4d %-8.0f %-8.0f %-8.0f %-10s %-11.1f %-11d %-7d %-8d %-6.1f %-6.2f\n",
			v.idx, v.connectMs, waitMs, firstMs, full, v.firstNonBlack*100, v.firstBlackBnds,
			v.frames, v.fullFrames, fps, mbps)
		// ⚠️ 公平性只能拿"全程在线"的观众比：中途被踢掉的人帧数天然少一截，
		// 把它算进极差会得到 50% 这种假数字，看上去像严重不公平。
		if v.frames > 0 && !v.dropped {
			ok++
			if v.frames < minF {
				minF = v.frames
			}
			if v.frames > maxF {
				maxF = v.frames
			}
			fpsList = append(fpsList, fps)
		}
		v.mu.Unlock()
	}
	if ok == 0 {
		fmt.Println("没有观众收到帧")
		return
	}
	fmt.Printf("\n完整性恢复（首次 16 带全不黑）：\n")
	for _, v := range stats {
		v.mu.Lock()
		if !v.joined {
			// 连都没连上，谈"画面完不完整"没有意义，别输出误导行。
			v.mu.Unlock()
			continue
		}
		if v.completeAtFrame > 0 {
			extra := ""
			if af, rf := v.pliAtFr.Load(), v.pliRecvFr.Load(); af > 0 {
				if rf > 0 {
					extra = fmt.Sprintf("（第 %d 帧发 PLI，第 %d 帧拿到全量，间隔 %d 帧 · 共发 %d 次）",
						af, rf, rf-af, v.pliSent.Load())
				} else {
					extra = fmt.Sprintf("（第 %d 帧起发 PLI 共 %d 次，未收到全量）", af, v.pliSent.Load())
				}
			}
			fmt.Printf("  #%d 第 %d 帧 / %.0f ms 后画面完整%s\n",
				v.idx, v.completeAtFrame, v.completeAfterMs, extra)
		} else {
			fmt.Printf("  #%d 从未达到完整画面（黑带 %d）\n", v.idx, v.blackB)
		}
		v.mu.Unlock()
	}
	sorted := append([]float64(nil), fpsList...)
	sort.Float64s(sorted)
	spread := float64(maxF-minF) / float64(maxF) * 100
	fmt.Printf("\n公平性：收到帧的观众 %d/%d · 帧数 %d..%d（极差 %.1f%%）· fps %.1f..%.1f\n",
		ok, len(stats), minF, maxF, spread, sorted[0], sorted[len(sorted)-1])
	fmt.Printf("总时长 %.0f s\n", time.Since(t0).Seconds())
}

// bandStat 算整图非黑/亮度，外加 16 分带里有多少带是黑的。
//
// 只靠整图均值会漏掉"半张画"：一半区域黑、一半区域亮，均值是个中间数，
// 看起来像"画面偏暗"，实际是"有条带从来没被填充"（R32 的原始症状）。
func bandStat(pix []byte, w, h int) (nonBlack, meanLum float64, blackBands int) {
	bandLum := make([]float64, bands)
	bandN := make([]int, bands)
	sum, n, nb := 0, 0, 0
	for y := 0; y < h; y += sampleStep {
		bi := y * bands / h
		row := y * w * 4
		for x := 0; x < w; x += sampleStep {
			i := row + x*4
			lum := (77*int(pix[i]) + 150*int(pix[i+1]) + 29*int(pix[i+2])) >> 8
			sum += lum
			n++
			if lum > 8 {
				nb++
			}
			bandLum[bi] += float64(lum)
			bandN[bi]++
		}
	}
	if n == 0 {
		return 0, 0, bands
	}
	for i := 0; i < bands; i++ {
		if bandN[i] == 0 {
			continue
		}
		if bandLum[i]/float64(bandN[i]) < blackLum {
			blackBands++
		}
	}
	return float64(nb) / float64(n), float64(sum) / float64(n), blackBands
}
