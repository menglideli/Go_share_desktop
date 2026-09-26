//go:build windows

// clicker — 在指定物理像素坐标模拟一次鼠标左键点击（验证辅助探针）。
//
// 用途：展开托盘溢出面板这类"必须点一下才能截屏取证"的场景。
// 坐标是**物理像素**（进程先 EnsureDPIAware，不会被虚拟化）。
package main

import (
	"flag"
	"log"
	"syscall"
	"time"

	"goshare/internal/capture"
)

func main() {
	x := flag.Int("x", 0, "物理像素 X")
	y := flag.Int("y", 0, "物理像素 Y")
	flag.Parse()

	capture.EnsureDPIAware()
	u := syscall.NewLazyDLL("user32.dll")
	scp := u.NewProc("SetCursorPos")
	me := u.NewProc("mouse_event")
	if r, _, err := scp.Call(uintptr(*x), uintptr(*y)); r == 0 {
		log.Fatalf("SetCursorPos(%d,%d) 失败: %v", *x, *y, err)
	}
	time.Sleep(300 * time.Millisecond)
	const leftDown, leftUp = 0x2, 0x4
	me.Call(leftDown, 0, 0, 0, 0)
	me.Call(leftUp, 0, 0, 0, 0)
	log.Printf("已点击 (%d,%d)", *x, *y)
}
