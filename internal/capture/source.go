package capture

import (
	"context"
	"errors"
	"time"

	"github.com/go-mswin/screencapture"
)

// errNoFrame 表示底层返回了帧句柄但内容无效（或区域裁切后为空）。
var errNoFrame = errors.New("capture: 无有效帧")

// ErrNoFrame 暴露给调用方用于 errors.Is 判定。
func ErrNoFrame() error { return errNoFrame }

// Frame 是采集管线统一的输出格式：紧凑 BGRA（无行 padding），
// 已应用区域裁切与光标叠加。下游（JPEG 条带编码）依赖紧凑布局。
type Frame struct {
	Pix []byte
	W   int
	H   int
	Seq uint64
	TS  time.Time
	// CaptureMs 是采集一帧的总耗时（WaitMs + ProcMs），保留兼容旧调用方。
	CaptureMs float64
	// WaitMs 是阻塞等待桌面产生变化的时间。
	//
	// ⚠️ 千万别把它当"采集开销"：DXGI Duplication 是**变化驱动**的，
	// 桌面静止时 WaitFrame 会一直阻塞（实测静止 2s 只出 1 帧），
	// 此时 WaitMs 可以高达数百毫秒，但 CPU 实际是空闲的。
	// 真正的 CPU 开销看 ProcMs。
	WaitMs float64
	// ProcMs 是拿到原始帧之后的处理耗时：区域裁切 + 光标叠加。
	// 这才是能计入 CPU 预算的数字。
	ProcMs float64
}

// Empty 报告该帧是否无有效数据。
func (f Frame) Empty() bool { return len(f.Pix) == 0 || f.W <= 0 || f.H <= 0 }

// Clone 返回帧的深拷贝。
func (f Frame) Clone() Frame {
	if f.Empty() {
		return Frame{}
	}
	p := make([]byte, len(f.Pix))
	copy(p, f.Pix)
	f.Pix = p
	return f
}

// Options 描述一次采集会话。
type Options struct {
	Display Display
	Region  Rect  // 相对显示器左上角的物理像素区域；为空则整屏
	FPS     int   // 目标帧率，0 表示用库默认值
	Cursor  bool  // 是否叠加鼠标光标（自绘，不走库选项）
}

// Source 是一个采集会话。非并发安全：同一时刻只能有一个 goroutine 取帧。
type Source struct {
	opt    Options
	stream *screencapture.Stream
	region Rect
	// bufs 是双缓冲：本帧写 bufs[idx]，上一帧的内容留在 bufs[1-idx]。
	// 这样 Last() 可直接引用上一块，省掉每帧一次整帧深拷贝
	//（2K 整屏 20MB，实测是笔不小的开销）。
	bufs    [2][]byte
	idx     int
	last    Frame
	frames  uint64
	draws   uint64
	capture time.Duration
	wait    time.Duration
	proc    time.Duration
	started time.Time
}

// NewSource 启动采集。region 为空时采集整个显示器。
func NewSource(ctx context.Context, opt Options) (*Source, error) {
	region := opt.Region.Clamp(opt.Display)
	if opt.FPS <= 0 {
		opt.FPS = 30
	}
	// ⚠️ 绝不设置 ShowsCursor：会强制回退 GDI（4.8ms → 58.3ms，见 VERIFY.md）
	sopt := screencapture.Options{FPS: float64(opt.FPS)}
	st, err := screencapture.CaptureDisplay(ctx, opt.Display.raw, sopt)
	if err != nil {
		return nil, err
	}
	s := &Source{
		opt:     opt,
		stream:  st,
		region:  region,
		bufs:    [2][]byte{make([]byte, region.W*region.H*4), make([]byte, region.W*region.H*4)},
		started: time.Now(),
	}
	return s, nil
}

// Region 返回实际生效的采集区域（已 clamp 与偶数对齐）。
func (s *Source) Region() Rect { return s.region }

// SetRegion 热切换采集区域。
//
// 这是 PLAN 3.5-B 要求的「无感切换」：区域变化只影响裁切矩形，
// 不重建底层采集流、不重建编码器（前提是输出分辨率不变）。
// 若新区域尺寸与旧区域不同，缓冲区会在下一帧自动重新分配。
func (s *Source) SetRegion(r Rect) {
	s.region = r.Clamp(s.opt.Display)
	n := s.region.W * s.region.H * 4
	for i := range s.bufs {
		if cap(s.bufs[i]) < n {
			s.bufs[i] = make([]byte, n)
		}
	}
}

