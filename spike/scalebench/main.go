// scalebench v2：在 v1 结论（RGBA 域缩放不可用）基础上，
// 实测并行化与"先降采样后转换"能否把流水线压进 33ms/frame 预算。
package main

import (
	"fmt"
	"math/rand"
	"runtime"
	"strings"
	"sync"
	"time"
)

// ---------- 测试图像 ----------

func newBGRA(w, h int) []byte {
	pix := make([]byte, w*h*4)
	r := rand.New(rand.NewSource(42))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := (y*w + x) * 4
			base := uint8((x*255)/w) ^ uint8((y*255)/h)
			v := base
			if x%3 == 0 || y%5 == 0 {
				v = 255 - base
			}
			pix[i+0] = v
			pix[i+1] = uint8(int(v)*3/4) + uint8(r.Intn(8))
			pix[i+2] = uint8(int(v)/2) + uint8(r.Intn(8))
			pix[i+3] = 255
		}
	}
	return pix
}

// ---------- BGRA -> I420 ----------

func bgraToI420(dst, src []byte, w, h int) { bgraToI420N(dst, src, w, h, 1) }

func bgraToI420N(dst, src []byte, w, h, workers int) {
	ySize := w * h
	cw, ch := w/2, h/2
	cSize := cw * ch
	y := dst[:ySize]
	cb := dst[ySize : ySize+cSize]
	cr := dst[ySize+cSize : ySize+2*cSize]

	var wg sync.WaitGroup
	// Y：按行分片
	for k := 0; k < workers; k++ {
		y0 := k * h / workers
		y1 := (k + 1) * h / workers
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
					b := int(src[p])
					g := int(src[p+1])
					r := int(src[p+2])
					y[dstRow+i] = uint8((66*r+129*g+25*b+128)>>8 + 16)
				}
			}
		}(y0, y1)
	}
	// 色度：按行分片
	for k := 0; k < workers; k++ {
		j0 := k * ch / workers
		j1 := (k + 1) * ch / workers
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
						for dx := 0; dx < 2; dx++ {
							p := ((j*2+dy)*w + (i*2+dx)) * 4
							b += int(src[p])
							g += int(src[p+1])
							r += int(src[p+2])
						}
					}
					r >>= 2
					g >>= 2
					b >>= 2
					ci := j*cw + i
					cb[ci] = uint8((-38*r-74*g+112*b+128)>>8 + 128)
					cr[ci] = uint8((112*r-94*g-18*b+128)>>8 + 128)
				}
			}
		}(j0, j1)
	}
	wg.Wait()
}

// ---------- BGRA 域 box 2:1 降采样 ----------

func bgraBox2x1N(dst, src []byte, w, h, workers int) {
	dw, dh := w/2, h/2
	var wg sync.WaitGroup
	for k := 0; k < workers; k++ {
		j0 := k * dh / workers
		j1 := (k + 1) * dh / workers
		if j0 >= j1 {
			continue
		}
		wg.Add(1)
		go func(j0, j1 int) {
			defer wg.Done()
			for j := j0; j < j1; j++ {
				s0 := (j * 2) * w * 4
				s1 := (j*2 + 1) * w * 4
				d := j * dw * 4
				for i := 0; i < dw; i++ {
					a := s0 + i*8
					b := s0 + i*8 + 4
					c := s1 + i*8
					e := s1 + i*8 + 4
					for ch := 0; ch < 4; ch++ {
						dst[d+i*4+ch] = uint8((int(src[a+ch]) + int(src[b+ch]) + int(src[c+ch]) + int(src[e+ch])) >> 2)
					}
				}
			}
		}(j0, j1)
	}
	wg.Wait()
}

// ---------- I420 域缩放 ----------

