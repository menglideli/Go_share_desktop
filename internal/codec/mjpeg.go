// Package codec 实现屏幕共享用的条带并行 Motion JPEG 编解码。
//
// 为什么是 MJPEG 而不是 H.264（见 VERIFY.md 完整实测）：
//   - go264 的 MF 硬件编码在本机 Encode() 永久挂起，CPU 路径 1080p 仅 12.9fps；
//   - MJPEG 条带并行：2K 编码 9.9ms(101fps)、1080p 4.9ms(206fps)；
//   - 每帧独立 → 新观众天然秒开、丢帧不花屏、分辨率随便切。
//
// 唯一短板是带宽（2K 约 410Mbps @30fps 全量）。对策是 dirty tile：
// 屏幕多数时候只有局部在动，只编码变化的条带。
package codec

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"log"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// reportOversizedTile 对"降到地板还装不下"的条带记一笔（只打一次，避免刷屏）。
var oversizedLogged atomic.Bool

func reportOversizedTile(n int) {
	if oversizedLogged.CompareAndSwap(false, true) {
		log.Printf("codec: 有条带降到 q%d 仍超过单条带上限（%d > %d 字节）—— "+
			"它会以超大消息发出；通常说明共享内容是高细节画面（照片/视频），可调小条带高度",
			minTileQuality, n, maxSingleTilePayload)
	}
}

// MCU 高：4:2:0 下为 16。条带高度必须对齐，否则相邻条带边界会互相串色。
const mcuHeight = 16

// Config 是编码器配置。
type Config struct {
	// Quality 是 JPEG 质量 1~100，默认 75。
	Quality int
	// Tiles 是水平条带数量，<=0 时取 NumCPU。更多条带 = 更细的增量粒度，
	// 但单条带过小会损失一点压缩率。
	Tiles int
	// Workers 是并行度，<=0 时取 NumCPU。
	Workers int
	// Dirty 启用脏条带增量更新。关闭时每帧全量编码（带宽高但实现路径最简单）。
	Dirty bool
}

func (c Config) withDefaults() Config {
	if c.Quality <= 0 {
		c.Quality = 75
	}
	if c.Quality > 100 {
		c.Quality = 100
	}
	if c.Tiles <= 0 {
		c.Tiles = runtime.NumCPU()
	}
	if c.Workers <= 0 {
		c.Workers = runtime.NumCPU()
	}
	return c
}

// Tile 是一个水平条带的编码结果。
type Tile struct {
	Index int
	Y0    int
	Y1    int
	Data  []byte
}

// Frame 是一帧的编码结果。
//
// ⚠️ 一次 Marshal / Unmarshal 对应线路上**一个分块**，不是一个完整帧：
// 2K 全量帧约 2MB，必须切成十几个分块才能发；每个分块自带完整帧头 + 若干**完整**
// 条带记录（条带绝不跨块拆分），于是接收端收到一块就能解一块 —— 这是"丢一块只丢
// 它带的条带"的前提（旧格式要求凑齐所有分块才能解析，丢任何一块 = 整帧 2MB 作废）。
//
// Tiles / Full / TilesMask 描述的都是**整个逻辑帧**：Tiles 只有本块携带的那些，
// TilesMask 是本帧（跨全部块）包含哪些条带 —— 接收端据此知道该期待哪些条带，
// 从而精确判定"哪些条带丢了"，而不是笼统地认为整帧丢了。
type Frame struct {
	Seq        uint64
	W          int
	H          int
	TileH      int // 标准条带高度（最后一个条带可能更矮）
	TotalTiles int
	Tiles      []Tile // 本**块**携带的条带（未变化的条带不出现）
	Full       bool   // 整个逻辑帧是否包含全部条带（首帧 / 强制关键帧 / 变化过大）
	// TilesMask 是逻辑帧的条带集合位图（bit i = 条带 i 在本帧内）。
	// 每块里都写同一份 —— 这样任一块到达都能告诉接收端"还缺哪些"。
	TilesMask uint64
	// Refresh 是修复轮次：0 = 稳态；>0 表示分享端正在做渐进式修复（轮转发条带）。
	// 接收端用它判断"修复在飞，别重复要全量"。
	Refresh uint8
	// ChunkIdx / ChunkTotal 是本块在帧内的序号与总块数（未被 Split 时 = 0/1）。
	ChunkIdx   uint8
	ChunkTotal uint8
	TS         int64
	EncodeMs   float64
	Bytes      int
	DirtyTiles int
}

