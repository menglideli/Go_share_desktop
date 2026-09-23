package ui

import (
	"os"
	"path/filepath"

	"gioui.org/font/gofont"
	"gioui.org/font/opentype"
	"gioui.org/text"
)

// systemFontCandidates 是 Windows 上常见的中文字体，按优先级排列。
// Gio 自带的 gofont 不含汉字，不加载系统字体的话界面上的中文全是豆腐块。
var systemFontCandidates = []string{
	"msyh.ttc",  // 微软雅黑 Regular
	"msyhbd.ttc",// 微软雅黑 Bold
	"msyhl.ttc", // 微软雅黑 Light
	"simhei.ttf",
	"simsun.ttc",
}

// loadFontFaces 加载系统中文字体；失败时回退到 Gio 自带的 gofont。
func loadFontFaces() []text.FontFace {
	dir := os.Getenv("SystemRoot")
	if dir == "" {
		dir = `C:\Windows`
	}
	fontDir := filepath.Join(dir, "Fonts")

	var faces []text.FontFace
	for _, name := range systemFontCandidates {
		b, err := os.ReadFile(filepath.Join(fontDir, name))
		if err != nil {
			continue
		}
		f, err := opentype.ParseCollection(b)
		if err != nil {
			continue
		}
		faces = append(faces, f...)
	}
	if len(faces) == 0 {
		return gofont.Collection()
	}
	// 补上 gofont 作为兜底（含符号与等宽字体）
	faces = append(faces, gofont.Collection()...)
	return faces
}
