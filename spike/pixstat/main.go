// Command pixstat 读一张 PNG，输出整体与"分带"像素统计。
//
// 为什么需要分带：整幅统计会把"一半画了一半黑的画面"平均成一个中间值，
// 看起来像"画面偏暗"，实际是"有条带根本没被填充"。
// 分带后能直接定位黑带在哪个高度区间 —— 用来查 MJPEG 条带增量没覆盖全画布的问题。
//
// 用法：go run ./spike/pixstat a.png [b.png ...]
package main

import (
	"fmt"
	"image"
	_ "image/png"
	"os"
	"path/filepath"
)

const bands = 16

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "用法: pixstat a.png [b.png ...]")
		os.Exit(2)
	}
	for _, p := range os.Args[1:] {
		f, err := os.Open(p)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		img, _, err := image.Decode(f)
		f.Close()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		report(filepath.Base(p), img)
	}
}

func report(name string, img image.Image) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	sum, n, nb := 0, 0, 0
	bandLum := make([]float64, bands)
	bandN := make([]int, bands)
	step := 2
	for y := 0; y < h; y += step {
		bi := y * bands / h
		for x := 0; x < w; x += step {
			r, g, bl, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			R, G, B := int(r>>8), int(g>>8), int(bl>>8)
			lum := (77*R + 150*G + 29*B) >> 8
			sum += lum
			n++
			if lum > 8 {
				nb++
			}
			bandLum[bi] += float64(lum)
			bandN[bi]++
		}
	}
	fmt.Printf("%s  %dx%d  非黑 %.1f%%  亮度 %.1f\n",
		name, w, h, float64(nb)/float64(n)*100, float64(sum)/float64(n))
	black := 0
	for i := 0; i < bands; i++ {
		if bandN[i] == 0 {
			continue
		}
		l := bandLum[i] / float64(bandN[i])
		mark := " "
		if l < 5 {
			mark = "X" // 这一带基本是黑的
			black++
		}
		fmt.Printf("    带 %2d/%d  y %4d..%4d  亮度 %6.1f %s\n",
			i+1, bands, i*h/bands, (i+1)*h/bands, l, mark)
	}
	if black > 0 {
		fmt.Printf("    → %d / %d 带是黑的\n", black, bands)
	}
}
