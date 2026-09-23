package ui

import (
	"image"
	"runtime"
	"sync"

	"goshare/internal/capture"
)

// View 是把采集帧转换后供渲染使用的位图。
// Pix 复用，避免每帧重新分配。
type View struct {
	img    *image.NRGBA
	srcW   int
	srcH   int
	scale  int
	stamp  uint64
	mu     sync.RWMutex
	buf    []byte // 采集帧的深拷贝（BGRA，紧凑）
	bufW   int
	bufH   int
	hasBuf bool
}

// NewView 创建视图，scale 为降采样倍率（>=1）。
func NewView(w, h, scale int) *View {
	if scale < 1 {
		scale = 1
	}
	dw, dh := (w+scale-1)/scale, (h+scale-1)/scale
	if dw < 1 {
		dw = 1
	}
	if dh < 1 {
		dh = 1
	}
	return &View{
		img:   image.NewNRGBA(image.Rect(0, 0, dw, dh)),
		srcW:  w,
		srcH:  h,
		scale: scale,
	}
}

// Image 返回可渲染位图。
func (v *View) Image() *image.NRGBA { return v.img }

// SrcSize 返回最近一帧采集帧的原始尺寸（未降采样）。
func (v *View) SrcSize() (int, int) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.bufW, v.bufH
}

// Stamp 返回已渲染的帧序号，用于判断是否有新内容。
func (v *View) Stamp() uint64 {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.stamp
}

// Update 接收一帧采集输出，深拷贝后异步转换。
// 采集帧的 Pix 是复用缓冲，必须拷出来。
func (v *View) Update(f capture.Frame) {
	if f.Empty() {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if cap(v.buf) < len(f.Pix) {
		v.buf = make([]byte, len(f.Pix))
	}
	v.buf = v.buf[:len(f.Pix)]
	copy(v.buf, f.Pix)
	v.bufW, v.bufH = f.W, f.H
	v.hasBuf = true
	v.stamp++
}

// Render 把缓冲中的采集帧转换到渲染位图。
func (v *View) Render() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.hasBuf {
		return
	}
	srcW, srcH, scale := v.bufW, v.bufH, v.scale
	dw, dh := (srcW+scale-1)/scale, (srcH+scale-1)/scale
	if dw < 1 || dh < 1 {
		return
	}
	if v.img.Bounds().Dx() != dw || v.img.Bounds().Dy() != dh {
		v.img = image.NewNRGBA(image.Rect(0, 0, dw, dh))
	}
	scaleBGRAtoNRGBA(v.img.Pix, v.buf, srcW, srcH, scale, dw, dh)
}

// scaleBGRAtoNRGBA 把紧凑 BGRA 按整数倍降采样并转为 NRGBA（并行按行）。
func scaleBGRAtoNRGBA(dst, src []byte, srcW, srcH, scale, dw, dh int) {
	workers := runtime.NumCPU()
	if workers > dh {
		workers = dh
	}
	rows := (dh + workers - 1) / workers
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		y0 := w * rows
		y1 := y0 + rows
		if y0 >= dh {
			break
		}
		if y1 > dh {
			y1 = dh
		}
		wg.Add(1)
		go func(y0, y1 int) {
			defer wg.Done()
			for dy := y0; dy < y1; dy++ {
				sy := dy * scale
				drow := dy * dw * 4
				srow := sy * srcW * 4
				for dx := 0; dx < dw; dx++ {
					sx := dx * scale
					si := srow + sx*4
					di := drow + dx*4
					dst[di] = src[si+2]   // R ← B
					dst[di+1] = src[si+1] // G
					dst[di+2] = src[si]   // B ← R
					dst[di+3] = 255
				}
			}
		}(y0, y1)
	}
	wg.Wait()
}

// FitScale 计算让源尺寸适配目标显示区所需的整数降采样倍率。
// 只在源明显大于目标时降采样（scale=1 表示原尺寸）。
func FitScale(srcW, srcH, dstW, dstH int) int {
	if dstW <= 0 || dstH <= 0 || srcW <= 0 || srcH <= 0 {
		return 1
	}
	sx := srcW / dstW
	sy := srcH / dstH
	s := sx
	if sy < s {
		s = sy
	}
	if s < 1 {
		s = 1
	}
	return s
}
