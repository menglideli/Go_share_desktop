//go:build !windows

package ui

// MinimizeMainWindow 在非 Windows 平台是空操作。
// 本项目只面向 Windows（采集走 DXGI/GDI），保留这个 stub 只是为了让
// "跨平台编译"和 `go vet ./...` 不至于因为缺符号而失败。
func MinimizeMainWindow() error { return nil }
