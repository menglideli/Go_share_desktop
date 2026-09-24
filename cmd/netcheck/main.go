// netcheck — 阶段 2 · 端到端链路验证
//
// 真实跑通：BGRA → MJPEG 条带编码 → pion DataChannel 分片 → 重组 → 解码 → NRGBA。
// 两个 PeerConnection 经真实 UDP socket + ICE + DTLS 建连（同进程，走本机回环）。
//
// 重点验证（都是之前没测过的）：
//  1. 巨帧重组：2K 全量单帧约 2MB，切成 30+ 分片后必须能完整拼回
//  2. 端到端延迟：编码完成时刻 → 接收端解码完成时刻
//  3. 跨网络后解码画面仍与源一致（PSNR）
//  4. 增量更新在观众端累积后不漂移
package main

import (
	"fmt"
	"image"
	"math"
	"os"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"goshare/internal/codec"
	"goshare/internal/rtc"
)

var pass, fail int

func check(name string, ok bool, detail string) {
	if ok {
		pass++
		fmt.Printf("  ✅ %-26s %s\n", name, detail)
	} else {
		fail++
		fmt.Printf("  ❌ %-26s %s\n", name, detail)
	}
}

// sink 是观看端的落点：收帧 → 解码 → 统计。
type sink struct {
	mu     sync.Mutex
	dec    *codec.Decoder
	got    int
	last   *image.NRGBA
	// sentAt 记录每帧的发送时刻。延迟用它算，而不是帧里的 TS：
	// 线格式里 TS 只有 32 位（Unix 毫秒被截断），直接相减会得到天文数字。
	// 跨机器时还需要时钟同步，本机同进程则完全可信。
	sentAt map[uint64]time.Time
	latSum float64
	latN   int
	latMax float64
	decMS  float64
	pli    int
}

func (s *sink) reset(w, h int) {
	s.mu.Lock()
	s.dec = codec.NewDecoder(0)
	s.got = 0
	s.last = nil
	s.sentAt = map[uint64]time.Time{}
	s.latSum, s.latN, s.latMax, s.decMS = 0, 0, 0, 0
	s.mu.Unlock()
}

func (s *sink) markSent(seq uint64) {
	s.mu.Lock()
	s.sentAt[seq] = time.Now()
	s.mu.Unlock()
}

func (s *sink) onFrame(f *codec.Frame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dec == nil {
		return
	}
	// ⚠️ 解码必须在锁内：解码器写的是同一块持久画布，
	// 而 pion 的消息回调可能并发进入 —— 锁外解码会让两块条带互相撕裂，
	// 表现就是 PSNR 从 34dB 掉到 19dB，而且不是每次都复现。
	img, err := s.dec.Decode(f)
	if err != nil {
		return
	}
	s.got++
	s.last = img
	s.decMS += s.dec.DecodeMs
	if t, ok := s.sentAt[f.Seq]; ok {
		lat := float64(time.Since(t).Microseconds()) / 1000.0
		s.latSum += lat
		s.latN++
		if lat > s.latMax {
			s.latMax = lat
		}
		delete(s.sentAt, f.Seq)
	}
}

func (s *sink) snapshot() (got int, img *image.NRGBA, avgLat, maxLat, decMS float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	avg, mx := 0.0, s.latMax
	if s.latN > 0 {
		avg = s.latSum / float64(s.latN)
	}
	return s.got, s.last, avg, mx, s.decMS
}

func makeFrame(w, h int) []byte {
	pix := make([]byte, w*h*4)
	for j := 0; j < h; j++ {
		textRow := (j % 24) < 14
		for i := 0; i < w; i++ {
			var r, g, b uint8 = 245, 245, 245
			switch {
			case textRow && (i%17) < 9:
				r, g, b = 30, 30, 30
			case j > h-140 && i < 520:
				r, g, b = 200, 120, 90
			case j < 70:
				r, g, b = 225, 225, 230
			}
			p := (j*w + i) * 4
			pix[p], pix[p+1], pix[p+2], pix[p+3] = b, g, r, 255
		}
	}
	return pix
}