// Backend 返回实际使用的采集后端（duplication / gdi）。
func (s *Source) Backend() string { return s.stream.Backend().String() }

// Note 返回库给出的后端说明（例如降级原因）。
func (s *Source) Note() string { return s.stream.Note() }

// Err 返回采集流的终态错误（例如 UAC/锁屏导致的访问丢失）。
func (s *Source) Err() error { return s.stream.Err() }

// Close 释放采集会话。
func (s *Source) Close() error { return s.stream.Close() }

// Stats 是采集侧的运行时统计。
type Stats struct {
	Frames      uint64
	CursorDraws uint64
	FPS         float64
	// MeanCapture 是总耗时均值（等待 + 处理）。桌面静止时会虚高，别当 CPU 开销看。
	MeanCapture time.Duration
	// MeanWait 是阻塞等待桌面变化的均值（CPU 空闲时间）。
	MeanWait time.Duration
	// MeanProc 是取帧后处理（裁切 + 光标）的均值，这是真实 CPU 开销。
	MeanProc time.Duration
	Backend  string
}

// Stats 返回当前统计。
func (s *Source) Stats() Stats {
	el := time.Since(s.started).Seconds()
	fps := 0.0
	if el > 0 {
		fps = float64(s.frames) / el
	}
	mean, mw, mp := time.Duration(0), time.Duration(0), time.Duration(0)
	if s.frames > 0 {
		d := time.Duration(s.frames)
		mean = s.capture / d
		mw = s.wait / d
		mp = s.proc / d
	}
	return Stats{
		Frames:      s.frames,
		CursorDraws: s.draws,
		FPS:         fps,
		MeanCapture: mean,
		MeanWait:    mw,
		MeanProc:    mp,
		Backend:     s.stream.Backend().String(),
	}
}

// Last 返回最近一次成功采集的帧（用于静止时不至于让下游断流，P0-4 心跳）。
// 返回的是内部缓冲，调用方不得修改。
func (s *Source) Last() Frame { return s.last }

// WaitFrame 等待下一帧。ctx 超时时返回错误（底层是变化驱动，
// 桌面静止时可能长时间不出帧 —— 这是预期行为，不是故障）。
func (s *Source) WaitFrame(ctx context.Context) (Frame, error) {
	t0 := time.Now()
	f, err := s.stream.WaitFrame(ctx)
	waited := time.Since(t0) // 阻塞等待，CPU 空闲
	if err != nil {
		return Frame{}, err
	}
	if !f.Valid() {
		return Frame{}, errNoFrame
	}
	tp := time.Now() // 开始真正的 CPU 处理
	// 分辨率中途变化（换屏/改分辨率）时重新 clamp 区域
	region := s.region
	if f.Width != s.opt.Display.W || f.Height != s.opt.Display.H {
		region = region.Clamp(Display{W: f.Width, H: f.Height})
	}
	if region.X+region.W > f.Width || region.Y+region.H > f.Height {
		region = region.Clamp(Display{W: f.Width, H: f.Height})
	}
	if region.Empty() {
		return Frame{}, errNoFrame
	}
	n := region.W * region.H * 4
	idx := s.idx
	if cap(s.bufs[idx]) < n {
		s.bufs[idx] = make([]byte, n)
	}
	buf := s.bufs[idx][:n]
	s.idx = 1 - idx // 下一帧写另一块，本块得以保留给 Last()

	cropBGRA(buf, f.Pix, f.Stride, region.X, region.Y, region.W, region.H)

	out := Frame{
		Pix: buf,
		W:   region.W,
		H:   region.H,
		Seq: f.Seq,
		TS:  time.Now(),
	}

	if s.opt.Cursor {
		// 光标位置换算：区域左上角对应的虚拟屏幕坐标
		originX := s.opt.Display.X + region.X
		originY := s.opt.Display.Y + region.Y
		DrawCursor(out.Pix, out.W, out.H, originX, originY)
		s.draws++
	}

	processed := time.Since(tp)
	capture := time.Since(t0)
	out.WaitMs = float64(waited.Microseconds()) / 1000.0
	out.ProcMs = float64(processed.Microseconds()) / 1000.0
	out.CaptureMs = float64(capture.Microseconds()) / 1000.0

	s.frames++
	s.capture += capture
	s.wait += waited
	s.proc += processed
	// 心跳兜底（P0-4）：直接引用刚写完的这块缓冲，下一帧会写另一块。
	s.last = out
	s.region = region
	return out, nil
}