// Encoder 把紧凑 BGRA 帧编码为 MJPEG 条带。非并发安全。
type Encoder struct {
	cfg      Config
	w, h     int
	tileH    int
	nTiles   int
	i420     []byte // 当前帧（写入目标）
	prev     []byte // 上一帧（用于脏检测）
	havePrev bool
	seq      uint64
	i420Size int
	// ---- 渐进式修复状态（见 BeginRepair）----
	repairLeft  int   // 本轮还差多少条带没轮转
	repairStep  int   // 每帧轮转多少条带
	repairCur   int   // 轮转游标
	repairEpoch uint8 // 当前轮次号（写进每帧的 Refresh 字段）
	refresh     uint8 // 本帧要写出的轮次号（0 = 稳态）
}

// NewEncoder 创建编码器。初始尺寸未知，首次 Encode 时按帧尺寸初始化。
func NewEncoder(cfg Config) *Encoder {
	return &Encoder{cfg: cfg.withDefaults()}
}

// BeginRepair 开始一轮**渐进式修复**：把"补齐整屏"分摊到 overFrames 帧里，
// 每帧多带 1/overFrames 的条带（无论有没有变化）。
//
// 为什么不用一次性全量帧：全量帧是一发 2MB 的突发，在弱网/拥塞时大概率整发丢掉，
// 而丢掉之后观众端只会再来要一次 —— "越要越堵、越堵越黑"的正反馈。
// 摊成十几帧后单帧依然很小，拥塞时也发得出去；总流量与一发全量帧相同，
// 只是用 0.3 秒铺开（30fps 下 overFrames=10）。
//
// overFrames <= 0 时按 10 帧算。已经在修复中时重复调用会被忽略（一轮跑完再说）。
func (e *Encoder) BeginRepair(overFrames int) {
	if e.nTiles == 0 || e.repairLeft > 0 {
		return
	}
	if overFrames <= 0 {
		overFrames = defaultRepairFrames
	}
	step := (e.nTiles + overFrames - 1) / overFrames
	if step < 1 {
		step = 1
	}
	e.repairStep = step
	e.repairLeft = e.nTiles
	e.repairCur = 0
	e.repairEpoch++
	if e.repairEpoch == 0 {
		e.repairEpoch = 1 // 0 是"稳态"的保留值
	}
}

// Repairing 报告当前是否有一轮修复在飞。
func (e *Encoder) Repairing() bool { return e.repairLeft > 0 }

// Config 返回生效配置。
func (e *Encoder) Config() Config { return e.cfg }

// SetQuality 热切换 JPEG 质量（自适应码率用）。
//
// 只在编码 goroutine 里调用（Encoder 非并发安全）。
// 不需要强制全量帧：每个条带都是独立 JPEG，质量只影响后续编码的条带，
// 观众端画布上的旧条带不受影响 —— 这恰恰是想要的：网络已经在堵了，
// 不能再补一发全量帧火上浇油。
func (e *Encoder) SetQuality(q int) {
	if q < 1 {
		q = 1
	}
	if q > 100 {
		q = 100
	}
	e.cfg.Quality = q
}

// Size 返回当前编码尺寸。
func (e *Encoder) Size() (w, h int) { return e.w, e.h }

// Reset 重置到指定尺寸，丢弃脏检测历史（下一帧自动成为全量帧）。
// 分辨率中途变化时必须调用（例如分享端切换显示器）。
func (e *Encoder) Reset(w, h int) {
	e.w, e.h = w, h
	n := w * h * 3 / 2
	e.i420Size = n
	if cap(e.i420) < n {
		e.i420 = make([]byte, n)
	}
	e.i420 = e.i420[:n]
	if cap(e.prev) < n {
		e.prev = make([]byte, n)
	}
	e.prev = e.prev[:n]
	e.havePrev = false

	tileH := (h/e.cfg.Tiles + mcuHeight - 1) / mcuHeight * mcuHeight
	if tileH < mcuHeight {
		tileH = mcuHeight
	}
	e.tileH = tileH
	e.nTiles = (h + tileH - 1) / tileH
	// 条带集合用 uint64 位图描述（见 Frame.TilesMask），所以要限个数。
	// 常规配置（Tiles = CPU 核数或 40）远达不到这个上限。
	if e.nTiles > maxTiles {
		e.tileH = ((h+maxTiles-1)/maxTiles + mcuHeight - 1) / mcuHeight * mcuHeight
		if e.tileH < mcuHeight {
			e.tileH = mcuHeight
		}
		e.nTiles = (h + e.tileH - 1) / e.tileH
	}
}

