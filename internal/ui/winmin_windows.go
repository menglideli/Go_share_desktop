//go:build windows

package ui

import (
	"fmt"
	"log"
	"os"
	"sync/atomic"
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
// GetWindowThreadProcessId，ShowWindow / GetWindow / SetWindowDisplayAffinity 得自己取
// （都集中在 winapi_windows.go）。

// minMainWindowArea 是"能当主窗"的面积下限（物理像素²），只在**无法按标题精确
// 匹配**时用于兜底筛选。
//
// 依据：外壳窗口的 MinSize 是 720×520 dp（internal/ui/shell.go），
// 即便按 100% 缩放算，物理面积也有 374400 px²；取 20 万作下限留足余量。
// 之所以需要这条下限：Gio 建窗用 CW_USEDEFAULT，**Configure 之前**窗口的
// 尺寸和标题都还不是最终值 —— 实测抓到过一个 339×56、空标题的瞬态窗口
// （20 万 px² 的十分之一不到），它出现得比主窗早，旧的"面积最大"规则
// 会把最小化打在它身上，而它随后就被 Gio 配置成了别的东西 → 主窗根本没被最小化。
const minMainWindowArea = 200_000

// mainWindowTitle 是应用外壳窗口的标题（Shell.Run 启动时登记）。
//
// 认主窗优先用它做**精确匹配**：比"面积最大/枚举顺序"这类启发式可靠得多 ——
// 本轮两次踩坑（悬浮条被当成主窗、Gio 配置前的瞬态窗被当成主窗）都是
// 启发式的误判。标题是外壳自己声明的身份，不需要猜。
var mainWindowTitle atomic.Value // string

// RegisterMainWindowTitle 由 Shell.Run 在创建窗口前登记主窗标题。
func RegisterMainWindowTitle(title string) { mainWindowTitle.Store(title) }

// ownMainWindow 找出本进程的**主窗**，也就是 Gio 的应用外壳窗口。
//
// 判据（按优先级）：
//  1. **标题精确等于**外壳登记的标题（RegisterMainWindowTitle）—— 首选，
//     这是外壳自己声明的身份，不需要猜；
//  2. 兜底：同进程 + 可见 + 无 owner 的顶层窗口里取面积最大的，
//     排除悬浮条（hudWindowTitle）与面积不足的"尚未配置完成的窗口"。
//
// ⚠️ 为什么不是"第一个匹配的"：悬浮条是分享一开始就创建的第二个 Gio 窗口，
// 新建窗口位于 Z 序顶端，于是 EnumWindows 往往先枚举到它 —— 旧规则会把悬浮条
// 当成主窗，SW_MINIMIZE 打到悬浮条身上。实测后果（真机窗口枚举）：
// 悬浮条被停靠到 (-32000,-32000) 用户完全看不见、主窗留在屏幕上被自己采集，
// 而日志照样打「主窗已最小化」——**在错误的窗口上成功**，全程静默。
// 悬浮条 ≤560×80、主窗 ≥2065×1365，面积差三个数量级，取面积最大不存在误判空间。
//
// ⚠️ 回调**始终返回 1（继续枚举）**：返回 0 会让 EnumWindows 返回 FALSE，
// 而 x/sys 的封装把它当成 error，于是"成功找到窗口"反而变成一个带脏错误码的失败。
// 顶层窗口只有几十个，遍历完的开销可以忽略。
func ownMainWindow() (windows.HWND, error) {
	pid := uint32(os.Getpid())

	type candidate struct {
		hwnd  windows.HWND
		area  int64
		title string
		rect  winRect
	}
	var best candidate
	var exact candidate
	want, _ := mainWindowTitle.Load().(string)
	cb := syscall.NewCallback(func(hwnd windows.HWND, _ uintptr) uintptr {
		// ⚠️ 必须走 x/sys/windows 的封装，不能自己 proc.Call(hwnd, uintptr(unsafe.Pointer(&pid)))：
		// 那样把指向 Go 变量的指针塞进 uintptr 传给 syscall，pid 拿不到（实测恒为 0），
		// 于是每个窗口都被判成"别的进程"、全部跳过，最后报"没找到本进程的可见顶层窗口"。
		var wpid uint32
		windows.GetWindowThreadProcessId(hwnd, &wpid)
		if wpid != pid {
			return 1
		}
		if !isVisible(hwnd) {
			return 1
		}
		// 有 owner 的是对话框/工具窗，不是主窗。
		if windowOwner(hwnd) != 0 {
			return 1
		}
		title := windowTitle(hwnd)
		r := windowRect(hwnd)
		area := int64(r.Dx()) * int64(r.Dy())
		if want != "" && title == want {
			if exact.hwnd == 0 {
				exact = candidate{hwnd: hwnd, area: area, title: title, rect: r}
			}
			return 1
		}
		log.Printf("winmin: 候选 0x%x %q (%d,%d)-(%d,%d) 面积 %d", hwnd, title, r.Left, r.Top, r.Right, r.Bottom, area)
		if title == hudWindowTitle {
			return 1
		}
		// 面积下限只用来排除"还没配置完的新窗口"。
		// ⚠️ 已被最小化的窗口必须豁免：最小化后 GetWindowRect 返回的是**图标位**的
		// 小矩形（实测 276×45），拿它算面积会把真正的主窗判成不合格 ——
		// 而 RestoreMainWindow 恰恰就是在主窗最小化的状态下调用的（实测踩过）。
		if !isIconic(hwnd) && area < minMainWindowArea {
			return 1
		}
		if area > best.area {
			best = candidate{hwnd: hwnd, area: area, title: title, rect: r}
		}
		return 1
	})
	if err := windows.EnumWindows(cb, nil); err != nil {
		return 0, fmt.Errorf("ui: 枚举窗口失败：%w", err)
	}
	if exact.hwnd != 0 {
		best = exact
		log.Printf("winmin: 主窗 = 0x%x %q（按标题精确匹配）矩形 (%d,%d)-(%d,%d)",
			best.hwnd, best.title, best.rect.Left, best.rect.Top, best.rect.Right, best.rect.Bottom)
		return best.hwnd, nil
	}
	if best.hwnd == 0 {
		return 0, fmt.Errorf("ui: 没找到本进程的主窗（既没有标题为 %q 的窗口，最大候选面积也不足 %d px²）",
			want, minMainWindowArea)
	}
	// 目标必须留痕：这次事故的全部代价，来自"打错窗口还报成功"。
	log.Printf("winmin: 主窗 = 0x%x %q 矩形 (%d,%d)-(%d,%d) 面积 %d",
		best.hwnd, best.title, best.rect.Left, best.rect.Top, best.rect.Right, best.rect.Bottom, best.area)
	return best.hwnd, nil
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
		// 复核用 IsIconic（见 winapi_windows.go 的说明，不能用 IsWindowVisible）。
		if !isIconic(h) {
			return fmt.Errorf("ui: ShowWindow(SW_MINIMIZE) 未生效：%v", e)
		}
	}
	return nil
}

