//go:build !windows

package tray

import "fmt"

// 非 Windows 平台：当前只支持 Windows（项目目标是内网 Windows 桌面）。
// 提供同名 stub，让调用方无需条件编译。

// Config 是托盘的初始配置（Windows 版见 tray_windows.go）。
type Config struct {
	Tooltip string
	OnShow  func()
	OnStop  func()
	OnQuit  func()
}

// Tray 是托盘图标实例。
type Tray struct{}

// New 在非 Windows 平台恒返回错误。
func New(Config) (*Tray, error) { return nil, fmt.Errorf("tray: 仅支持 Windows") }

// SetSharing 切换菜单状态（stub）。
func (t *Tray) SetSharing(bool, string) {}

// SetTooltip 更新提示（stub）。
func (t *Tray) SetTooltip(string) {}

// Balloon 弹气泡（stub）。
func (t *Tray) Balloon(string, string) {}

// Close 关闭（stub）。
func (t *Tray) Close() {}

// ShowMenuForTest 弹出菜单（stub）。
func (t *Tray) ShowMenuForTest() {}