func scaleI420Box2x1N(dst, src []byte, w, h, workers int) {
	dw, dh := w/2, h/2
	var wg sync.WaitGroup
	for k := 0; k < workers; k++ {
		j0 := k * dh / workers
		j1 := (k + 1) * dh / workers
		if j0 >= j1 {
			continue
		}
		wg.Add(1)
		go func(j0, j1 int) {
			defer wg.Done()
			for j := j0; j < j1; j++ {
				for i := 0; i < dw; i++ {
					a := src[(j*2)*w+i*2]
					b := src[(j*2)*w+i*2+1]
					c := src[(j*2+1)*w+i*2]
					d := src[(j*2+1)*w+i*2+1]
					dst[j*dw+i] = uint8((int(a) + int(b) + int(c) + int(d)) >> 2)
				}
			}
		}(j0, j1)
	}
	sw2, sh2 := w/2, h/2
	dw2, dh2 := dw/2, dh/2
	for p := 0; p < 2; p++ {
		sOff := w*h + p*sw2*sh2
		dOff := dw*dh + p*dw2*dh2
		for k := 0; k < workers; k++ {
			j0 := k * dh2 / workers
			j1 := (k + 1) * dh2 / workers
			if j0 >= j1 {
				continue
			}
			wg.Add(1)
			go func(sOff, dOff, j0, j1 int) {
				defer wg.Done()
				for j := j0; j < j1; j++ {
					for i := 0; i < dw2; i++ {
						a := src[sOff+(j*2)*sw2+i*2]
						b := src[sOff+(j*2)*sw2+i*2+1]
						c := src[sOff+(j*2+1)*sw2+i*2]
						d := src[sOff+(j*2+1)*sw2+i*2+1]
						dst[dOff+j*dw2+i] = uint8((int(a) + int(b) + int(c) + int(d)) >> 2)
					}
				}
			}(sOff, dOff, j0, j1)
		}
	}
	wg.Wait()
}

// scaleI420Bilinear 任意比例（整数定点，避免每像素 float 除法）
func scaleI420BilinearN(dst, src []byte, w, h, dw, dh, workers int) {
	scalePlane := func(dOff, sOff, sw, sh, tw, th int) {
		var wg sync.WaitGroup
		xstep := (sw << 16) / tw
		ystep := (sh << 16) / th
		for k := 0; k < workers; k++ {
			j0 := k * th / workers
			j1 := (k + 1) * th / workers
			if j0 >= j1 {
				continue
			}
			wg.Add(1)
			go func(j0, j1 int) {
				defer wg.Done()
				for j := j0; j < j1; j++ {
					fy := j * ystep
					y0 := fy >> 16
					wy := (fy >> 8) & 0xFF
					y1 := y0 + 1
					if y1 >= sh {
						y1 = sh - 1
					}
					r0 := sOff + y0*sw
					r1 := sOff + y1*sw
					dr := dOff + j*tw
					for i := 0; i < tw; i++ {
						fx := i * xstep
						x0 := fx >> 16
						wx := (fx >> 8) & 0xFF
						x1 := x0 + 1
						if x1 >= sw {
							x1 = sw - 1
						}
						a := int(src[r0+x0])
						b := int(src[r0+x1])
						c := int(src[r1+x0])
						d := int(src[r1+x1])
						t0 := a + ((b-a)*wx)>>8
						t1 := c + ((d-c)*wx)>>8
						dst[dr+i] = uint8(t0 + ((t1-t0)*wy)>>8)
					}
				}
			}(j0, j1)
		}
		wg.Wait()
	}
	scalePlane(0, 0, w, h, dw, dh)
	sw2, sh2 := w/2, h/2
	dw2, dh2 := dw/2, dh/2
	scalePlane(dw*dh, w*h, sw2, sh2, dw2, dh2)
	scalePlane(dw*dh+dw2*dh2, w*h+sw2*sh2, sw2, sh2, dw2, dh2)
}

// ---------- 计时 ----------

func bench(name string, n int, fn func()) {
	fn()
	fn()
	start := time.Now()
	for i := 0; i < n; i++ {
		fn()
	}
	d := time.Since(start) / time.Duration(n)
	ms := float64(d.Microseconds()) / 1000.0
	fps := 1000.0 / ms
	mark := "  "
	if ms > 33.3 {
		mark = "!!"
	} else if ms > 16.6 {
		mark = " !"
	}
	fmt.Printf("%s%-50s %8.2f ms   %6.1f fps\n", mark, name, ms, fps)
}

