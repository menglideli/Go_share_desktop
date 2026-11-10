// Package clip 负责系统剪贴板：写入文本（一键复制地址）与读取文本（剪贴板同步）。
//
// 写用 clip.exe：它就在系统目录里、不需要 CGO、不闪控制台窗口。
// 读用 PowerShell 的 Get-Clipboard：**不用 GlobalLock + unsafe.Pointer**，
// 因为 go vet 的 unsafeptr 检查会报"uintptr → unsafe.Pointer 可能误用"，
// 而它是 CI 门禁；起进程约 150ms，但只在剪贴板序号变化时才调用（见 ClipSeq）。
package clip

import (
	"os/exec"
	"strings"
	"syscall"
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
	user32c    = syscall.NewLazyDLL("user32.dll")
	pClipSeqNo = user32c.NewProc("GetClipboardSequenceNumber")
)

// ClipSeq 返回剪贴板序号（每次内容变化 +1）。
//
// 用它做"变了没有"的廉价判断：纯整数、无 unsafe，500ms 轮询几乎零成本；
// 只有序号变了才起 PowerShell 读文本。
func ClipSeq() uint32 {
	r, _, _ := pClipSeqNo.Call()
	return uint32(r)
}

// GetText 读取剪贴板里的文本。
func GetText() (string, bool) {
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command",
		"[Console]::OutputEncoding=[System.Text.Encoding]::UTF8; Get-Clipboard -Raw").Output()
	if err != nil {
		return "", false
	}
	text := strings.TrimRight(string(out), "\r\n")
	if text == "" {
		return "", false
	}
	return text, true
}
