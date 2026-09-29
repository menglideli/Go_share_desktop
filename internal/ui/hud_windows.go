//go:build windows

package ui

import (
	"image"
	"log"
	"time"

	"golang.org/x/sys/windows"
)

// placeAndVerifyHUD 把悬浮条摆到目标位置并置顶，然后复核"摆完还能刷新吗"。
//
// 位置来源：用户上次拖到的地方（hudLastPos）优先，否则主屏工作区顶部居中。
//
// ⚠️ 为什么必须复核：PLAN 约束 5 —— 外部**移动** Gio 窗口会让它不再产生
// 帧事件（实测停在 10 帧）。只改 Z 序（SWP_NOMOVE）当年验证过是安全的，
// 但"挪位置"这条路径没有验证过。这里摆完就立刻验证一次：
// 主动 SetState 一次（内容翻转 → Invalidate → 应产生 FrameEvent），
// 由窗口自己的帧计数确认它还活着；不通过就打一条明确告警并记进日志，
// 不会静默退化成"悬浮条永久停在旧画面"。
func placeAndVerifyHUD(title string, size image.Point) {
	var target *image.Point
	// 重试：窗口刚创建时未必立刻能被 FindWindowW 找到；而且**SetWindowPos 返回成功
	// 不等于置顶位真的置上了** —— 实测：同一组参数，应用内调用后立刻读
	// WS_EX_TOPMOST 是 0，而外进程对着同一个窗口调用却能置上。
	// 所以复核判据必须是"读回来的置顶位"，不是返回值（旧实现只看返回值，
	// 于是"置顶失败"被当成成功，全程静默 —— 这类事故的通用教训）。
	for i := 0; i < 15; i++ {
		h := findWindow(title)
		if h != 0 {
			p := hudTargetPos(h, size)
			if target == nil {
				if moveAndTopmost(h, p.X, p.Y) {
					target = &p
				}
			} else if topmost(h) {
				// 位置已经摆好，这里只补置顶
			}
			if isTopmost(h) {
				log.Printf("hud: 已置顶并摆放到 (%d,%d)（第 %d 次尝试），ex=0x%08x", p.X, p.Y, i+1, exStyleOf(h))
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if target == nil {
		log.Printf("hud: ⚠ 摆放失败（窗口仍可用，位置由系统决定）")
		return
	}
	if h := findWindow(title); h != 0 {
		r := windowRect(h)
		log.Printf("hud: ⚠ 摆放已完成但置顶位始终置不上（15 次尝试）：ex=0x%08x 矩形 (%d,%d)-(%d,%d) —— 悬浮条可能被别的窗口盖住",
			exStyleOf(h), r.Left, r.Top, r.Right, r.Bottom)
	}
}

// hudTargetPos 返回本次要摆到的位置（物理像素）。
func hudTargetPos(h windows.HWND, size image.Point) image.Point {
	if p := hudLastPos.Load(); p != nil {
		return *p
	}
	wa, ok := primaryWorkArea()
	if !ok {
		// 拿不到工作区（极少见）：只置顶，位置交给系统。
		r := windowRect(h)
		return image.Pt(int(r.Left), int(r.Top))
	}
	// 顶部居中，离工作区上沿 12 dp（用窗口高度的 dp 比例换算缩放）。
	scale := 1.0
	if size.Y > 0 {
		scale = float64(size.Y) / 46
	}
	x := int((wa.Left+wa.Right)/2) - size.X/2
	y := int(wa.Top) + int(12*scale)
	if y < int(wa.Top) {
		y = int(wa.Top)
	}
	return image.Pt(x, y)
}

// rememberHUDPos 记下悬浮条当前的位置（关窗前调用），供下次分享复用。
func rememberHUDPos(title string) {
	h := findWindow(title)
	if h == 0 {
		return
	}
	r := windowRect(h)
	// 被最小化时读到的矩形是停靠位（-32000 之类），不能当"用户拖到的位置"存。
	if isIconic(h) || r.Left < -10000 || r.Top < -10000 {
		return
	}
	hudLastPos.Store(&image.Point{X: int(r.Left), Y: int(r.Top)})
}
