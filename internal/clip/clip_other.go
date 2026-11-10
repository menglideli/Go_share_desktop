//go:build !windows

package clip

import "errors"

// SetText 在非 Windows 平台没有实现（本项目目标平台是 Windows）。
func SetText(string) error { return errors.New("clip: 当前平台不支持剪贴板") }

// GetText 在非 Windows 平台不可用。
func GetText() (string, bool) { return "", false }

// ClipSeq 在非 Windows 平台恒为 0。
func ClipSeq() uint32 { return 0 }