// ForceKeyFrame 让下一帧编码为全量帧（新观众加入、重连时调用）。
func (e *Encoder) ForceKeyFrame() { e.havePrev = false }

// Encode 编码一帧紧凑 BGRA。
// 返回的 Frame.Tiles 引用内部缓冲之外的独立分配，可安全跨 goroutine 使用。
func (e *Encoder) Encode(bgra []byte, w, h int) (*Frame, error) {
	if w <= 0 || h <= 0 {
		return nil, errors.New("codec: 无效帧尺寸")
	}
	if len(bgra) < w*h*4 {
		return nil, fmt.Errorf("codec: 帧数据不足，需要 %d 字节，实际 %d", w*h*4, len(bgra))
	}
	if w != e.w || h != e.h {
		e.Reset(w, h)
	}
	t0 := time.Now()

	// 与上一帧交替：prev 保留历史，i420 作为本次写入目标。
	// BGRAtoI420 会写满整块，所以交换后旧 prev 被完全覆盖，安全且省一次 7.7MB 拷贝。
	e.i420, e.prev = e.prev, e.i420
	BGRAtoI420(e.i420, bgra, w, h, e.cfg.Workers)

	full := !e.cfg.Dirty || !e.havePrev
	var dirty []int
	if full {
		dirty = make([]int, e.nTiles)
		for i := range dirty {
			dirty[i] = i
		}
		e.repairLeft = 0 // 整帧都发了，修复轮次没必要继续
	} else {
		dirty = dirtyTiles(e.i420, e.prev, w, h, e.tileH, e.nTiles)
		// 变化面积极大时，增量已无意义（省不了带宽还多付了比较开销）→ 转全量
		if len(dirty)*4 > e.nTiles*3 {
			full = true
			dirty = make([]int, e.nTiles)
			for i := range dirty {
				dirty[i] = i
			}
			e.repairLeft = 0
		} else if e.repairLeft > 0 {
			// 渐进式修复：本帧额外轮转 repairStep 个条带（不管它们有没有变化）。
			dirty = e.addRepairTiles(dirty)
		}
	}
	// TilesMask 描述的是**整个逻辑帧**（跨全部块），接收端据此判断丢了哪些条带。
	var mask uint64
	for _, idx := range dirty {
		mask |= 1 << uint(idx)
	}
	if full {
		mask = allTilesMask(e.nTiles)
	}

	img := I420Image(e.i420, w, h)
	tiles := make([]Tile, len(dirty))
	var wg sync.WaitGroup
	// 限制并行度，避免条带数远大于核数时产生过多 goroutine
	sem := make(chan struct{}, e.cfg.Workers)
	for k, idx := range dirty {
		y0 := idx * e.tileH
		y1 := y0 + e.tileH
		if y1 > h {
			y1 = h
		}
		wg.Add(1)
		go func(k, idx, y0, y1 int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			sub := img.SubImage(image.Rect(0, y0, w, y1))
			var buf bytes.Buffer
			// 单条带不会被拆开，所以它超了标称分块大小也只是"自己占一块更大的消息"。
			// 这里只在它超过**硬上限**（1MB，正常永远到不了）时才降质重编 ——
			// 曾经把阈值定在 60KB，结果 2K 高细节画面被降质，PSNR 直接掉 3.5dB。
			q := e.cfg.Quality
			for {
				buf.Reset()
				if err := jpeg.Encode(&buf, sub, &jpeg.Options{Quality: q}); err != nil {
					break
				}
				if buf.Len() <= maxSingleTilePayload || q <= minTileQuality {
					break
				}
				q -= 15
				if q < minTileQuality {
					q = minTileQuality
				}
			}
			if buf.Len() > maxSingleTilePayload {
				reportOversizedTile(buf.Len())
			}
			tiles[k] = Tile{Index: idx, Y0: y0, Y1: y1, Data: buf.Bytes()}
		}(k, idx, y0, y1)
	}
	wg.Wait()

	// 过滤掉编码失败的空条带（不应发生，但别让观众端拿到空数据）
	out := tiles[:0]
	total := 0
	for _, t := range tiles {
		if len(t.Data) == 0 {
			continue
		}
		total += len(t.Data)
		out = append(out, t)
	}

	e.havePrev = true
	e.seq++
	// 本帧要写出的修复轮次：修复还在飞就带上轮次号，跑完自动回 0（稳态）。
	if e.repairLeft > 0 {
		e.refresh = e.repairEpoch
	} else {
		e.refresh = 0
	}
	return &Frame{
		Seq:        e.seq,
		W:          w,
		H:          h,
		TileH:      e.tileH,
		TotalTiles: e.nTiles,
		Tiles:      out,
		Full:       full,
		TilesMask:  mask,
		Refresh:    e.refresh,
		ChunkIdx:   0,
		ChunkTotal: 1,
		TS:         time.Now().UnixMilli(),
		EncodeMs:   float64(time.Since(t0).Microseconds()) / 1000.0,
		Bytes:      total,
		DirtyTiles: len(out),
	}, nil
}

