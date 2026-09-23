// Package capture 封装屏幕采集：显示器枚举、区域裁切、鼠标光标叠加。
//
// 硬约束（阶段 0 实测得出，改动前务必读 VERIFY.md）：
//   - 禁用 screencapture.Options.ShowsCursor：会强制回退 GDI，
//     采集耗时 4.8ms → 58.3ms（12 倍），帧率掉到 17fps。光标必须自绘。
//   - 索引帧数据必须用 Frame.Stride，不能假设 Width*4
//     （本机恰好相等，换机器不一定）。
package capture

import (
	"context"
	"fmt"
	"sort"

	"github.com/go-mswin/screencapture"
)

// Display 是对 screencapture.Display 的精简包装，只暴露本项目需要的字段。
// X/Y 是显示器原点在 Windows 虚拟屏幕坐标系中的位置（多屏时可为负值），
// W/H 是物理像素尺寸。
type Display struct {
	raw          screencapture.Display
	ID           int
	Name         string
	X, Y         int
	W, H         int // 物理像素
	LogicalW     int // 逻辑像素（DIP）
	LogicalH     int
	DPI          int
	Scale        float64
	Primary      bool
	Duplicable   bool
	Rotation     string
	AdapterIndex int
	OutputIndex  int
}

// Rect 是以物理像素表示的矩形，原点为显示器左上角。
type Rect struct{ X, Y, W, H int }

// Full 返回覆盖整个显示器的矩形。
func (d Display) Full() Rect { return Rect{W: d.W, H: d.H} }

// Empty 报告矩形是否无效（宽或高非正）。
func (r Rect) Empty() bool { return r.W <= 0 || r.H <= 0 }

// Clamp 把矩形裁剪到显示器范围内，并保证 W/H 为偶数（视频编码要求）。
func (r Rect) Clamp(d Display) Rect {
	if r.W <= 0 || r.H <= 0 {
		r = d.Full()
	}
	if r.X < 0 {
		r.X = 0
	}
	if r.Y < 0 {
		r.Y = 0
	}
	if r.X > d.W {
		r.X = d.W
	}
	if r.Y > d.H {
		r.Y = d.H
	}
	if r.X+r.W > d.W {
		r.W = d.W - r.X
	}
	if r.Y+r.H > d.H {
		r.H = d.H - r.Y
	}
	// 偶数对齐：YUV 4:2:0 要求宽高均为偶数
	r.W &= ^1
	r.H &= ^1
	return r
}

// VirtualRect 返回该区域在 Windows 虚拟屏幕坐标系中的绝对矩形，
// 用于把鼠标光标位置换算到区域坐标。
func (d Display) VirtualRect(r Rect) (x0, y0, x1, y1 int) {
	return d.X + r.X, d.Y + r.Y, d.X + r.X + r.W, d.Y + r.Y + r.H
}

// String 返回人类可读的单行描述。
func (d Display) String() string {
	mark := ""
	if d.Primary {
		mark = " ·主屏"
	}
	return fmt.Sprintf("[%d] %s %dx%d (DPI %d, %.0f%%)%s",
		d.ID, d.Name, d.W, d.H, d.DPI, d.Scale*100, mark)
}

// Displays 枚举系统中所有显示器，按「主屏优先，其次 X、Y」排序，
// 并填充稳定的 ID（排序后的下标）。
func Displays(ctx context.Context) ([]Display, error) {
	raw, err := screencapture.Displays(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Display, 0, len(raw))
	for _, d := range raw {
		out = append(out, Display{
			raw:          d,
			Name:         d.DeviceName,
			// Bounds 是虚拟屏幕坐标系中的设备像素位置，可为负（显示器在主屏左侧/上方）
			X:            d.Bounds.X,
			Y:            d.Bounds.Y,
			W:            d.PixelWidth,
			H:            d.PixelHeight,
			LogicalW:     d.Width,
			LogicalH:     d.Height,
			DPI:          d.DPI,
			Scale:        d.Scale(),
			Primary:      d.Primary,
			Duplicable:   d.Duplicable(),
			Rotation:     fmt.Sprint(d.Rotation),
			AdapterIndex: d.AdapterIndex,
			OutputIndex:  d.OutputIndex,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Primary != out[j].Primary {
			return out[i].Primary
		}
		if out[i].X != out[j].X {
			return out[i].X < out[j].X
		}
		return out[i].Y < out[j].Y
	})
	for i := range out {
		out[i].ID = i
	}
	return out, nil
}
