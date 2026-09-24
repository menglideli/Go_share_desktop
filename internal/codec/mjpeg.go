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
	"runtime"
	"sync"
	"time"
)

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

// Frame 是一帧编码结果。Tiles 只包含本帧真正编码的条带：
// 未变化的条带不会出现在里面（观众端保留上一帧对应行即可）。
type Frame struct {
	Seq         uint64
	W           int
	H           int
	TileH       int // 标准条带高度（最后一个条带可能更矮）
	TotalTiles  int
	Tiles       []Tile
	Full        bool // true 表示全量帧（首帧 / 强制关键帧 / 变化过大）
	TS          int64
	EncodeMs    float64
	Bytes       int
	DirtyTiles  int
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
}

// NewEncoder 创建编码器。初始尺寸未知，首次 Encode 时按帧尺寸初始化。
func NewEncoder(cfg Config) *Encoder {
	return &Encoder{cfg: cfg.withDefaults()}
}

// Config 返回生效配置。
func (e *Encoder) Config() Config { return e.cfg }

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
	} else {
		dirty = dirtyTiles(e.i420, e.prev, w, h, e.tileH, e.nTiles)
		// 变化面积极大时，增量已无意义（省不了带宽还多付了比较开销）→ 转全量
		if len(dirty)*4 > e.nTiles*3 {
			full = true
			dirty = make([]int, e.nTiles)
			for i := range dirty {
				dirty[i] = i
			}
		}
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
			if err := jpeg.Encode(&buf, sub, &jpeg.Options{Quality: e.cfg.Quality}); err != nil {
				return
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
	return &Frame{
		Seq:        e.seq,
		W:          w,
		H:          h,
		TileH:      e.tileH,
		TotalTiles: e.nTiles,
		Tiles:      out,
		Full:       full,
		TS:         time.Now().UnixMilli(),
		EncodeMs:   float64(time.Since(t0).Microseconds()) / 1000.0,
		Bytes:      total,
		DirtyTiles: len(out),
	}, nil
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
// Wire 格式
// ---------------------------------------------------------------------------
//
// 帧 = 28 字节头 + N 个条带记录。
// 采用定长头 + 变长条带，接收端一次解析即可，无需流式状态机。
//
//	0  .. 3  magic "GSJ1"
//	4  .. 11 seq        uint64 BE
//	12 .. 15 ts         uint32 BE（Unix 毫秒低 32 位，仅用于延迟统计）
//	16 .. 17 width      uint16 BE
//	18 .. 19 height     uint16 BE
//	20 .. 21 totalTiles uint16 BE
//	22 .. 23 tileH      uint16 BE
//	24       flags      bit0 = 全量帧
//	25 .. 27 reserved
//	此后每条带：
//	   0 .. 1  index    uint16 BE
//	   2 .. 3  y0       uint16 BE
//	   4 .. 7  dataLen  uint32 BE
//	   8 ..    JPEG 数据

const (
	frameHeaderLen = 28
	tileHeaderLen  = 8
	flagFull       = 1 << 0
)

var frameMagic = [4]byte{'G', 'S', 'J', '1'}

// Marshal 序列化为线格式。巨帧（2K 高质量）可能上百 KB，
// 发送端需按 MaxMessageSize 分片（见 internal/net）。
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

// UnmarshalFrame 解析线格式帧。
func UnmarshalFrame(buf []byte) (*Frame, error) {
	if len(buf) < frameHeaderLen {
		return nil, errors.New("codec: 帧过短")
	}
	if !bytes.Equal(buf[0:4], frameMagic[:]) {
		return nil, errors.New("codec: 帧魔数不匹配")
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

// Decode 解码一帧到画布。分辨率变化时自动重建画布（MJPEG 天然支持，
// 不需要像 H.264 那样重配解码器 —— 这是选 MJPEG 的收益之一）。
func (d *Decoder) Decode(f *Frame) (*image.NRGBA, error) {
	if f == nil || f.W <= 0 || f.H <= 0 {
		return nil, errors.New("codec: 无效帧")
	}
	t0 := time.Now()
	if d.img == nil || d.w != f.W || d.h != f.H {
		d.img = image.NewNRGBA(image.Rect(0, 0, f.W, f.H))
		d.w, d.h = f.W, f.H
		d.seq = 0
	}
	dst := d.img.Pix
	stride := f.W * 4

	var wg sync.WaitGroup
	sem := make(chan struct{}, d.workers)
	for _, t := range f.Tiles {
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
		}(t)
	}
	wg.Wait()
	d.seq = f.Seq
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
