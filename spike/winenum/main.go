// Command winenum 枚举顶层窗口并打印 pid / 可见性 / owner / 标题。
//
// 用途：定位"为什么 ownMainWindow 找不到本进程的主窗"。
// 用法：go run ./spike/winenum [只看这个 pid]
package main

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const gwOwner = 4

var (
	user32       = windows.NewLazySystemDLL("user32.dll")
	procGetOwner = user32.NewProc("GetWindow")
	procGetText  = user32.NewProc("GetWindowTextW")
	procGetTextL = user32.NewProc("GetWindowTextLengthW")
	procIsIconic = user32.NewProc("IsIconic")
)

func main() {
	var only uint32
	if len(os.Args) > 1 {
		v, _ := strconv.Atoi(os.Args[1])
		only = uint32(v)
	}
	fmt.Printf("本进程 pid=%d  只看 pid=%d\n", os.Getpid(), only)
	n := 0
	cb := syscall.NewCallback(func(hwnd windows.HWND, _ uintptr) uintptr {
		var pid uint32
		windows.GetWindowThreadProcessId(hwnd, &pid)
		if only != 0 && pid != only {
			return 1
		}
		n++
		owner, _, _ := procGetOwner.Call(uintptr(hwnd), gwOwner)
		vis := windows.IsWindowVisible(hwnd)
		// ⚠️ 可见=true 不代表没最小化：最小化的窗口仍带 WS_VISIBLE。
		// 判断"有没有最小化"只能看 IsIconic。
		iconic, _, _ := procIsIconic.Call(uintptr(hwnd))
		fmt.Printf("  hwnd=%#x pid=%-7d 可见=%-5v 最小化=%-5v owner=%#-8x 标题=%q\n",
			uintptr(hwnd), pid, vis, iconic != 0, owner, text(hwnd))
		return 1
	})
	if err := windows.EnumWindows(cb, nil); err != nil {
		fmt.Fprintln(os.Stderr, "枚举失败:", err)
		os.Exit(1)
	}
	fmt.Printf("共 %d 个\n", n)
}

func text(h windows.HWND) string {
	l, _, _ := procGetTextL.Call(uintptr(h))
	if l == 0 {
		return ""
	}
	buf := make([]uint16, l+1)
	procGetText.Call(uintptr(h), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return syscall.UTF16ToString(buf)
}
