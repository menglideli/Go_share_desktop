package pipeline

import (
	"testing"

	"goshare/internal/capture"
)

// TestCaptureFPS 钉住"档位真正驱动采集帧率"的语义（P0-2）。
//
// 背景：MaxFPS 原先只限制**发送**，采集源一直按 30fps —— "流畅60"永远是假 60。
// 但 60fps 不是无条件的：2K 一帧编码 12~15ms，60fps 的 16.7ms 预算不成立。
// 这两条约束必须同时被这条测试守住，否则要么回到假 60，要么在 2K 上把延迟拖爆。
func TestCaptureFPS(t *testing.T) {
	hd := capture.Display{W: 1920, H: 1080}
	big := capture.Display{W: 2880, H: 1800}

	cases := []struct {
		name string
		p    Preset
		r    capture.Rect
		d    capture.Display
		want int
	}{
		{"流畅60 + 1080p 真 60", Preset60, capture.Rect{}, hd, 60},
		{"流畅60 + 2K 回落 30", Preset60, capture.Rect{}, big, 30},
		{"流畅60 + 2K 上的小区域仍是 60", Preset60, capture.Rect{W: 1280, H: 720}, big, 60},
		{"流畅30 保持 30", Preset30, capture.Rect{}, hd, 30},
		{"最大档不擅自抬到 60", PresetMax, capture.Rect{}, hd, 30},
		{"急速不抬帧率", PresetTurbo, capture.Rect{}, hd, 30},
	}
	for _, c := range cases {
		if got := captureFPSFor(c.p, c.r, c.d); got != c.want {
			t.Errorf("%s: captureFPSFor = %d，期望 %d", c.name, got, c.want)
		}
	}
}
