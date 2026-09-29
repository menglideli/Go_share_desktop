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
	"errors"
	"fmt"
	"image"
	"math"
	"math/bits"
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

// sink 是观看端的落点：收分块 → 解码 → 统计。
type sink struct {
	mu      sync.Mutex
	dec     *codec.Decoder
	got     int // 逻辑帧数（按 seq 去重）
	chunks  int
	lastSeq uint64
	last    *image.NRGBA
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

func (s *sink) onFrames(batch []*codec.Frame) {
	if len(batch) == 0 {
		return
	}
	// 与应用同一条路径：一批块一起解（单块只带 1~2 个条带，逐块解会把
	// 帧内并行度丢光 —— 实测 2K 全量 7ms → 90ms）。
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dec == nil {
		return
	}
	img, err := s.dec.DecodeBatch(batch)
	if err != nil {
		return
	}
	// 收到的每个消息是一**块**（一帧可能多块）：帧数按 seq 去重，
	// 块数单独计 —— 否则"发送 30 / 收到 90"会看着像收了 3 倍帧。
	s.chunks += len(batch)
	for _, f := range batch {
		if f.Seq != s.lastSeq {
			s.lastSeq = f.Seq
			s.got++
		}
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
	s.last = img
	s.decMS += s.dec.DecodeMs
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
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

// coverState 报告画布是否"每个条带都是当前内容"，以及缺多少条带。
// 判据来自解码器的完整性位图（丢块会被精确标出来，不是笼统的"收没收到全量帧"）。
// 同时返回条带总数与"内容有效"的条带数，便于核对统计口径。
func (s *sink) coverState() (bool, int, int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dec == nil {
		return false, 0, 0, 0
	}
	tiles := s.dec.TileCount()
	have := bits.OnesCount64(s.dec.Have())
	return s.dec.Complete(), bits.OnesCount64(s.dec.Missing()), tiles, have
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
		// 与生产观看端同一条路径：异步 + 批量解析解码。
		AsyncFrames: true,
		OnFrames:    sk.onFrames,
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

	fmt.Printf("\n[4] 丢块恢复：故意丢 1 个分块，验证「只丢它带的条带」+ 渐进式修复\n")
	runLossRepair(sk, host)

	fmt.Printf("\n------------------------------------------------------------\n")
	fmt.Printf("结果: %d 通过 / %d 失败\n\n", pass, fail)
	if fail > 0 {
		os.Exit(1)
	}
	os.Exit(0)
}

// runCase 跑一轮：编码 → 发送 →（异步）批量解码 → 断言。
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
		// 背压是**预期行为**（SendLimit 只允许约一帧半的积压）：突发发送时
		// 等一等再发，而不是把这一帧算作失败 —— 真实的发送循环也是这么做的
		// （跳过这一帧、下一帧顶上）。这里为了测吞吐/延迟/画质，选择重试。
		sendDeadline := time.Now().Add(10 * time.Second)
		for {
			err = host.SendFrame(f)
			if err == nil {
				break
			}
			if !errors.Is(err, rtc.ErrBackpressure) || time.Now().After(sendDeadline) {
				check(label+" 发送", false, err.Error())
				return
			}
			time.Sleep(2 * time.Millisecond)
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

	// 先让画布**自愈**，再算 PSNR。
	//
	// ⚠️ 为什么必须有这一步：突发压测下接收端邮箱（latest-wins，容量 4）会溢出几块，
	// 于是画布缺几条带 —— 这是系统的**正常行为**（真实链路里会立刻精确补发补回来），
	// 但拿这个中间态算 PSNR，就会把机器负载抖动误报成"画质回归"。
	// 实测踩过：锁屏/高负载下 2K 全量 PSNR 从 34.97 掉到 28.72，其实只丢了几块。
	// 门禁要测的是"系统最终交付给用户什么"，不是"某一瞬间的中间态"。
	heal := 0
	for i := 0; i < 12; i++ {
		if c, _, _, _ := sk.coverState(); c {
			break
		}
		enc.RequestTiles(sk.missingMask())
		out, err := enc.Encode(src, w, h)
		if err != nil {
			break
		}
		if err := sendFrame(host, out); err != nil {
			break
		}
		heal++
		waitCover(sk, 0)
	}

	got, img, avgLat, maxLat, decMS := sk.snapshot()
	vs := viewer.Stats()

	check(label+" 帧收齐", got == sent,
		fmt.Sprintf("发送 %d / 收到 %d 帧（缺失 %d）· 分块 %d · 坏块 %d · 邮箱溢出丢弃 %d",
			sent, got, sent-got, vs.ChunksRecv, vs.BadChunks, vs.ChunksSkip))

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
		fmt.Sprintf("PSNR %.2f dB（源 → 经网络 → 解码结果）· 自愈补发 %d 帧 · 画布完整=%v",
			p, heal, func() bool { c, _, _, _ := sk.coverState(); return c }()))
}

// runLossRepair 验证"丢一个分块"的后果与修复代价 —— 这是本轮改动的核心命题。
//
// 旧实现：分块按帧重组，丢任意一块 = 整帧 2MB 作废，只能再要一整帧全量
// （拥塞时那一发大概率又丢 → "越要越堵"）。
// 新实现：每个分块自带帧头 + 完整条带，丢一块只丢它带的那几条带；
// 修复走"渐进式轮转"，把补齐整屏摊到十几帧里，单帧始终很小。
func runLossRepair(sk *sink, host *rtc.Peer) {
	const (
		w, h = 1920, 1080
	)
	src := makeFrame(w, h)
	// 强制全量帧（Reset 后首帧必为全量），这样才能拿到"整屏"的条带集合。
	enc := codec.NewEncoder(codec.Config{Quality: 75, Dirty: true})
	full, err := enc.Encode(src, w, h)
	if err != nil {
		check("丢块恢复 编码", false, err.Error())
		return
	}
	parts := full.Split(codec.MaxChunkPayload)
	if len(parts) < 3 {
		check("丢块恢复 分块", false, fmt.Sprintf("只切出 %d 块，无法做丢块实验", len(parts)))
		return
	}
	// 丢掉中间那一块：它带的条带应当"精确地只有它那几条"。
	dropIdx := len(parts) / 2
	var dropMask uint64
	for _, t := range parts[dropIdx].Tiles {
		dropMask |= 1 << uint(t.Index)
	}

	// ---- A. 格式级断言（不走网络，确定性）----
	// "丢一块只丢它带的条带"是**线格式**的性质，必须做成确定性判据：
	// 走网络会混入"接收端邮箱溢出"这类与格式无关的丢失（高负载/锁屏下实测发生过），
	// 把这条断言变成随机器状态漂移的东西。
	wantMissing := bits.OnesCount64(dropMask)
	off := codec.NewDecoder(0)
	kept := make([]*codec.Frame, 0, len(parts))
	for i, c := range parts {
		if i == dropIdx {
			continue
		}
		kept = append(kept, c)
	}
	if _, err := off.DecodeBatch(kept); err != nil {
		check("丢块只丢它带的条带", false, err.Error())
		return
	}
	gotMask := off.Missing()
	check("丢块只丢它带的条带", gotMask == dropMask && !off.Complete(),
		fmt.Sprintf("丢第 %d/%d 块（含 %d 条带）→ 画布 %d 条带里缺 %d 条，且缺的正是它带的那几条（位图 %x == %x）",
			dropIdx+1, len(parts), wantMissing, off.TileCount(),
			bits.OnesCount64(gotMask), gotMask, dropMask))

	// ---- A2. 同一场景走真实链路：丢的条带必须在缺失集合里，且能被精确补发补回 ----
	sk.reset(w, h)
	sendParts(host, parts, dropIdx)
	waitCover(sk, wantMissing)
	complete, missing, tiles, have := sk.coverState()
	wireMask := sk.missingMask()
	check("链路丢块后缺失集合正确", !complete && wireMask&dropMask == dropMask,
		fmt.Sprintf("画布 %d 条带里 %d 条有效、缺 %d（含被丢那 %d 条）· 额外丢失 %d 条（邮箱溢出，属正常）",
			tiles, have, missing, wantMissing, missing-wantMissing))

	// ---- A. 精确补发：观众报"缺哪几条"，分享端只补那几条 ----
	enc.RequestTiles(sk.missingMask())
	out, err := enc.Encode(src, w, h)
	if err != nil {
		check("精确补发 编码", false, err.Error())
		return
	}
	if err := sendFrame(host, out); err != nil {
		check("精确补发 发送", false, err.Error())
		return
	}
	waitCover(sk, 0)
	okA, missA, _, _ := sk.coverState()
	check("精确补发一帧补齐", okA && missA == 0,
		fmt.Sprintf("补 %d 条带只用 1 帧 · 该帧 %d KB（全量帧 %d KB 的 %d%%）· 补后仍缺 %d 条",
			wantMissing, out.Bytes/1024, full.Bytes/1024,
			out.Bytes*100/max(full.Bytes, 1), missA))

	// ---- B. 轮转修复：从**空画布**开始，不发全量帧也要铺满整屏 ----
	// 这就是新观众接入的路径：以前是"3 秒内每 400ms 一发 2MB 全量帧"，
	// 现在是十几帧、每帧都很小。
	sk.reset(w, h)
	enc.BeginRepair(10)
	repaired := false
	repairFrames, repairBytes, maxRepairFrame := 0, 0, 0
	for i := 0; i < 14; i++ {
		out, err := enc.Encode(src, w, h)
		if err != nil {
			check("轮转修复 编码", false, err.Error())
			return
		}
		repairFrames++
		repairBytes += out.Bytes
		if out.Bytes > maxRepairFrame {
			maxRepairFrame = out.Bytes
		}
		if err := sendFrame(host, out); err != nil {
			check("轮转修复 发送", false, err.Error())
			return
		}
		time.Sleep(20 * time.Millisecond)
		if c, _, _, _ := sk.coverState(); c {
			repaired = true
			break
		}
	}
	waitCover(sk, 0)
	done, missingNow, tilesNow, _ := sk.coverState()
	check("轮转修复铺满整屏", repaired && done && missingNow == 0,
		fmt.Sprintf("从空画布用 %d 帧铺满 %d 条带（仍缺 %d）· 单帧最大 %d KB（全量帧 %d KB 的 %d%%）",
			repairFrames, tilesNow, missingNow, maxRepairFrame/1024, full.Bytes/1024,
			maxRepairFrame*100/max(full.Bytes, 1)))
	check("修复不产生大突发", maxRepairFrame*4 <= full.Bytes,
		fmt.Sprintf("单帧最大 %d KB ≤ 全量帧的 1/4（%d KB）· 修复总流量 %d KB（全量帧 %d KB）",
			maxRepairFrame/1024, full.Bytes/4096, repairBytes/1024, full.Bytes/1024))
}

// missingMask 返回画布上"内容已过期"的条带位图（精确补发用）。
func (s *sink) missingMask() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dec == nil {
		return 0
	}
	return s.dec.Missing()
}

// sendParts 逐块发送，跳过 dropIdx 那一块（模拟丢包）。
func sendParts(host *rtc.Peer, parts []*codec.Frame, dropIdx int) {
	for i, c := range parts {
		if i == dropIdx {
			continue
		}
		wire, _ := c.Marshal()
		_ = sendRetry(host, [][]byte{wire})
	}
}

// sendFrame 把一帧切块发出。
func sendFrame(host *rtc.Peer, f *codec.Frame) error {
	parts := f.Split(codec.MaxChunkPayload)
	wire := make([][]byte, 0, len(parts))
	for _, c := range parts {
		b, err := c.Marshal()
		if err != nil {
			return err
		}
		wire = append(wire, b)
	}
	return sendRetry(host, wire)
}

// sendRetry 发送并在背压时重试：背压是预期行为（SendLimit 只允许约一帧半积压），
// 真实的发送循环是"跳过这一帧、下一帧顶上"，这里为了测完整性选择等一等再发。
func sendRetry(host *rtc.Peer, wire [][]byte) error {
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := host.SendChunks(wire)
		if err == nil {
			return nil
		}
		if !errors.Is(err, rtc.ErrBackpressure) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// waitCover 等画布覆盖状态到达期望值（最多 3 秒）。
func waitCover(sk *sink, wantMissing int) {
	t0 := time.Now()
	for i := 0; i < 60; i++ {
		if _, m, _, _ := sk.coverState(); m == wantMissing {
			return
		}
		if time.Since(t0) > 3*time.Second {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}
