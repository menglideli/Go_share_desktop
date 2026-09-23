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
	// CaptureMs 是底层采集一帧的耗时（库内部统计口径之外的一次实测），
	// 用于诊断 HUD，不影响数据正确性。
	CaptureMs float64
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
	opt     Options
	stream  *screencapture.Stream
	region  Rect
	buf     []byte
	last    Frame
	frames  uint64
	draws   uint64
	capture time.Duration
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
		buf:     make([]byte, region.W*region.H*4),
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
	if cap(s.buf) < s.region.W*s.region.H*4 {
		s.buf = make([]byte, s.region.W*s.region.H*4)
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
	Frames    uint64
	CursorDraws uint64
	FPS       float64
	MeanCapture time.Duration
	Backend   string
}

// Stats 返回当前统计。
func (s *Source) Stats() Stats {
	el := time.Since(s.started).Seconds()
	fps := 0.0
	if el > 0 {
		fps = float64(s.frames) / el
	}
	mean := time.Duration(0)
	if s.frames > 0 {
		mean = s.capture / time.Duration(s.frames)
	}
	return Stats{
		Frames:      s.frames,
		CursorDraws: s.draws,
		FPS:         fps,
		MeanCapture: mean,
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
	capture := time.Since(t0)
	if err != nil {
		return Frame{}, err
	}
	if !f.Valid() {
		return Frame{}, errNoFrame
	}
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
	if cap(s.buf) < region.W*region.H*4 {
		s.buf = make([]byte, region.W*region.H*4)
	}
	buf := s.buf[:region.W*region.H*4]

	cropBGRA(buf, f.Pix, f.Stride, region.X, region.Y, region.W, region.H)

	out := Frame{
		Pix:       buf,
		W:         region.W,
		H:         region.H,
		Seq:       f.Seq,
		TS:        time.Now(),
		CaptureMs: float64(capture.Microseconds()) / 1000.0,
	}

	if s.opt.Cursor {
		// 光标位置换算：区域左上角对应的虚拟屏幕坐标
		originX := s.opt.Display.X + region.X
		originY := s.opt.Display.Y + region.Y
		DrawCursor(out.Pix, out.W, out.H, originX, originY)
		s.draws++
	}

	s.frames++
	s.capture += capture
	// 保留最近一帧（拷贝，因为 buf 会被下一帧复用）
	s.last = out.Clone()
	s.region = region
	return out, nil
}
