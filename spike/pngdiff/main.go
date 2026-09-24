// Command pngdiff 比较两张同尺寸 PNG 的像素差异，用于反向变异验证：
// 证明「改掉修复 → 画面确实变了」，而不是只凭肉眼或粗粒度统计下结论。
//
// 用法：go run ./spike/pngdiff a.png b.png
package main

import (
	"fmt"
	"image"
	_ "image/png"
	"os"
	"path/filepath"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "用法: pngdiff a.png b.png")
		os.Exit(2)
	}
	a, err := load(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	b, err := load(os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if a.Bounds() != b.Bounds() {
		fmt.Fprintf(os.Stderr, "尺寸不同: %v vs %v\n", a.Bounds(), b.Bounds())
		os.Exit(1)
	}
	ra, ga, ba, aa := rgba(a)
	rb, gb, bb, ab := rgba(b)
	total := a.Bounds().Dx() * a.Bounds().Dy()
	diff := 0
	sum := 0
	var box image.Rectangle
	first := true
	for y := a.Bounds().Min.Y; y < a.Bounds().Max.Y; y++ {
		for x := a.Bounds().Min.X; x < a.Bounds().Max.X; x++ {
			i := (y-a.Bounds().Min.Y)*a.Bounds().Dx() + (x - a.Bounds().Min.X)
			d := absi(int(ra[i])-int(rb[i])) + absi(int(ga[i])-int(gb[i])) +
				absi(int(ba[i])-int(bb[i])) + absi(int(aa[i])-int(ab[i]))
			if d > 24 {
				diff++
				sum += d
				p := image.Pt(x, y)
				if first {
					box = image.Rect(p.X, p.Y, p.X+1, p.Y+1)
					first = false
				} else {
					box = box.Union(image.Rect(p.X, p.Y, p.X+1, p.Y+1))
				}
			}
		}
	}
	pct := float64(diff) / float64(total) * 100
	fmt.Printf("%s vs %s\n", filepath.Base(os.Args[1]), filepath.Base(os.Args[2]))
	fmt.Printf("  差异像素 %d / %d = %.3f%%\n", diff, total, pct)
	if diff > 0 {
		fmt.Printf("  差异包围盒 %v（%dx%d）· 平均色差 %.1f\n",
			box, box.Dx(), box.Dy(), float64(sum)/float64(diff))
	}
}

func absi(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func load(p string) (image.Image, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	return img, err
}

func rgba(img image.Image) (r, g, b, a []uint8) {
	n := img.Bounds().Dx() * img.Bounds().Dy()
	r = make([]uint8, n)
	g = make([]uint8, n)
	b = make([]uint8, n)
	a = make([]uint8, n)
	i := 0
	for y := img.Bounds().Min.Y; y < img.Bounds().Max.Y; y++ {
		for x := img.Bounds().Min.X; x < img.Bounds().Max.X; x++ {
			cr, cg, cb, ca := img.At(x, y).RGBA()
			r[i], g[i], b[i], a[i] = uint8(cr>>8), uint8(cg>>8), uint8(cb>>8), uint8(ca>>8)
			i++
		}
	}
	return
}
