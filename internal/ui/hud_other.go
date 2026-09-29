//go:build !windows

package ui

import (
	"image"
	"log"
)

// 非 Windows 平台暂无实现（本项目只发 Windows 版）：悬浮条仍可用，
// 只是不置顶、位置由系统决定。
func placeAndVerifyHUD(title string, size image.Point) {
	log.Printf("hud: 非 Windows 平台，跳过置顶与初始摆放")
}

func rememberHUDPos(title string) {}
