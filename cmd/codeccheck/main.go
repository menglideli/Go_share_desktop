// codeccheck — 阶段 2 · MJPEG 条带编解码自检
//
// 重点不是"能跑通"，而是证伪。核心是第 4 项：
// 增量（dirty tile）编码链累积 30 帧后，必须与"每帧全量"的结果逐像素一致。
// 只要脏检测漏掉任何一个真正变化的条带，画面就会残留旧内容 —— 这个断言能抓到。
package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"math"
	"os"
	"runtime"
	"time"

	"goshare/internal/codec"
)

var (
	pass, fail int
	builder    bytes.Buffer
)

func out(s string) {
	fmt.Println(s)
	builder.WriteString(s + "\n")
}

func check(name string, ok bool, detail string) {
	if ok {
		pass++
		out(fmt.Sprintf("  ✅ %-28s %s", name, detail))
	} else {
		fail++
		out(fmt.Sprintf("  ❌ %-28s %s", name, detail))
	}
}

// 合成桌面：大面积静态背景 + 文本条纹 + 底部色块，模拟真实桌面特征
func makeFrame(w, h int) []byte {
	pix := make([]byte, w*h*4)
	for j := 0; j < h; j++ {
		textRow := (j % 24) < 14
		for i := 0; i < w; i++ {
			var r, g, b uint8 = 245, 245, 245
			switch {
			case textRow && (i%17) < 9:
				r, g, b = 30, 30, 30
			case j > h-140 && i < 520:
				r, g, b = 200, 120, 90
			case j < 70:
				r, g, b = 225, 225, 230
			}
			p := (j*w + i) * 4
			pix[p], pix[p+1], pix[p+2], pix[p+3] = b, g, r, 255 // BGRA
		}
	}
	return pix
}

// mutate 在指定矩形内写入随机内容，模拟局部变化（如打字、鼠标移动）
func mutate(pix []byte, w, h int, x0, y0, x1, y1 int, seed int) {
	for j := y0; j < y1; j++ {
		for i := x0; i < x1; i++ {
			p := (j*w + i) * 4
			v := uint8((i*7 + j*13 + seed*31) & 0xff)
			pix[p], pix[p+1], pix[p+2] = v, uint8(255-int(v)), uint8((int(v)*3)&0xff)
		}
	}
}

// psnr 计算 BGRA 源与 NRGBA 解码结果的峰值信噪比（只看 RGB）
func psnr(src []byte, dst *image.NRGBA, w, h int) float64 {
	sum := 0.0
	n := 0
	for j := 0; j < h; j++ {
		for i := 0; i < w; i++ {
			p := (j*w + i) * 4
			q := j*dst.Stride + i*4
			// src 是 BGRA，dst 是 NRGBA
			dr := int(src[p+2]) - int(dst.Pix[q])
			dg := int(src[p+1]) - int(dst.Pix[q+1])
			db := int(src[p]) - int(dst.Pix[q+2])
			sum += float64(dr*dr + dg*dg + db*db)
			n++
		}
	}
	if sum == 0 {
		return math.Inf(1)
	}
	mse := sum / float64(n*3)
	return 10 * math.Log10(255*255/mse)
}