func main() {
	const SW, SH = 2880, 1800
	const DW1, DH1 = 1440, 900   // 2:1 整数倍（16:10 保持）
	const DW2, DH2 = 1728, 1080  // 适配 1080p 高度（16:10 保持）

	ncpu := runtime.NumCPU()
	fmt.Printf("\nCPU 逻辑核心: %d   （30fps 预算 33.3ms / 60fps 预算 16.6ms；!! 超出，! 偏紧）\n", ncpu)

	src := newBGRA(SW, SH)
	halfBGRA := make([]byte, DW1*DH1*4)
	i420Full := make([]byte, SW*SH*3/2)
	i420Half := make([]byte, DW1*DH1*3/2)
	i420D2 := make([]byte, DW2*DH2*3/2)

	fmt.Printf("\n=== 1. BGRA -> I420 转换 %dx%d（源分辨率，必做） ===\n", SW, SH)
	bench("单线程", 20, func() { bgraToI420N(i420Full, src, SW, SH, 1) })
	for _, n := range []int{4, 8, 16, ncpu} {
		if n > ncpu {
			n = ncpu
		}
		bench(fmt.Sprintf("并行 workers=%d", n), 20, func() { bgraToI420N(i420Full, src, SW, SH, n) })
	}

	fmt.Printf("\n=== 2. I420 域缩放 ===\n")
	bench(fmt.Sprintf("box 2:1 -> %dx%d (单线程)", DW1, DH1), 20, func() { scaleI420Box2x1N(i420Half, i420Full, SW, SH, 1) })
	bench(fmt.Sprintf("box 2:1 -> %dx%d (workers=%d)", DW1, DH1, ncpu), 20, func() { scaleI420Box2x1N(i420Half, i420Full, SW, SH, ncpu) })
	bench(fmt.Sprintf("bilinear -> %dx%d (单线程)", DW2, DH2), 20, func() { scaleI420BilinearN(i420D2, i420Full, SW, SH, DW2, DH2, 1) })
	bench(fmt.Sprintf("bilinear -> %dx%d (workers=%d)", DW2, DH2, ncpu), 20, func() { scaleI420BilinearN(i420D2, i420Full, SW, SH, DW2, DH2, ncpu) })

	fmt.Printf("\n=== 3. BGRA 域 box 降采样 ===\n")
	bench(fmt.Sprintf("bgraBox2x1 %dx%d->%dx%d (workers=%d)", SW, SH, DW1, DH1, ncpu), 20, func() { bgraBox2x1N(halfBGRA, src, SW, SH, ncpu) })

	fmt.Printf("\n=== 4. 完整流水线对比（源 %dx%d） ===\n", SW, SH)
	bench(fmt.Sprintf("[A] 原生直发：I420(%dx%d)", SW, SH), 20, func() {
		bgraToI420N(i420Full, src, SW, SH, ncpu)
	})
	bench(fmt.Sprintf("[B] 半分辨率：I420全量 -> box2:1 (%dx%d)", DW1, DH1), 20, func() {
		bgraToI420N(i420Full, src, SW, SH, ncpu)
		scaleI420Box2x1N(i420Half, i420Full, SW, SH, ncpu)
	})
	bench(fmt.Sprintf("[C] 半分辨率：BGRA box2:1 先降 -> I420 (%dx%d)", DW1, DH1), 20, func() {
		bgraBox2x1N(halfBGRA, src, SW, SH, ncpu)
		bgraToI420N(i420Half, halfBGRA, DW1, DH1, ncpu)
	})
	bench(fmt.Sprintf("[D] 1080高度：I420全量 -> bilinear (%dx%d)", DW2, DH2), 20, func() {
		bgraToI420N(i420Full, src, SW, SH, ncpu)
		scaleI420BilinearN(i420D2, i420Full, SW, SH, DW2, DH2, ncpu)
	})

	fmt.Printf("\n%s\n", strings.Repeat("-", 68))
	fmt.Printf("注：以上仅含 转换+缩放，未含 采集、H.264 编码、网络、解码、渲染。\n\n")
}