// mutate 模拟"桌面局部发生变化"。
//
// ⚠️ 别用逐像素随机噪声来模拟变化：JPEG 对纯噪声的 PSNR 会崩到 20 dB 以下，
// 看起来像编码坏了，其实是测试内容不真实 —— 真实桌面变化是文本、窗口、
// 色块这类结构化内容。曾经踩过这个坑，误判成质量缺陷。
func mutate(pix []byte, w, h, x0, y0, x1, y1, seed int) {
	base := uint8(40 + (seed*13)%180)
	for j := y0; j < y1 && j < h; j++ {
		for i := x0; i < x1 && i < w; i++ {
			v := base
			if (j%12) < 6 && (i%9) < 5 {
				v = uint8(255 - int(base))
			}
			p := (j*w + i) * 4
			pix[p], pix[p+1], pix[p+2] = v, v, v
		}
	}
}

func psnr(src []byte, dst *image.NRGBA, w, h int) float64 {
	if dst == nil {
		return 0
	}
	sum, n := 0.0, 0
	for j := 0; j < h; j++ {
		for i := 0; i < w; i++ {
			p := (j*w + i) * 4
			q := j*dst.Stride + i*4
			dr := int(src[p+2]) - int(dst.Pix[q])
			dg := int(src[p+1]) - int(dst.Pix[q+1])
			db := int(src[p]) - int(dst.Pix[q+2])
			sum += float64(dr*dr + dg*dg + db*db)
			n++
		}
	}
	if sum == 0 {
		return math.Inf(1)
	}
	return 10 * math.Log10(255*255/(sum/float64(n*3)))
}

func main() {
	go func() {
		time.Sleep(240 * time.Second)
		fmt.Println("!!! 看门狗超时（240s），强制退出")
		os.Exit(2)
	}()

	fmt.Printf("\n=== GoShare 端到端链路自检 · 阶段 2 ===\n")
	fmt.Printf("pion/webrtc · 同进程双 Peer · 真实 UDP 回环 + ICE + DTLS\n\n")

	sk := &sink{}
	t0 := time.Now()

	viewer, err := rtc.NewPeer(rtc.Config{
		OnFrame: sk.onFrame,
		OnControl: func(t rtc.CtlType, payload []byte) {
			if t == rtc.CtlPLI {
				sk.mu.Lock()
				sk.pli++
				sk.mu.Unlock()
			}
		},
	})
	if err != nil {
		fmt.Printf("观看端创建失败: %v\n", err)
		os.Exit(1)
	}
	defer viewer.Close()
	host, err := rtc.NewPeer(rtc.Config{})
	if err != nil {
		fmt.Printf("分享端创建失败: %v\n", err)
		os.Exit(1)
	}
	defer host.Close()

	host.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		_ = viewer.AddICECandidate(c.ToJSON())
	})
	viewer.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		_ = host.AddICECandidate(c.ToJSON())
	})

	viewer.WaitChannels()
	if err := host.SetupMedia(); err != nil {
		fmt.Printf("创建数据通道失败: %v\n", err)
		os.Exit(1)
	}

	offer, err := host.PC().CreateOffer(nil)
	if err != nil {
		fmt.Printf("CreateOffer 失败: %v\n", err)
		os.Exit(1)
	}
	_ = host.PC().SetLocalDescription(offer)
	_ = viewer.PC().SetRemoteDescription(*host.PC().LocalDescription())
	answer, err := viewer.PC().CreateAnswer(nil)
	if err != nil {
		fmt.Printf("CreateAnswer 失败: %v\n", err)
		os.Exit(1)
	}
	_ = viewer.PC().SetLocalDescription(answer)
	_ = host.PC().SetRemoteDescription(*viewer.PC().LocalDescription())

	if err := host.WaitReady(20 * time.Second); err != nil {
		check("媒体通道就绪", false, err.Error())
		os.Exit(1)
	}
	vdl := time.Now().Add(20 * time.Second)
	for time.Now().Before(vdl) {
		if viewer.Ready() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	connMs := float64(time.Since(t0).Milliseconds())
	check("ICE+DTLS 建连", connMs < 5000 && viewer.Ready(),
		fmt.Sprintf("%.0f ms · 双向通道就绪", connMs))

	fmt.Printf("\n[1] 1080p · 30 帧 · 全量（每帧约 800KB → 14 分片）\n")
	runCase(sk, host, viewer, 1920, 1080, 30, false, 33*time.Millisecond)

	fmt.Printf("\n[2] 2K · 20 帧 · 全量（单帧约 2MB → 35 分片，验证巨帧重组）\n")
	runCase(sk, host, viewer, 2880, 1800, 20, false, 33*time.Millisecond)

	fmt.Printf("\n[3] 1080p · 40 帧 · 增量（观众端累积，验证不漂移）\n")
	runCase(sk, host, viewer, 1920, 1080, 40, true, 33*time.Millisecond)

	fmt.Printf("\n------------------------------------------------------------\n")
	fmt.Printf("结果: %d 通过 / %d 失败\n\n", pass, fail)
	if fail > 0 {
		os.Exit(1)
	}
	os.Exit(0)
}