func main() {
	// 全局看门狗：任何一步挂死都能给出信号，而不是静默卡住
	done := make(chan struct{})
	go func() {
		select {
		case <-done:
		case <-time.After(180 * time.Second):
			fmt.Println("!!! 看门狗超时（180s），强制退出")
			os.Exit(2)
		}
	}()

	out("")
	out("=== GoShare 编码器自检 · 阶段 2 ===")
	out(fmt.Sprintf("CPU 逻辑核心 %d · CGO_ENABLED=0 · %s", runtime.NumCPU(), time.Now().Format("15:04:05")))
	out("")

	w, h := 1920, 1080

	// ---------- 1. 编码 → 解码 → 画质 ----------
	out("[1] 编码/解码往返画质（1080p，q75/85/60）")
	for _, q := range []int{85, 75, 60} {
		enc := codec.NewEncoder(codec.Config{Quality: q, Tiles: runtime.NumCPU(), Dirty: false})
		src := makeFrame(w, h)
		f, err := enc.Encode(src, w, h)
		if err != nil {
			check(fmt.Sprintf("q%d 编码", q), false, err.Error())
			continue
		}
		dec := codec.NewDecoder(0)
		img, err := dec.Decode(f)
		if err != nil {
			check(fmt.Sprintf("q%d 解码", q), false, err.Error())
			continue
		}
		p := psnr(src, img, w, h)
		check(fmt.Sprintf("q%d 画质", q), p >= 28,
			fmt.Sprintf("PSNR %.2f dB · 帧 %d KB · 编码 %.1fms · 解码 %.1fms",
				p, f.Bytes/1024, f.EncodeMs, dec.DecodeMs))
	}

	// ---------- 2. 线格式往返 ----------
	out("")
	out("[2] 线格式 Marshal → Unmarshal 往返")
	{
		enc := codec.NewEncoder(codec.Config{Quality: 75, Tiles: runtime.NumCPU(), Dirty: true})
		src := makeFrame(w, h)
		f, _ := enc.Encode(src, w, h)
		buf, err := f.Marshal()
		if err != nil {
			check("序列化", false, err.Error())
		} else {
			f2, err := codec.UnmarshalFrame(buf)
			same := err == nil && f2.Seq == f.Seq && f2.W == f.W && f2.H == f.H &&
				len(f2.Tiles) == len(f.Tiles) && f2.Full == f.Full && f2.Bytes == f.Bytes
			detail := fmt.Sprintf("%d 字节 → %d 条带", len(buf), len(f2.Tiles))
			if err != nil {
				detail = err.Error()
			}
			check("往返一致", same, detail)
		}
		// 反向变异：截断数据必须报错，而不是静默产出坏帧
		if len(buf) > 40 {
			_, err := codec.UnmarshalFrame(buf[:len(buf)/2])
			check("截断数据被拒绝", err != nil, fmt.Sprintf("err=%v", err))
		}
		// 坏魔数必须被拒绝
		bad := make([]byte, len(buf))
		copy(bad, buf)
		bad[0] = 'X'
		_, err = codec.UnmarshalFrame(bad)
		check("错误魔数被拒绝", err != nil, fmt.Sprintf("err=%v", err))
	}

	// ---------- 3. dirty tile 定位正确性 ----------
	out("")
	out("[3] 脏条带定位（只改画面中部一小块）")
	{
		enc := codec.NewEncoder(codec.Config{Quality: 75, Tiles: 20, Dirty: true})
		src := makeFrame(w, h)
		if _, err := enc.Encode(src, w, h); err != nil {
			check("首帧（全量）", false, err.Error())
		}
		// 只改 y=500..560 这一块（1080/20=54 行/条带，应命中 1~2 个条带）
		mutate(src, w, h, 300, 500, 900, 560, 7)
		f, err := enc.Encode(src, w, h)
		if err != nil {
			check("增量帧", false, err.Error())
		} else {
			hit := false
			bad := false
			for _, t := range f.Tiles {
				// 命中判据：条带区间与 [500,560) 有交集
				if t.Y1 > 500 && t.Y0 < 560 {
					hit = true
				} else {
					bad = true
				}
			}
			check("变化区域被命中", hit, fmt.Sprintf("编码 %d/%d 条带：%s",
				f.DirtyTiles, f.TotalTiles, tileRanges(f)))
			check("无多余条带", !bad, fmt.Sprintf("只应命中与 [500,560) 相交的条带"))

			// 反向变异：完全不变的帧应产出 0 条带
			f2, _ := enc.Encode(src, w, h)
			check("静止帧产出 0 条带", f2.DirtyTiles == 0,
				fmt.Sprintf("实际 %d 条带，%d 字节", f2.DirtyTiles, f2.Bytes))
		}
	}

	// ---------- 4. 增量累积无漂移（核心断言） ----------
	out("")
	out("[4] 增量链累积 30 帧 vs 每帧全量（抓漏检）")
	{
		base := makeFrame(w, h)
		// 流 A：增量累积
		encA := codec.NewEncoder(codec.Config{Quality: 75, Tiles: 20, Dirty: true})
		decA := codec.NewDecoder(0)
		// 流 B：每帧全量
		encB := codec.NewEncoder(codec.Config{Quality: 75, Tiles: 20, Dirty: false})
		decB := codec.NewDecoder(0)

		cur := make([]byte, len(base))
		copy(cur, base)
		totalInc, totalFull := 0, 0
		for k := 0; k < 30; k++ {
			// 每帧改动一个随机小区域（模拟打字/鼠标）
			x0 := 100 + (k*97)%1300
			y0 := 60 + (k*53)%900
			mutate(cur, w, h, x0, y0, x0+220, y0+80, k)

			fa, _ := encA.Encode(cur, w, h)
			decA.Decode(fa)
			totalInc += fa.Bytes

			fb, _ := encB.Encode(cur, w, h)
			decB.Decode(fb)
			totalFull += fb.Bytes
		}
		ia, ib := decA.Image(), decB.Image()
		diff, maxd := 0, 0
		for j := 0; j < h; j++ {
			for i := 0; i < w; i++ {
				q := j*ia.Stride + i*4
				for c := 0; c < 3; c++ {
					d := int(ia.Pix[q+c]) - int(ib.Pix[q+c])
					if d < 0 {
						d = -d
					}
					if d > 2 {
						diff++
					}
					if d > maxd {
						maxd = d
					}
				}
			}
		}
		check("增量链与全量链一致", diff == 0,
			fmt.Sprintf("不一致像素 %d（最大差 %d）", diff, maxd))
		out(fmt.Sprintf("      带宽：增量累计 %.1f MB · 全量累计 %.1f MB · 节省 %.1f%%",
			float64(totalInc)/1e6, float64(totalFull)/1e6,
			(1-float64(totalInc)/float64(totalFull))*100))
	}

	// ---------- 5. 性能：2K 与 1080p ----------
	out("")
	out("[5] 编码/解码性能（真实运动内容）")
	type spec struct {
		w, h int
		name string
	}
	for _, sp := range []spec{{2880, 1800, "2K"}, {1920, 1080, "1080p"}} {
		src := makeFrame(sp.w, sp.h)
		cur := make([]byte, len(src))
		copy(cur, src)

		for _, dirty := range []bool{false, true} {
			label := "全量"
			if dirty {
				label = "增量"
			}
			enc := codec.NewEncoder(codec.Config{Quality: 75, Tiles: runtime.NumCPU(), Dirty: dirty})
			dec := codec.NewDecoder(0)
			// 预热
			enc.Encode(cur, sp.w, sp.h)
			var encMs, decMs float64
			totalBytes := 0
			n := 12
			for k := 0; k < n; k++ {
				// 每帧改动 25% 区域，模拟"部分活动"的桌面
				mutate(cur, sp.w, sp.h, 0, sp.h/4*k%sp.h, sp.w, sp.h/4*k%sp.h+sp.h/4, k)
				f, err := enc.Encode(cur, sp.w, sp.h)
				if err != nil {
					out(fmt.Sprintf("  ❌ %s %s 编码失败: %v", sp.name, label, err))
					break
				}
				totalBytes += f.Bytes
				encMs += f.EncodeMs
				if _, err := dec.Decode(f); err != nil {
					out(fmt.Sprintf("  ❌ %s %s 解码失败: %v", sp.name, label, err))
					break
				}
				decMs += dec.DecodeMs
			}
			em, dm := encMs/float64(n), decMs/float64(n)
			avg := totalBytes / n
			mbps := float64(avg) * 8 * 30 / 1e6
			ok := em+dm < 33.3
			check(fmt.Sprintf("%s %s", sp.name, label), ok,
				fmt.Sprintf("编码 %.1fms + 解码 %.1fms = %.1fms（%.0ffps）· 帧 %.0f KB · %.0f Mbps@30fps",
					em, dm, em+dm, 1000/(em+dm), float64(avg)/1024, mbps))
		}
	}

	out("")
	out("------------------------------------------------------------")
	out(fmt.Sprintf("结果: %d 通过 / %d 失败", pass, fail))
	out("")
	if fail > 0 {
		close(done)
		os.Exit(1)
	}
	close(done)
	os.Exit(0)
}

func tileRanges(f *codec.Frame) string {
	if len(f.Tiles) == 0 {
		return "无"
	}
	s := ""
	for i, t := range f.Tiles {
		if i >= 6 {
			s += fmt.Sprintf(" …(+%d)", len(f.Tiles)-6)
			break
		}
		if i > 0 {
			s += ","
		}
		s += fmt.Sprintf("%d[%d-%d]", t.Index, t.Y0, t.Y1)
	}
	return s
}

var _ = color.NRGBA{}
