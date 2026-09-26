// traytest — 托盘的隔离验证工具（不进产品）。
//
// 验证点：
//  1. tray.New 成功（图标进通知区域，NIM_ADD 返回 TRUE）
//  2. SetSharing(true) 后菜单里「停止分享」可用
//  3. Balloon 气泡能发出去
//  4. ShowMenuForTest 弹出菜单（模态），配合 shot 截屏取证
//
// 用法：
//
//	go run ./cmd/traytest            # 建托盘 + 气泡，挂 12s（截托盘图标用）
//	go run ./cmd/traytest -menu      # 同上，并在 3s 后弹出右键菜单（截菜单用）
package main

import (
	"flag"
	"log"
	"time"

	"goshare/internal/tray"
)

func main() {
	menu := flag.Bool("menu", false, "3 秒后弹出右键菜单（模态，截屏用）")
	hold := flag.Duration("hold", 12*time.Second, "保持时长")
	flag.Parse()

	t, err := tray.New(tray.Config{
		Tooltip: "traytest · 托盘验证",
		OnShow:  func() { log.Printf("回调：OnShow（显示主窗口）") },
		OnStop:  func() { log.Printf("回调：OnStop（停止分享）") },
		OnQuit:  func() { log.Printf("回调：OnQuit（退出）") },
	})
	if err != nil {
		log.Fatalf("托盘创建失败: %v", err)
	}
	log.Printf("托盘已创建（图标应已出现在通知区域/溢出面板）")

	t.SetSharing(true, "traytest · 分享中（停止分享应可用）")
	t.Balloon("traytest 气泡", "如果你看到这条通知，Balloon 通路正常。")
	log.Printf("已 SetSharing(true) + Balloon")

	if *menu {
		go func() {
			time.Sleep(3 * time.Second)
			log.Printf("弹出右键菜单（模态）——菜单应出现在鼠标当前位置")
			t.ShowMenuForTest()
		}()
	}

	time.Sleep(*hold)
	t.Close()
	log.Printf("托盘已移除，退出")
}