// addRepairTiles 把本轮修复的下 repairStep 个条带并进 dirty（去重）。
// 条带按游标轮转，一轮走完全部条带 —— 观众端画布因此在 ~overFrames 帧内铺满。
func (e *Encoder) addRepairTiles(dirty []int) []int {
	if e.nTiles <= 0 {
		return dirty
	}
	seen := make(map[int]struct{}, len(dirty))
	for _, idx := range dirty {
		seen[idx] = struct{}{}
	}
	for i := 0; i < e.repairStep && e.repairLeft > 0; i++ {
		idx := e.repairCur % e.nTiles
		e.repairCur++
		e.repairLeft--
		if _, ok := seen[idx]; ok {
			continue
		}
		seen[idx] = struct{}{}
		dirty = append(dirty, idx)
	}
	return dirty
}

// allTilesMask 返回 nTiles 个条带全在的位图（nTiles 上限见 maxTiles。
// 超出 64 个条带时退化为"最高位全 1"，接收端只会更保守）。
func allTilesMask(nTiles int) uint64 {
	if nTiles >= 64 {
		return ^uint64(0)
	}
	return 1<<uint(nTiles) - 1
}

// dirtyTiles 逐条带比较 I420，返回有变化的条带下标。
func dirtyTiles(cur, prev []byte, w, h, tileH, nTiles int) []int {
	var out []int
	for t := 0; t < nTiles; t++ {
		y0, y1 := t*tileH, (t+1)*tileH
		if y1 > h {
			y1 = h
		}
		if tileChanged(cur, prev, w, h, y0, y1) {
			out = append(out, t)
		}
	}
	return out
}

