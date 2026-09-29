//go:build windows

package ui

import "log"

// LogWindowVisible 用 Win32 FindWindowW / IsWindowVisible 断言窗口真实存在。
//
// 「渲染了帧」不等于「窗口真的显示出来了」——阶段 0 就差点在这里误判：
// 第一帧时 IsWindowVisible 返回 0，实际是 Gio 尚未完成 ShowWindow，
// 持续渲染到第 10 帧再断言才是 1。
func LogWindowVisible(title string) {
	h := findWindow(title)
	if h == 0 {
		log.Printf("wincheck: 未找到窗口 %q", title)
		return
	}
	log.Printf("wincheck: 窗口 %q hwnd=0x%x visible=%v", title, h, isVisible(h))
}

// LogWindowState 是"置顶小窗到底看得见吗"的完整断言，返回是否正常。
//
// 为什么需要它：悬浮条这类独立置顶窗**没有任何界面入口能看到自己的状态**，
// 出了事只能靠日志。本轮实测过一次：日志打着「主窗已最小化」，
// 而实际被最小化的是悬浮条 —— 它被停靠在 (-32000,-32000)，用户什么都看不到，
// 因为没有这条断言，整个事故只能靠窗口枚举探针才发现。
//
// 判据三项：可见（IsWindowVisible）、没被最小化（IsIconic，不能用可见性代替）、
// 真的在置顶层（WS_EX_TOPMOST）。
func LogWindowState(title string) bool {
	h := findWindow(title)
	if h == 0 {
		log.Printf("wincheck: ⚠ 未找到窗口 %q —— 它根本没被创建出来", title)
		return false
	}
	r := windowRect(h)
	vis, iconic, top := isVisible(h), isIconic(h), isTopmost(h)
	log.Printf("wincheck: 窗口 %q hwnd=0x%x 可见=%v 最小化=%v 置顶=%v 矩形=(%d,%d)-(%d,%d) %dx%d",
		title, h, vis, iconic, top, r.Left, r.Top, r.Right, r.Bottom, r.Dx(), r.Dy())
	if !vis || iconic || !top {
		log.Printf("wincheck: ⚠ 窗口 %q 状态异常（可见=%v 最小化=%v 置顶=%v）—— 用户很可能看不到它",
			title, vis, iconic, top)
		return false
	}
	return true
}
