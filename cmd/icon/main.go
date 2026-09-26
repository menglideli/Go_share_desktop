// icon — 生成应用图标（多尺寸 PNG + 单文件 ICO）。
//
// 图标图案来自 internal/tray.PaintIcon（与运行时托盘图标同一份绘制代码，
// 保证托盘与 exe 图标视觉一致）。输出供 winres/winres.json 引用，
// 由 scripts/build.ps1 在发布构建时调用。
//
// 用法：go run ./cmd/icon -out winres/icons
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"image"
	"image/png"
	"log"
	"os"
	"path/filepath"

	xdraw "golang.org/x/image/draw"

	"goshare/internal/tray"
)

// sizes 是输出的图标尺寸集合。16/32/48 是资源管理器与任务栏常用尺寸，
// 256 给高分屏与"大图标"视图（PNG 压缩条目，Vista 起支持）。
var sizes = []int{16, 24, 32, 48, 64, 128, 256}

func render(size int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	// NRGBA 的 Pix 布局 (y*size+x)*4 与 PaintIcon 的索引约定一致。
	tray.PaintIcon(img.Pix, size, false)
	return img
}

func main() {
	out := flag.String("out", "winres/icons", "输出目录")
	flag.Parse()
	if err := os.MkdirAll(*out, 0o755); err != nil {
		log.Fatalf("创建输出目录失败: %v", err)
	}

	src := render(256)
	pngs := make([][]byte, 0, len(sizes))
	for _, sz := range sizes {
		img := src
		if sz != 256 {
			img = image.NewNRGBA(image.Rect(0, 0, sz, sz))
			xdraw.CatmullRom.Scale(img, img.Bounds(), src, src.Bounds(), xdraw.Over, nil)
		}
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			log.Fatalf("PNG 编码失败: %v", err)
		}
		pngs = append(pngs, buf.Bytes())
		p := filepath.Join(*out, fmt.Sprintf("icon-%d.png", sz))
		if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
			log.Fatalf("写 %s 失败: %v", p, err)
		}
	}

	icoPath := filepath.Join(*out, "icon.ico")
	if err := writeICO(icoPath, pngs); err != nil {
		log.Fatalf("写 ICO 失败: %v", err)
	}
	log.Printf("已生成 %d 个 PNG + icon.ico → %s", len(sizes), *out)
}

// writeICO 把多个 PNG 打包成单文件 ICO（全部用 PNG 压缩条目）。
//
// ICO 结构：ICONDIR(6B) + ICONDIRENTRY(16B × N) + 各图像数据。
// 宽/高字段是单字节，256 按规范写 0。
func writeICO(path string, pngs [][]byte) error {
	var buf bytes.Buffer
	w16 := func(v uint16) { _ = binary.Write(&buf, binary.LittleEndian, v) }
	w32 := func(v uint32) { _ = binary.Write(&buf, binary.LittleEndian, v) }

	w16(0) // reserved
	w16(1) // type: icon
	w16(uint16(len(pngs)))
	offset := 6 + 16*len(pngs)
	for i, sz := range sizes {
		b := byte(sz)
		if sz >= 256 {
			b = 0
		}
		buf.WriteByte(b) // width
		buf.WriteByte(b) // height
		buf.WriteByte(0) // palette colors
		buf.WriteByte(0) // reserved
		w16(1)           // planes
		w16(32)          // bits per pixel
		w32(uint32(len(pngs[i])))
		w32(uint32(offset))
		offset += len(pngs[i])
	}
	for _, p := range pngs {
		buf.Write(p)
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}