// runCase 跑一轮：编码 → 发送 →（异步）重组解码 → 断言。
func runCase(sk *sink, host, viewer *rtc.Peer, w, h, frames int, dirty bool, pace time.Duration) {
	label := fmt.Sprintf("%dx%d", w, h)
	if dirty {
		label += " 增量"
	} else {
		label += " 全量"
	}
	sk.reset(w, h)

	src := makeFrame(w, h)
	enc := codec.NewEncoder(codec.Config{Quality: 75, Dirty: dirty})

	sentBytes, maxFrame := 0, 0
	encodeMs := 0.0
	t0 := time.Now()
	sent := 0
	for k := 0; k < frames; k++ {
		mutate(src, w, h, 100+k*30, 200+k*20, 700+k*30, 500+k*20, k)
		f, err := enc.Encode(src, w, h)
		if err != nil {
			check(label+" 编码", false, err.Error())
			return
		}
		encodeMs += f.EncodeMs
		sentBytes += f.Bytes
		if f.Bytes > maxFrame {
			maxFrame = f.Bytes
		}
		sk.markSent(f.Seq)
		if err := host.SendFrame(f); err != nil {
			check(label+" 发送", false, err.Error())
			return
		}
		sent++
		if pace > 0 {
			time.Sleep(pace)
		}
	}
	elapsed := time.Since(t0)

	// 排空：等到收齐，或超时。
	// 用"收齐即停"而不是"连续几次没变化就停" —— 后者会在分片还在 SCTP 缓冲里
	// 时就误判为结束，把正常的传输延迟报成丢包。
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		g, _, _, _, _ := sk.snapshot()
		if g >= sent {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond) // 余量：让最后一帧的解码也落定

	got, img, avgLat, maxLat, decMS := sk.snapshot()
	vs := viewer.Stats()

	check(label+" 帧收齐", got == sent,
		fmt.Sprintf("发送 %d / 重组解码 %d（缺失 %d）· 分片残缺 %d",
			sent, got, sent-got, vs.FragLost))

	avgFrame := 0
	if sent > 0 {
		avgFrame = sentBytes / sent
	}
	mbps := float64(sentBytes) * 8 / elapsed.Seconds() / 1e6
	check(label+" 吞吐", true,
		fmt.Sprintf("平均帧 %.0f KB（最大 %.0f KB / %d 分片）· %.0f Mbps",
			float64(avgFrame)/1024, float64(maxFrame)/1024,
			(maxFrame+60*1024-1)/(60*1024), mbps))

	check(label+" 编码耗时", encodeMs/float64(sent) < 33.3,
		fmt.Sprintf("%.1f ms/帧（%.0f fps）· 解码 %0.1f ms/帧",
			encodeMs/float64(sent), 1000/(encodeMs/float64(sent)),
			decMS/float64(max(got, 1))))

	// 端到端延迟：编码完成 → 解码完成。加回编码耗时即"采集后到显示前"。
	if got > 0 {
		e2e := avgLat + encodeMs/float64(sent)
		check(label+" 端到端延迟", e2e < 100,
			fmt.Sprintf("平均 %.1f ms（传输+重组+解码 %.1f，编码 %.1f）· 峰值 %.0f ms",
				e2e, avgLat, encodeMs/float64(sent), maxLat))
	}

	p := psnr(src, img, w, h)
	check(label+" 画质", p >= 30,
		fmt.Sprintf("PSNR %.2f dB（源 → 经网络 → 解码结果）", p))
}
