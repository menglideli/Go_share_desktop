//go:build !windows

package ui

// AlertError 在非 Windows 平台是空操作（发布目标只有 Windows）。
func AlertError(title, msg string) {}
