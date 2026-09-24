package codec

import (
	"image"
	"runtime"
	"sync"
)

// I420 布局：Y(w*h) + Cb(w*h/4) + Cr(w*h/4)，与阶段 0 实测用过的一致。
//
// 注意：这里的并行化是硬需求。阶段 0 实测单线程 BGRA→I420 在 2K 上要
// 26.14ms（吃掉整个 30fps 预算），20 路并行后降到 4.29ms（6 倍加速）。

// BGRAtoI420 把紧凑 BGRA 转换为 I420。
// workers<=0 时自动取 NumCPU。
//
// ⚠️ 色彩范围必须两端一致：这里用 **BT.601 full-range**（Y/Cb/Cr 均 0~255），
// 因为 JPEG/JFIF 就是 full-range。曾经这里写成 limited-range（带 +16 偏移）
// 而解码端按 full-range 反变换，结果是系统性偏色，PSNR 只有 23 dB，
// 且"提高质量几乎无改善"—— 那个现象就是范围不匹配的指纹。
func BGRAtoI420(dst, src []byte, w, h, workers int) {
	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	ySize := w * h
	cw, ch := w/2, h/2
	cSize := cw * ch
	y := dst[:ySize]
	cb := dst[ySize : ySize+cSize]
	cr := dst[ySize+cSize : ySize+2*cSize]

	if workers > h {
		workers = h
	}
	var wg sync.WaitGroup
	// 亮度：按行分段
	for k := 0; k < workers; k++ {
		y0, y1 := k*h/workers, (k+1)*h/workers
		if y0 >= y1 {
			continue
		}
		wg.Add(1)
		go func(y0, y1 int) {
			defer wg.Done()
			for j := y0; j < y1; j++ {
				row := j * w * 4
				dstRow := j * w
				for i := 0; i < w; i++ {
					p := row + i*4
					b, g, r := int(src[p]), int(src[p+1]), int(src[p+2])
					// BT.601 full-range
					y[dstRow+i] = uint8((77*r + 150*g + 29*b + 128) >> 8)
				}
			}
		}(y0, y1)
	}
	// 色度：2x2 取样，按色度行分段
	cw2 := workers
	if cw2 > ch {
		cw2 = ch
	}
	for k := 0; k < cw2; k++ {
		j0, j1 := k*ch/cw2, (k+1)*ch/cw2
		if j0 >= j1 {
			continue
		}
		wg.Add(1)
		go func(j0, j1 int) {
			defer wg.Done()
			for j := j0; j < j1; j++ {
				for i := 0; i < cw; i++ {
					var r, g, b int
					for dy := 0; dy < 2; dy++ {
						base := ((j*2+dy)*w + i*2) * 4
						b += int(src[base])
						g += int(src[base+1])
						r += int(src[base+2])
						b += int(src[base+4])
						g += int(src[base+5])
						r += int(src[base+6])
					}
					r, g, b = r>>2, g>>2, b>>2
					ci := j*cw + i
					// BT.601 full-range 色度（以 128 为零点）
					cb[ci] = uint8(((-43*r - 85*g + 128*b + 128) >> 8) + 128)
					cr[ci] = uint8(((128*r - 107*g - 21*b + 128) >> 8) + 128)
				}
			}
		}(j0, j1)
	}
	wg.Wait()
}

// I420Image 把 I420 缓冲包装成 *image.YCbCr（零拷贝）。
func I420Image(i420 []byte, w, h int) *image.YCbCr {
	ySize := w * h
	cSize := ySize / 4
	return &image.YCbCr{
		Y:              i420[:ySize:ySize],
		Cb:             i420[ySize : ySize+cSize : ySize+cSize],
		Cr:             i420[ySize+cSize : ySize+2*cSize : ySize+2*cSize],
		YStride:        w,
		CStride:        w / 2,
		SubsampleRatio: image.YCbCrSubsampleRatio420,
		Rect:           image.Rect(0, 0, w, h),
	}
}

// YCbCrToNRGBA 把 *image.YCbCr 的指定行区间 [y0,y1) 并行写入 dst（NRGBA，行宽 dw）。
// 用于观看端把解码后的条带拼进渲染位图。
//
// ⚠️ 两个易错点：
//  1. CbAt/CrAt 返回的是 color.Color（接口），直接断言 uint8 会 panic；
//     必须自己按 CStride 索引，性能也更好。
//  2. BT.601 full-range 系数要匹配 Y 的放大倍数：这里把 Y 放大 256 倍后，
//     对应系数是 359/88/183/454，不能沿用 "Y 不放大" 那套 229/45/91/283。
func YCbCrToNRGBA(dst []byte, dw int, src *image.YCbCr, y0, y1, workers int) {
	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	w := src.Rect.Dx()
	if y1 > src.Rect.Dy() {
		y1 = src.Rect.Dy()
	}
	if y1 <= y0 {
		return
	}
	var wg sync.WaitGroup
	if workers > (y1 - y0) {
		workers = y1 - y0
	}
	for k := 0; k < workers; k++ {
		a := y0 + k*(y1-y0)/workers
		b := y0 + (k+1)*(y1-y0)/workers
		if a >= b {
			continue
		}
		wg.Add(1)
		go func(a, b int) {
			defer wg.Done()
			for j := a; j < b; j++ {
				// 注意用 Rect.Min 偏移：SubImage 后 YOffset 已含偏移，
				// 这里统一用 YOffset/CStride 计算，避免坐标系搞错。
				yOff := src.YOffset(0, j)
				cOff := src.COffset(0, j)
				drow := j * dw * 4
				for i := 0; i < w; i++ {
					yy := int(src.Y[yOff+i]) << 8
					ci := cOff + i/2
					cb := int(src.Cb[ci]) - 128
					cr := int(src.Cr[ci]) - 128
					r := (yy + 359*cr) >> 8
					g := (yy - 88*cb - 183*cr) >> 8
					bl := (yy + 454*cb) >> 8
					p := drow + i*4
					dst[p], dst[p+1], dst[p+2], dst[p+3] = clamp8(r), clamp8(g), clamp8(bl), 255
				}
			}
		}(a, b)
	}
	wg.Wait()
}

func clamp8(v int) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}
