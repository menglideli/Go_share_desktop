package tray

// 图标绘制与平台无关，单独放在无 build tag 的文件里：
// Windows 托盘（BGRA DIB）与 cmd/icon（生成 PNG 给 exe 资源）共用一份。

// PaintIcon 在 size×size 的 4 字节/像素缓冲上画应用图标：
// 主题蓝圆形底，中央白色显示器（圆角屏 + 支架 + 底座），屏幕里一个向上的分享箭头。
// bgra=true 时按 BGRA 字节序写（Windows DIB），false 按 RGBA（image.NRGBA）。
// px 长度必须 ≥ size*size*4，调用方保证。
func PaintIcon(px []byte, size int, bgra bool) {
	s := float64(size)
	cx, cy := s/2, s/2
	r := s/2 - 0.5
	// 主题蓝 #3B82F6
	blR, blG, blB := byte(0x3B), byte(0x82), byte(0xF6)
	wh := byte(255)

	setPx := func(x, y int, rr, gg, bb, aa byte) {
		i := (y*size + x) * 4
		if bgra {
			px[i+0] = bb
			px[i+1] = gg
			px[i+2] = rr
		} else {
			px[i+0] = rr
			px[i+1] = gg
			px[i+2] = bb
		}
		px[i+3] = aa
	}

	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			dx, dy := float64(x)+0.5-cx, float64(y)+0.5-cy
			if dx*dx+dy*dy > r*r {
				continue // 圆外全透明
			}
			setPx(x, y, blR, blG, blB, 255)
		}
	}

	// 显示器屏幕：圆角矩形（宽 62%，高 40%，居中偏上）
	sw, sh := s*0.62, s*0.40
	sx0, sy0 := cx-sw/2, cy-sh/2-s*0.06
	sx1, sy1 := sx0+sw, sy0+sh
	cr := s * 0.06 // 圆角半径
	inRect := func(fx, fy float64) bool {
		if fx < sx0 || fx >= sx1 || fy < sy0 || fy >= sy1 {
			return false
		}
		// 四角圆角
		for _, c := range [4][2]float64{{sx0 + cr, sy0 + cr}, {sx1 - cr, sy0 + cr}, {sx0 + cr, sy1 - cr}, {sx1 - cr, sy1 - cr}} {
			if (fx < sx0+cr || fx >= sx1-cr) && (fy < sy0+cr || fy >= sy1-cr) {
				ddx, ddy := fx-c[0], fy-c[1]
				if ddx*ddx+ddy*ddy > cr*cr {
					return false
				}
			}
		}
		return true
	}
	// 底座
	bw, bh := s*0.30, s*0.055
	bx0, by0 := cx-bw/2, sy1+s*0.07
	// 支架
	nw := s * 0.07
	nx0, ny0, ny1 := cx-nw/2, sy1, by0

	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			fx, fy := float64(x)+0.5, float64(y)+0.5
			dx, dy := fx-cx, fy-cy
			if dx*dx+dy*dy > r*r {
				continue
			}
			switch {
			case inRect(fx, fy):
				setPx(x, y, wh, wh, wh, 255)
			case fx >= nx0 && fx < nx0+nw && fy >= ny0 && fy < ny1:
				setPx(x, y, wh, wh, wh, 255)
			case fx >= bx0 && fx < bx0+bw && fy >= by0 && fy < by0+bh:
				setPx(x, y, wh, wh, wh, 255)
			}
		}
	}

	// 屏幕内画一个向上的分享箭头（白屏上用主题蓝反色画）。
	pad := s * 0.10
	ax0, ay0 := sx0+pad, sy0+pad
	ax1, ay1 := sx1-pad, sy1-pad*1.4
	aw := (ax1 - ax0) * 0.28 // 箭杆宽
	tipY := ay0
	shaftTop := ay0 + (ay1-ay0)*0.38
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			fx, fy := float64(x)+0.5, float64(y)+0.5
			if !inRect(fx, fy) {
				continue
			}
			mid := (ax0 + ax1) / 2
			inShaft := fx >= mid-aw/2 && fx < mid+aw/2 && fy >= shaftTop && fy < ay1
			// 箭头三角：从 (mid, tipY) 张开到 shaftTop 高度处与 ax0..ax1 同宽
			var inHead bool
			if fy >= tipY && fy < shaftTop {
				half := (fy - tipY) / (shaftTop - tipY) * (ax1 - ax0) / 2
				inHead = fx >= mid-half && fx < mid+half
			}
			if inShaft || inHead {
				setPx(x, y, blR, blG, blB, 255)
			}
		}
	}
}
