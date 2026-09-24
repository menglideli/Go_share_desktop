//go:build windows

package ui

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

// P0-1 防自摄入。
//
// LTSC 17763 没有 WDA_EXCLUDEFROMCAPTURE，分享端自己的窗口会被自己采集，
// 形成**无限套娃**（实测 dump 出来的帧里清清楚楚套了 6~7 层）。
// 这既是观感灾难，也让每一帧都在变化 —— 全量帧永远不会停，带宽白烧。
//
// 主要手段：分享开始后把主窗最小化。最小化的窗口不在屏幕上，自然进不了采集画面。
// 兜底手段：顺手设 WDA_MONITOR，万一用户又把窗口还原了，采集里也是一块黑矩形，
// 仍然远好过套娃（WDA_MONITOR 从 Vista 就有，不像 WDA_EXCLUDEFROMCAPTURE 要 2004+）。
//
// 为什么不用 x/sys/windows 的现成封装：它只导出了 EnumWindows / IsWindowVisible /
// GetWindowThreadProcessId，ShowWindow / GetWindow / SetWindowDisplayAffinity 得自己取。
const (
	swMinimize = 6 // SW_MINIMIZE
	wdaMonitor = 1 // WDA_MONITOR
	gwOwner    = 4 // GW_OWNER
)

var (
	user32                       = windows.NewLazySystemDLL("user32.dll")
	procShowWindow               = user32.NewProc("ShowWindow")
	procIsIconic                 = user32.NewProc("IsIconic")
	procGetWindow                = user32.NewProc("GetWindow")
	procSetWindowDisplayAffinity = user32.NewProc("SetWindowDisplayAffinity")
)

// ownMainWindow 找出本进程的可见顶层窗口，也就是 Gio 的主窗。
//
// 用"同进程 + 可见 + 无 owner"来认，而不是按标题匹配：
// 标题里有「·」这类字符，跨 FindWindowW 的编码/匹配容易出岔子；
// 而且按标题匹配会把别的进程的同名窗口也认进来。
//
// ⚠️ 回调**始终返回 1（继续枚举）**：返回 0 会让 EnumWindows 返回 FALSE，
// 而 x/sys 的封装把它当成 error，于是"成功找到窗口"反而变成一个带脏错误码的失败。
// 顶层窗口只有几十个，遍历完的开销可以忽略。
func ownMainWindow() (windows.HWND, error) {
	pid := uint32(os.Getpid())
	var found windows.HWND
	cb := syscall.NewCallback(func(hwnd windows.HWND, _ uintptr) uintptr {
		if found != 0 {
			return 1
		}
		// ⚠️ 必须走 x/sys/windows 的封装，不能自己 proc.Call(hwnd, uintptr(unsafe.Pointer(&pid)))：
		// 那样把指向 Go 变量的指针塞进 uintptr 传给 syscall，pid 拿不到（实测恒为 0），
		// 于是每个窗口都被判成"别的进程"、全部跳过，最后报"没找到本进程的可见顶层窗口"。
		var wpid uint32
		windows.GetWindowThreadProcessId(hwnd, &wpid)
		if wpid != pid {
			return 1
		}
		if !windows.IsWindowVisible(hwnd) {
			return 1
		}
		// 有 owner 的是对话框/工具窗，不是主窗。
		if owner, _, _ := procGetWindow.Call(uintptr(hwnd), gwOwner); owner != 0 {
			return 1
		}
		found = hwnd
		return 1
	})
	if err := windows.EnumWindows(cb, nil); err != nil {
		return 0, fmt.Errorf("ui: 枚举窗口失败：%w", err)
	}
	if found == 0 {
		return 0, fmt.Errorf("ui: 没找到本进程的可见顶层窗口")
	}
	return found, nil
}

// MinimizeMainWindow 把本进程主窗最小化，并顺手设上防采集兜底。
//
// ⚠️ 不要在 Gio 的事件回调里直接调：ShowWindow 会同步派发窗口消息，
// 在事件处理中重入 Gio 的消息循环是自找麻烦。调用方应挪到独立 goroutine，
// 并稍微延后一点（见 cmd/goshare 的 startShare）。
func MinimizeMainWindow() error {
	h, err := ownMainWindow()
	if err != nil {
		return err
	}
	// 兜底先设。失败不报错：这只是"万一窗口被还原"的第二道防线，
	// 最小化这个主要手段不依赖它（有些策略组/远程桌面会话会拒绝这个调用）。
	_, _, _ = procSetWindowDisplayAffinity.Call(uintptr(h), wdaMonitor)

	r, _, e := procShowWindow.Call(uintptr(h), swMinimize)
	if r == 0 {
		// ShowWindow 的返回值是"调用前窗口是否可见"，返回 0 不代表失败。
		//
		// ⚠️ 复核**不能**用 IsWindowVisible：最小化的窗口仍然带 WS_VISIBLE 样式位，
		// IsWindowVisible 照样返回 true（实测探针显示"可见=true"却已经最小化）。
		// 判据必须是 IsIconic —— 它问的是"有没有真的变成图标/最小化"。
		if iconic, _, _ := procIsIconic.Call(uintptr(h)); iconic == 0 {
			return fmt.Errorf("ui: ShowWindow(SW_MINIMIZE) 未生效：%v", e)
		}
	}
	return nil
}
