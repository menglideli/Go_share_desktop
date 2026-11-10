// Package clip 把文本放进系统剪贴板，给"一键复制地址"用。
//
// Windows 上直接调 clip.exe：它就在系统目录里、不需要 CGO、不会闪控制台窗口。
// 自己走 user32 的 OpenClipboard/SetClipboardData 当然也行，但要多写 60 行
// syscall 与内存锁定逻辑，收益只是少起一个进程 —— 复制是低频操作，不值得。
package clip

import (
	"os/exec"
	"strings"
	"syscall"
	"unsafe"
)

// SetText 把文本写入剪贴板。失败不影响主流程（用户还能手动选中复制）。
func SetText(s string) error {
	if s == "" {
		return nil
	}
	cmd := exec.Command("clip")
	cmd.Stdin = strings.NewReader(s)
	return cmd.Run()
}

var (
	user32c            = syscall.NewLazyDLL("user32.dll")
	kernel32c          = syscall.NewLazyDLL("kernel32.dll")
	pOpenClipboard     = user32c.NewProc("OpenClipboard")
	pCloseClipboard    = user32c.NewProc("CloseClipboard")
	pGetClipboardData  = user32c.NewProc("GetClipboardData")
	pIsClipFormatAvail = user32c.NewProc("IsClipboardFormatAvailable")
	pGlobalLock        = kernel32c.NewProc("GlobalLock")
	pGlobalUnlock      = kernel32c.NewProc("GlobalUnlock")
	pGlobalSize        = kernel32c.NewProc("GlobalSize")
)

const cfUnicodeText = 13

// GetText 读取剪贴板里的 Unicode 文本（P3-1 剪贴板同步）。
//
// 为什么这里用 user32 而 SetText 用 clip.exe：读剪贴板没有等价的命令行工具，
// PowerShell 的 Get-Clipboard 每次要起一个进程（~100ms），500ms 轮询扛不住。
// 只读文本格式，不碰图片/文件列表 —— 避免把大对象搬进网络。
func GetText() (string, bool) {
	if r, _, _ := pIsClipFormatAvail.Call(cfUnicodeText); r == 0 {
		return "", false
	}
	if r, _, _ := pOpenClipboard.Call(0); r == 0 {
		return "", false
	}
	defer pCloseClipboard.Call()
	h, _, _ := pGetClipboardData.Call(cfUnicodeText)
	if h == 0 {
		return "", false
	}
	ptr, _, _ := pGlobalLock.Call(h)
	if ptr == 0 {
		return "", false
	}
	defer pGlobalUnlock.Call(h)
	size, _, _ := pGlobalSize.Call(h)
	if size < 2 || size > 2<<20 {
		return "", false
	}
	u16 := unsafe.Slice((*uint16)(unsafe.Pointer(ptr)), size/2)
	n := 0
	for n < len(u16) && u16[n] != 0 {
		n++
	}
	return syscall.UTF16ToString(u16[:n]), true
}
