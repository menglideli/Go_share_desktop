// P1-5 的盒式降采样：自适应降分辨率时，把采集帧缩小后再编码。
//
// 为什么不用"直接缩小采集区域"：区域是**裁剪**不是缩放 —— 缩区域等于少看一块画面。
// 这里按整数比例做盒式平均（3×3→2×2、2×2→1×1），实现简单、无浮点、画质足够
// （观众端本来就会再缩放一次来适配窗口）。
package codec

import "errors"

// DownscaleBGRA 把紧凑 BGRA 按 num/den 缩小（num < den），返回新缓冲与尺寸。
//
// 输出宽高会向下取偶数：4:2:0 的 JPEG 对奇数尺寸不友好，且观众端画布重建时
// 尺寸变化本来就要发全量帧，取偶不会引入额外复杂度。
func DownscaleBGRA(src []byte, w, h, num, den int) ([]byte, int, int, error) {
	if num <= 0 || den <= 0 || num >= den {
		return nil, 0, 0, errors.New("codec: 缩放比例必须是 0 < num < den")
	}
	if w <= 0 || h <= 0 || len(src) < w*h*4 {
		return nil, 0, 0, errors.New("codec: 缩放输入无效")
	}
	ow, oh := w*num/den, h*num/den
	ow &^= 1
	oh &^= 1
	if ow < 2 || oh < 2 {
		return nil, 0, 0, errors.New("codec: 缩放后尺寸过小")
	}
	dst := make([]byte, ow*oh*4)
	for y := 0; y < oh; y++ {
		y0 := y * h / oh
		y1 := (y + 1) * h / oh
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < ow; x++ {
			x0 := x * w / ow
			x1 := (x + 1) * w / ow
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var sr, sg, sb, sa, n int
			for yy := y0; yy < y1; yy++ {
				row := yy * w * 4
				for xx := x0; xx < x1; xx++ {
					i := row + xx*4
					sb += int(src[i])
					sg += int(src[i+1])
					sr += int(src[i+2])
					sa += int(src[i+3])
					n++
				}
			}
			o := (y*ow + x) * 4
			dst[o] = byte(sb / n)
			dst[o+1] = byte(sg / n)
			dst[o+2] = byte(sr / n)
			dst[o+3] = byte(sa / n)
		}
	}
	return dst, ow, oh, nil
}