// RestoreMainWindow 把主窗从最小化恢复并抢到前台（托盘「显示主窗口」用）。
//
// 与 MinimizeMainWindow 同一个坑：调用方不要跑在 Gio 事件回调里
// （托盘回调本来就是独立 goroutine，天然满足）。
//
// ⚠️ ownMainWindow 只认"可见"顶层窗——最小化的窗口 WS_VISIBLE 位仍在
// （见 winapi_windows.go 的 isIconic 注释），所以最小化状态下照样能找到，
// 不用担心。但**面积下限对最小化窗口必须豁免**：最小化后 GetWindowRect 返回的是
// 图标位的小矩形，拿它算面积会把主窗判成不合格（实测踩过，报"没找到主窗"）。
func RestoreMainWindow() error {
	h, err := ownMainWindow()
	if err != nil {
		return err
	}
	procShowWindow.Call(uintptr(h), swRestore)
	// 从托盘（另一个进程的输入上下文之外）抢前台，Windows 常会拒绝
	// SetForegroundWindow —— 但先 SW_RESTORE 再调它的组合在实测里能工作，
	// 失败也无害（窗口已还原，只是没置顶），所以不检查返回值。
	procSetForegroundWindow.Call(uintptr(h))
	// 但**还原本身**必须复核：这条路径以前完全静默，"没找到主窗"时
	// 用户只会看到"点了托盘菜单什么都没发生"（实测踩过）。
	if isIconic(h) {
		return fmt.Errorf("ui: SW_RESTORE 未生效（窗口 0x%x 仍是图标态）", h)
	}
	return nil
}
