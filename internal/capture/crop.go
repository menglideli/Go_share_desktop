package capture

import (
	"runtime"
	"sync"
	"time"

	"github.com/go-mswin/screencapture"
)

// Crop 从原始采集帧中裁出区域，返回紧凑 BGRA。
//
// 导出它的目的是让诊断工具能用「同一个原始帧」同时做整帧拷贝与区域裁切，
// 逐像素比对以验证 Stride 索引正确（换机器时 stride 未必等于 Width*4）。
func Crop(f screencapture.Frame, r Rect) (Frame, error) {
	if !f.Valid() {
		return Frame{}, errNoFrame
	}
	r = r.Clamp(Display{W: f.Width, H: f.Height})
	if r.Empty() {
		return Frame{}, errNoFrame
	}
	dst := make([]byte, r.W*r.H*4)
	cropBGRA(dst, f.Pix, f.Stride, r.X, r.Y, r.W, r.H)
	return Frame{Pix: dst, W: r.W, H: r.H, Seq: f.Seq, TS: time.Now()}, nil
}

// TightCopy 把原始帧（含行 padding）拷成紧凑 BGRA，用于基准比对。
func TightCopy(f screencapture.Frame) (Frame, error) {
	if !f.Valid() {
		return Frame{}, errNoFrame
	}
	dst := make([]byte, f.Width*f.Height*4)
	for y := 0; y < f.Height; y++ {
		s0 := y * f.Stride
		d0 := y * f.Width * 4
		copy(dst[d0:d0+f.Width*4], f.Pix[s0:s0+f.Width*4])
	}
	return Frame{Pix: dst, W: f.Width, H: f.Height, Seq: f.Seq, TS: time.Now()}, nil
}

// cropBGRA 把源帧中 (rx,ry) 起点、rw×rh 大小的区域复制成「紧凑」BGRA
// （无行padding，dst 长度恒为 rw*rh*4）。
//
// 必须按 srcStride 索引：本机 Stride == Width*4，但其他机器可能对齐到 256 字节。
// 区域较大时按行块并行 —— 阶段 0 实测单线程内存搬运在 2K 上会吃掉数毫秒预算。
func cropBGRA(dst, src []byte, srcStride, rx, ry, rw, rh int) {
	if rw <= 0 || rh <= 0 {
		return
	}
	dstStride := rw * 4
	// 小区域直接单线程，避免 goroutine 开销盖过收益
	if rw*rh < 256*256 {
		for y := 0; y < rh; y++ {
			s0 := (ry+y)*srcStride + rx*4
			d0 := y * dstStride
			copy(dst[d0:d0+dstStride], src[s0:s0+dstStride])
		}
		return
	}
	workers := runtime.NumCPU()
	if workers > rh {
		workers = rh
	}
	var wg sync.WaitGroup
	rows := (rh + workers - 1) / workers
	for w := 0; w < workers; w++ {
		y0 := w * rows
		y1 := y0 + rows
		if y0 >= rh {
			break
		}
		if y1 > rh {
			y1 = rh
		}
		wg.Add(1)
		go func(y0, y1 int) {
			defer wg.Done()
			for y := y0; y < y1; y++ {
				s0 := (ry+y)*srcStride + rx*4
				d0 := y * dstStride
				copy(dst[d0:d0+dstStride], src[s0:s0+dstStride])
			}
		}(y0, y1)
	}
	wg.Wait()
}

// blitBGRA 把 src（紧凑 BGRA，sw×sh）以 src 全尺寸绘制到 dst 的 (dx,dy) 处。
// 用于输出缓冲区比采集区域大的场景（例如区域尺寸中途变化）。
func blitBGRA(dst []byte, dw, dh, dx, dy int, src []byte, sw, sh int) {
	if sw <= 0 || sh <= 0 {
		return
	}
	if dx+sw > dw {
		sw = dw - dx
	}
	if dy+sh > dh {
		sh = dh - dy
	}
	if sw <= 0 || sh <= 0 {
		return
	}
	dstStride := dw * 4
	srcStride := sw * 4
	for y := 0; y < sh; y++ {
		d0 := (dy+y)*dstStride + dx*4
		s0 := y * srcStride
		copy(dst[d0:d0+srcStride], src[s0:s0+srcStride])
	}
}
