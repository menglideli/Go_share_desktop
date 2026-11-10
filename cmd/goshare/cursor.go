// 观看端的光标图层状态（P0-3）。
//
// 分享端不再把光标画进帧，而是发两路 ctl：
//   - CtlCursorShape：形状位图（只在变化时发，按 ID 缓存）
//   - CtlCursorPos：60Hz 的位置采样（25 字节）
//
// 观看端按"当前显示时刻"取位置：播放缓冲 D 让画面比实时晚 D 毫秒，
// 光标也要取同一时刻的采样，否则会和画面错开（看鼠标精细操作时非常明显）。
package main

import (
	"fmt"
	"image"
	"sync"
	"time"

	"goshare/internal/rtc"
)

// cursorSampleCap 是位置采样的环形缓冲长度（60Hz 下约 0.25 秒）。
const cursorSampleCap = 16

type cursorSample struct {
	ts  uint32 // 分享端时钟毫秒
	x   int32  // 光标热点（虚拟屏幕绝对坐标）
	y   int32
	rx  int32 // 采样时采集区域的左上角
	ry  int32
	vis bool
	id  uint32
}

// cursorLayer 保存光标形状与最近的位置采样（并发安全）。
type cursorLayer struct {
	mu      sync.Mutex
	shapeID uint32
	shape   *image.NRGBA
	hot     image.Point
	samples [cursorSampleCap]cursorSample
	n, next int
}

// setShape 缓存一个形状（BGRA→NRGBA）。同一 ID 直接忽略。
func (c *cursorLayer) setShape(sh rtc.CursorShape) {
	if sh.W <= 0 || sh.H <= 0 || len(sh.Pix) < sh.W*sh.H*4 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if sh.ID == c.shapeID && c.shape != nil {
		return
	}
	img := image.NewNRGBA(image.Rect(0, 0, sh.W, sh.H))
	for i := 0; i < sh.W*sh.H; i++ {
		b, g, r, a := sh.Pix[i*4], sh.Pix[i*4+1], sh.Pix[i*4+2], sh.Pix[i*4+3]
		img.Pix[i*4], img.Pix[i*4+1], img.Pix[i*4+2], img.Pix[i*4+3] = r, g, b, a
	}
	c.shapeID = sh.ID
	c.shape = img
	c.hot = image.Pt(sh.HotX, sh.HotY)
}

// addPos 追加一个位置采样。
func (c *cursorLayer) addPos(p rtc.CursorPos) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.samples[c.next] = cursorSample{
		ts: p.TS, x: p.X, y: p.Y, rx: p.RX, ry: p.RY, vis: p.Vis, id: p.ID,
	}
	c.next = (c.next + 1) % cursorSampleCap
	if c.n < cursorSampleCap {
		c.n++
	}
}

// at 返回"分享端时钟 hostMS 时刻"的光标：位图、图像坐标（已含热点偏移）与是否可见。
//
// 采样点与 hostMS 最接近的一帧即可：60Hz 采样、30fps 画面，误差 < 16ms；
// 快速甩鼠标时这点误差肉眼看不出来（≤2px 的判据是按"绘制点与热点一致"算的）。
func (c *cursorLayer) at(hostMS uint32) (*image.NRGBA, image.Point, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n == 0 || c.shape == nil {
		return nil, image.Point{}, false
	}
	// 找 ts 最接近 hostMS 的采样（环形缓冲里最多 16 个，线性扫足够）。
	best := -1
	bestDiff := int64(1 << 40)
	for k := 0; k < c.n; k++ {
		idx := (c.next - 1 - k + cursorSampleCap*2) % cursorSampleCap
		s := c.samples[idx]
		d := int64(int32(s.ts - hostMS))
		if d < 0 {
			d = -d
		}
		if d < bestDiff {
			bestDiff, best = d, idx
		}
	}
	if best < 0 {
		return nil, image.Point{}, false
	}
	s := c.samples[best]
	if !s.vis || s.id != c.shapeID {
		return nil, image.Point{}, false
	}
	// 图像坐标 = 热点绝对坐标 - 区域原点 - 热点偏移
	return c.shape,
		image.Pt(int(s.x-s.rx)-c.hot.X, int(s.y-s.ry)-c.hot.Y),
		true
}

// lastSeen 返回最近一次采样的时刻（判断光标流是否还活着）。
func (c *cursorLayer) lastSeen() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n == 0 {
		return time.Time{}
	}
	idx := (c.next - 1 + cursorSampleCap) % cursorSampleCap
	return time.Unix(0, int64(c.samples[idx].ts)*int64(time.Millisecond))
}

// newest 直接用最新一个采样（时钟还没同步上时的退路）。
func (c *cursorLayer) newest() (*image.NRGBA, image.Point, bool) {
	c.mu.Lock()
	if c.n == 0 {
		c.mu.Unlock()
		return nil, image.Point{}, false
	}
	ts := c.samples[(c.next-1+cursorSampleCap)%cursorSampleCap].ts
	c.mu.Unlock()
	return c.at(ts)
}

// summary 返回光标层的一行状态（诊断日志用）。
func (c *cursorLayer) summary() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n == 0 {
		return "光标流 未收到"
	}
	s := c.samples[(c.next-1+cursorSampleCap)%cursorSampleCap]
	// 图像坐标 = 热点绝对坐标 - 区域原点 - 热点偏移（判据就是它落在画面里）。
	imgX, imgY := int(s.x-s.rx)-c.hot.X, int(s.y-s.ry)-c.hot.Y
	return fmt.Sprintf("光标流 采样%d 形状#%x 图像坐标(%d,%d) 可见=%v", c.n, c.shapeID, imgX, imgY, s.vis)
}

// cursorOverlay 返回"此刻该画的光标"（ui.Shell 每帧渲染时调用）。
//
// 时间换算：画面显示的是 `D` 毫秒前的内容（播放缓冲），光标必须取同一时刻的
// 采样，否则看鼠标精细操作时会出现"光标比画面快半拍"。时钟没同步上时退化为
// "取最新采样"—— 宁可不对齐，也不能不画。
func (a *app) cursorOverlay() (*image.NRGBA, image.Point, bool) {
	if a.cur == nil {
		return nil, image.Point{}, false
	}
	off, synced := a.clock.Offset()
	if !synced {
		return a.cur.newest()
	}
	hostNow := uint32(int64(time.Now().UnixMilli()) + int64(off))
	return a.cur.at(hostNow - uint32(a.playoutMS))
}
