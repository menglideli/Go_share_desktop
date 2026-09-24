// shot — 屏幕快照：采集指定显示器并保存 PNG，用于核对窗口里到底画出了什么。
//
// 为什么需要它：日志里的"渲染第 N 帧（画面 true）"只能证明收到了帧，
// 证明不了真的画到屏幕上。看窗口内容的唯一可信办法是把它截下来用眼睛看。
package main

import (
	"context"
	"flag"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"time"

	"goshare/internal/capture"
)

func main() {
	idx := flag.Int("display", 0, "采集哪个显示器")
	out := flag.String("out", "out/shot.png", "输出 PNG 路径")
	delay := flag.Duration("delay", 0, "启动后等待多久再取帧")
	skip := flag.Int("skip", 1, "跳过前 N 帧（采集流首帧可能是未初始化的黑帧）")
	flag.Parse()

	capture.EnsureDPIAware()
	ctx := context.Background()

	ds, err := capture.Displays(ctx)
	if err != nil || len(ds) == 0 {
		panic("枚举显示器失败")
	}
	if *idx < 0 || *idx >= len(ds) {
		panic("显示器下标越界")
	}
	time.Sleep(*delay)

	src, err := capture.NewSource(ctx, capture.Options{Display: ds[*idx], FPS: 30, Cursor: false})
	if err != nil {
		panic(err)
	}
	defer src.Close()

	for k := 0; k < *skip+3; k++ {
		c, cancel := context.WithTimeout(ctx, 3*time.Second)
		f, err := src.WaitFrame(c)
		cancel()
		if err != nil {
			continue
		}
		if k < *skip {
			continue
		}
		img := image.NewNRGBA(image.Rect(0, 0, f.W, f.H))
		d, s := img.Pix, f.Pix
		for i := 0; i < f.W*f.H; i++ {
			p := i * 4
			d[p], d[p+1], d[p+2], d[p+3] = s[p+2], s[p+1], s[p], 255
		}
		if err := os.MkdirAll(filepath.Dir(*out), 0755); err != nil {
			panic(err)
		}
		fh, err := os.Create(*out)
		if err != nil {
			panic(err)
		}
		if err := png.Encode(fh, img); err != nil {
			panic(err)
		}
		fh.Close()
		println("saved", *out, f.W, "x", f.H)
		return
	}
	println("没取到帧")
	os.Exit(1)
}
