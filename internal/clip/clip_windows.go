// Package clip 把文本放进系统剪贴板，给"一键复制地址"用。
//
// Windows 上直接调 clip.exe：它就在系统目录里、不需要 CGO、不会闪控制台窗口。
// 自己走 user32 的 OpenClipboard/SetClipboardData 当然也行，但要多写 60 行
// syscall 与内存锁定逻辑，收益只是少起一个进程 —— 复制是低频操作，不值得。
package clip

import (
	"os/exec"
	"strings"
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