// tileChanged 比较 I420 缓冲中某一水平条带（Y 行 y0..y1 及其色度行）是否有变化。
// 用 bytes.Equal 逐行比，走的是平台 SIMD，2K 全屏一轮也只有几毫秒。
func tileChanged(cur, prev []byte, w, h, y0, y1 int) bool {
	// 亮度
	for j := y0; j < y1; j++ {
		a := j * w
		if !bytes.Equal(cur[a:a+w], prev[a:a+w]) {
			return true
		}
	}
	// 色度（Cb、Cr 各占 h/2 行，每行 w/2 字节）
	ySize := w * h
	cw, ch := w/2, h/2
	cy0, cy1 := y0/2, (y1+1)/2
	if cy1 > ch {
		cy1 = ch
	}
	for c := 0; c < 2; c++ {
		base := ySize + c*cw*ch
		for j := cy0; j < cy1; j++ {
			a := base + j*cw
			if !bytes.Equal(cur[a:a+cw], prev[a:a+cw]) {
				return true
			}
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 协议版本
// ---------------------------------------------------------------------------

// ProtoVersion 是**媒体线格式**的版本号。任何改变线格式的改动都要 +1。
//
// 为什么必须有它：线格式变了以后，新旧混用不会报错，而是"对接上了但画面全黑"
// （旧端按老格式解析新分块，得到的长度/序号是乱的，静默丢弃）。
// 所以两端在 HTTP 接入阶段就互报版本，不一致时给一句人能看懂的话：
// 「双方版本不一致，请把两台机器都升级到同一版本」。
//
// 历史：1 = GSJ1（整帧一个消息、按字节切块）；2 = GSJ2（每块自带帧头 + 完整条带）。
const ProtoVersion = 2

// ---------------------------------------------------------------------------
// Wire 格式
// ---------------------------------------------------------------------------
//
// ⚠️ 线路上**一个消息 = 一个分块**，不是一个完整帧（2K 全量约 2MB，必须切块发）。
// 每个分块自带完整帧头 + 若干**完整**条带记录，条带绝不跨块拆分 ——
// 于是接收端收到一块就能解一块：丢一块只丢它带的那几条带，而不是整帧作废。
// （旧格式 GCJ1 是"整帧一个消息、按字节切块"，丢任意一块整帧都解析不出来。）
//
//	0  .. 3  magic "GSJ2"
//	4  .. 11 seq        uint64 BE
//	12 .. 15 ts         uint32 BE（Unix 毫秒低 32 位，仅用于延迟统计）
//	16 .. 17 width      uint16 BE
//	18 .. 19 height     uint16 BE
//	20 .. 21 totalTiles uint16 BE
//	22 .. 23 tileH      uint16 BE
//	24       flags      bit0 = 整个逻辑帧含全部条带
//	25       refresh    修复轮次（0 = 稳态）
//	26       chunkIdx   本块序号
//	27       chunkTotal 本帧总块数
//	28 .. 35 tilesMask  uint64 BE：整个逻辑帧包含哪些条带（每块都写同一份，
//	                    接收端据此精确知道"该期待哪些条带、丢了哪几条"）
//	此后每条带（永不跨块拆分）：
//	   0 .. 1  index    uint16 BE
//	   2 .. 3  y0       uint16 BE
//	   4 .. 7  dataLen  uint32 BE
//	   8 ..    JPEG 数据

const (
	frameHeaderLen = 36
	tileHeaderLen  = 8
	flagFull       = 1 << 0
	// maxTiles 是条带数上限：条带集合用 uint64 位图描述（见 Frame.TilesMask）。
	maxTiles = 64
	// MaxChunkPayload 是分块的**标称**字节上限（尽量不超过）。
	// rtc 的切块用它 —— 但注意：单个条带永远不会被拆开（拆了接收端就要为它
	// 保留重组状态，等于把刚拆掉的那套复杂度请回来），所以当一个条带本身就比
	// 标称值大时，它会**单独成块发一条更大的消息**。
	// 实测（out/msgsize 探针）：pion↔pion 的 DataChannel 512KB 消息照样送达，
	// 标称 60KB 只是"保持丢失粒度细"的偏好，不是硬上限。
	MaxChunkPayload = 60 << 10
	// maxSingleTilePayload 是单条带的硬上限，超过才降质重编（安全阀，不该触发）。
	// 2K 一条带正常 10~40KB，高细节画面也就一两百 KB。
	maxSingleTilePayload = 1 << 20
	// minTileQuality 是"为了塞进硬上限而降质重编"的地板。
	minTileQuality = 30
	// defaultRepairFrames 是渐进式修复默认摊到多少帧（见 Encoder.BeginRepair）。
	defaultRepairFrames = 10
)

var frameMagic = [4]byte{'G', 'S', 'J', '2'}

// Marshal 把**一个分块**序列化为线格式（见上面的格式说明）。
//
// 若这帧尚未 Split，则整帧就是一个分块（chunkTotal=1），用于测试与小帧。
// 发送端按消息大小上限切片（见 internal/rtc 的 chunkPayload）。
func (f *Frame) Marshal() ([]byte, error) {
	if f.W > 65535 || f.H > 65535 {
		return nil, errors.New("codec: 尺寸超出线格式上限")
	}
	size := frameHeaderLen
	for _, t := range f.Tiles {
		size += tileHeaderLen + len(t.Data)
	}
	buf := make([]byte, size)
	copy(buf[0:4], frameMagic[:])
	binary.BigEndian.PutUint64(buf[4:12], f.Seq)
	binary.BigEndian.PutUint32(buf[12:16], uint32(f.TS))
	binary.BigEndian.PutUint16(buf[16:18], uint16(f.W))
	binary.BigEndian.PutUint16(buf[18:20], uint16(f.H))
	binary.BigEndian.PutUint16(buf[20:22], uint16(f.TotalTiles))
	binary.BigEndian.PutUint16(buf[22:24], uint16(f.TileH))
	if f.Full {
		buf[24] = flagFull
	}
	buf[25] = f.Refresh
	buf[26] = f.ChunkIdx
	buf[27] = f.ChunkTotal
	if buf[27] == 0 {
		buf[27] = 1 // 未 Split 的单块帧
	}
	binary.BigEndian.PutUint64(buf[28:36], f.TilesMask)
	off := frameHeaderLen
	for _, t := range f.Tiles {
		binary.BigEndian.PutUint16(buf[off:off+2], uint16(t.Index))
		binary.BigEndian.PutUint16(buf[off+2:off+4], uint16(t.Y0))
		binary.BigEndian.PutUint32(buf[off+4:off+8], uint32(len(t.Data)))
		off += tileHeaderLen
		copy(buf[off:], t.Data)
		off += len(t.Data)
	}
	return buf, nil
}

// Split 把整帧切成若干个**可独立解析**的分块（每块自带帧头 + 完整条带记录）。
//
// maxPayload 是标称的单块字节上限。条带**不会**被拆开：一个条带本身就超过标称值时，
// 它单独成一块（消息更大，但接收端照样独立解析 —— 实测 pion↔pion 512KB 消息可达）。
func (f *Frame) Split(maxPayload int) []*Frame {
	if len(f.Tiles) == 0 || maxPayload <= frameHeaderLen {
		f.ChunkIdx, f.ChunkTotal = 0, 1
		return []*Frame{f}
	}
	budget := maxPayload - frameHeaderLen
	var chunks []*Frame
	cur := &Frame{}
	curSize := 0
	flush := func() {
		if cur == nil || len(cur.Tiles) == 0 {
			return
		}
		chunks = append(chunks, cur)
		cur = &Frame{}
		curSize = 0
	}
	for _, t := range f.Tiles {
		need := tileHeaderLen + len(t.Data)
		if len(cur.Tiles) > 0 && curSize+need > budget {
			flush()
		}
		if cur.Tiles == nil {
			// 每块都要带完整帧头（接收端靠它独立解析）。
			*cur = Frame{
				Seq: f.Seq, W: f.W, H: f.H, TileH: f.TileH, TotalTiles: f.TotalTiles,
				Full: f.Full, TilesMask: f.TilesMask, Refresh: f.Refresh,
				TS: f.TS, EncodeMs: f.EncodeMs,
			}
		}
		cur.Tiles = append(cur.Tiles, t)
		cur.Bytes += len(t.Data)
		curSize += need
	}
	flush()
	total := len(chunks)
	if total > 255 {
		// 理论上到不了（单帧最大 64 条带、单块 60KB）；真到了就退化成一块超大消息，
		// 让上层按消息发送，别静默丢内容。
		total = 255
	}
	for i, c := range chunks {
		c.ChunkIdx = uint8(i)
		c.ChunkTotal = uint8(total)
		c.DirtyTiles = len(c.Tiles)
	}
	return chunks
}

// UnmarshalFrame 解析**一个分块**（自带帧头，无需与其他块拼接）。
func UnmarshalFrame(buf []byte) (*Frame, error) {
	if len(buf) < frameHeaderLen {
		return nil, errors.New("codec: 分块过短")
	}
	if !bytes.Equal(buf[0:4], frameMagic[:]) {
		return nil, errors.New("codec: 魔数不匹配（对方可能是旧版本，两个端都要升级）")
	}
	f := &Frame{
		Seq:        binary.BigEndian.Uint64(buf[4:12]),
		TS:         int64(binary.BigEndian.Uint32(buf[12:16])),
		W:          int(binary.BigEndian.Uint16(buf[16:18])),
		H:          int(binary.BigEndian.Uint16(buf[18:20])),
		TotalTiles: int(binary.BigEndian.Uint16(buf[20:22])),
		TileH:      int(binary.BigEndian.Uint16(buf[22:24])),
	}
	f.Full = buf[24]&flagFull != 0
	f.Refresh = buf[25]
	f.ChunkIdx = buf[26]
	f.ChunkTotal = buf[27]
	if f.ChunkTotal == 0 {
		f.ChunkTotal = 1
	}
	f.TilesMask = binary.BigEndian.Uint64(buf[28:36])
	off := frameHeaderLen
	for off+tileHeaderLen <= len(buf) {
		idx := int(binary.BigEndian.Uint16(buf[off : off+2]))
		y0 := int(binary.BigEndian.Uint16(buf[off+2 : off+4]))
		n := int(binary.BigEndian.Uint32(buf[off+4 : off+8]))
		off += tileHeaderLen
		if off+n > len(buf) {
			return nil, errors.New("codec: 条带长度越界")
		}
		data := make([]byte, n)
		copy(data, buf[off:off+n])
		off += n
		y1 := y0 + f.TileH
		if y1 > f.H {
			y1 = f.H
		}
		f.Tiles = append(f.Tiles, Tile{Index: idx, Y0: y0, Y1: y1, Data: data})
		f.Bytes += n
	}
	f.DirtyTiles = len(f.Tiles)
	return f, nil
}

// ---------------------------------------------------------------------------
// Decoder
// ---------------------------------------------------------------------------

// Decoder 把 MJPEG 条带解码进一张持久的 NRGBA 画布。
// 未在本帧出现的条带保持上一帧内容（这正是增量更新的前提）。
type Decoder struct {
	img     *image.NRGBA
	w, h    int
	workers int
	seq     uint64
	// DecodeMs 是最近一帧的解码耗时（诊断用）。
	DecodeMs float64

	// ---- 分块级接收的画布完整性（见 Decode 与 Complete）----
	tileCount int    // 条带总数（画布尺寸变化时重置）
	have      uint64 // 画布上是"当前内容"的条带位图
	curSeq    uint64 // 正在收集的那一帧
	curExpect uint64 // 该帧应到（TilesMask）
	curGot    uint64 // 该帧实到
	maxSeq    uint64 // 已见过的最大 seq（乱序保护）
}

// NewDecoder 创建解码器。workers<=0 时取 NumCPU。
func NewDecoder(workers int) *Decoder {
	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	return &Decoder{workers: workers}
}

// Image 返回当前画布（可能为空）。画布内容在多次 Decode 间累积保留。
func (d *Decoder) Image() *image.NRGBA { return d.img }

// Seq 返回最近解出的帧序号。
func (d *Decoder) Seq() uint64 { return d.seq }

// reorderWindow 是"乱序容忍窗口"（帧）。
//
// 媒体通道是无序的：晚了几个帧的分块再到达时，如果照解就会把**新画面盖回旧内容**。
// 超过这个窗口的分块一律丢弃 —— 局域网内跨 3 帧乱序基本不存在，
// 而少了这道闸，丢帧时的花屏会变成"新旧交替闪烁"，很难查。
const reorderWindow = 4

// Complete 报告画布是否"每个条带都是当前内容"。
//
// 判据是我们自己维护的 have 位图（不是"收到过全量帧"）：
//   - 解出条带 → 置位；
//   - 一帧收尾时，**该到而没到的条带**（TilesMask 里有、实际没收到）→ 清位，
//     它们的内容已经过期（分块丢了就是丢了，增量帧不会重发）。
//
// 这是"卡顿黑屏"的判定入口：不完整就请对端做一轮渐进式修复。
func (d *Decoder) Complete() bool {
	if d.tileCount <= 0 {
		return false
	}
	return d.have == allTilesMask(d.tileCount)
}

// Missing 返回"内容已过期的条带"位图（诊断用）。
func (d *Decoder) Missing() uint64 {
	if d.tileCount <= 0 {
		return 0
	}
	return allTilesMask(d.tileCount) &^ d.have
}

// Have 返回"内容是当前值"的条带位图（诊断/统计用）。
func (d *Decoder) Have() uint64 { return d.have }

// TileCount 返回当前画布的条带总数。
func (d *Decoder) TileCount() int { return d.tileCount }

// Decode 把一个**分块**解到画布上（一个分块可能只带少数几个条带）。
// 分辨率变化时自动重建画布（MJPEG 天然支持，不需要像 H.264 那样重配解码器）。
//
// ⚠️ 语义与旧版不同：以前传进来的是"整帧"（必须凑齐所有分块才解析得出来），
// 现在是一个分块。丢一块只丢它带的那几条带，其余照常上屏。
func (d *Decoder) Decode(f *Frame) (*image.NRGBA, error) {
	return d.DecodeBatch([]*Frame{f})
}

// DecodeBatch 把若干分块（可以来自不同帧）一次解进画布。
//
// 为什么需要"批量"：单块通常只带 1~2 个条带，而 JPEG 解码是 CPU 大头
// （2K 一条带 ~3ms）。逐块串行解 = 把帧内的并行度全丢掉 —— 实测 2K 全量从
// 7ms 涨到 98ms。攒一批一起解，就能把所有条带铺到一个并行池上跑。
func (d *Decoder) DecodeBatch(frames []*Frame) (*image.NRGBA, error) {
	if len(frames) == 0 {
		return d.img, nil
	}
	t0 := time.Now()

	// ---- 1) 先按顺序处理"帧边界 / 乱序 / 尺寸变化"这类状态 ----
	var jobs []Tile
	for _, f := range frames {
		if f == nil || f.W <= 0 || f.H <= 0 {
			continue
		}
		if d.img == nil || d.w != f.W || d.h != f.H {
			d.img = image.NewNRGBA(image.Rect(0, 0, f.W, f.H))
			d.w, d.h = f.W, f.H
			d.seq = 0
			// 画布尺寸变了 = 全新一代画布：完整性位图与乱序窗口全部重来。
			d.tileCount = f.TotalTiles
			d.have = 0
			d.curSeq, d.curExpect, d.curGot, d.maxSeq = 0, 0, 0, 0
		}
		if f.TotalTiles > d.tileCount {
			d.tileCount = f.TotalTiles
		}
		// 一帧收尾：上一帧"该到没到"的条带内容已过期 → 清位（修复会补）。
		if f.Seq != d.curSeq {
			d.have &^= d.curExpect &^ d.curGot
			d.curSeq, d.curExpect, d.curGot = f.Seq, 0, 0
		}
		// 乱序保护：太旧的分块直接丢（否则旧内容会盖掉新画面）。
		if d.maxSeq > reorderWindow && f.Seq+reorderWindow < d.maxSeq {
			continue
		}
		if f.Seq > d.maxSeq {
			d.maxSeq = f.Seq
		}
		d.curExpect |= f.TilesMask
		d.seq = f.Seq
		jobs = append(jobs, f.Tiles...)
	}
	if len(jobs) == 0 {
		d.DecodeMs = float64(time.Since(t0).Microseconds()) / 1000.0
		return d.img, nil
	}

	// ---- 2) 并行解码 + 写画布（条带互不重叠，可以放心并行）----
	dst := d.img.Pix
	stride := d.w * 4
	var wg sync.WaitGroup
	sem := make(chan struct{}, d.workers)
	var mu sync.Mutex
	for _, t := range jobs {
		wg.Add(1)
		go func(t Tile) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			img, err := jpeg.Decode(bytes.NewReader(t.Data))
			if err != nil {
				return
			}
			writeTile(dst, stride, img, t.Y0, t.Y1, d.workers)
			mu.Lock()
			d.have |= 1 << uint(t.Index)
			d.curGot |= 1 << uint(t.Index)
			mu.Unlock()
		}(t)
	}
	wg.Wait()
	d.DecodeMs = float64(time.Since(t0).Microseconds()) / 1000.0
	return d.img, nil
}

// writeTile 把解码出的条带写进画布的 [y0,y1) 行。
func writeTile(dst []byte, stride int, img image.Image, y0, y1, workers int) {
	b := img.Bounds()
	hh := b.Dy()
	if hh > y1-y0 {
		hh = y1 - y0
	}
	rowStart := (y0 - b.Min.Y) * stride
	switch src := img.(type) {
	case *image.YCbCr:
		// 逐行写入：源可能是 SubImage，用 YOffset 处理偏移
		writeYCbCrRows(dst, stride, src, rowStart, hh, workers)
	case *image.RGBA:
		writeRGBARows(dst, stride, src, rowStart, hh)
	case *image.NRGBA:
		writeNRGBARows(dst, stride, src, rowStart, hh)
	}
}

func writeYCbCrRows(dst []byte, stride int, src *image.YCbCr, rowStart, hh, workers int) {
	w := src.Rect.Dx()
	n := workers
	if n > hh {
		n = hh
	}
	var wg sync.WaitGroup
	for k := 0; k < n; k++ {
		a, b := k*hh/n, (k+1)*hh/n
		if a >= b {
			continue
		}
		wg.Add(1)
		go func(a, b int) {
			defer wg.Done()
			for j := a; j < b; j++ {
				yOff := src.YOffset(src.Rect.Min.X, src.Rect.Min.Y+j)
				cOff := src.COffset(src.Rect.Min.X, src.Rect.Min.Y+j)
				drow := rowStart + j*stride
				for i := 0; i < w; i++ {
					yy := int(src.Y[yOff+i]) << 8
					ci := cOff + i/2
					cb := int(src.Cb[ci]) - 128
					cr := int(src.Cr[ci]) - 128
					p := drow + i*4
					dst[p] = clamp8((yy + 359*cr) >> 8)
					dst[p+1] = clamp8((yy - 88*cb - 183*cr) >> 8)
					dst[p+2] = clamp8((yy + 454*cb) >> 8)
					dst[p+3] = 255
				}
			}
		}(a, b)
	}
	wg.Wait()
}

func writeRGBARows(dst []byte, stride int, src *image.RGBA, rowStart, hh int) {
	w := src.Rect.Dx()
	for j := 0; j < hh; j++ {
		s := src.PixOffset(src.Rect.Min.X, src.Rect.Min.Y+j)
		d := rowStart + j*stride
		copy(dst[d:d+w*4], src.Pix[s:s+w*4])
	}
}

func writeNRGBARows(dst []byte, stride int, src *image.NRGBA, rowStart, hh int) {
	w := src.Rect.Dx()
	for j := 0; j < hh; j++ {
		s := src.PixOffset(src.Rect.Min.X, src.Rect.Min.Y+j)
		d := rowStart + j*stride
		copy(dst[d:d+w*4], src.Pix[s:s+w*4])
	}
}
