// goshare — 内网局域网桌面共享客户端
//
// 阶段 1：先跑通「采集 + 本地预览」。后续阶段在此骨架上接入
// 编码传输（阶段 2）、信令与授权码（阶段 3）、完整双模式界面（阶段 4）。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"goshare/internal/capture"
	"goshare/internal/ui"
)

func main() {
	displayIdx := flag.Int("display", -1, "要采集的显示器序号（-1 = 主屏）")
	exitAfter := flag.Duration("exit", 0, "自动退出时长（用于自动化验证，如 8s）")
	noCursor := flag.Bool("no-cursor", false, "关闭鼠标光标叠加")
	logPath := flag.String("log", "", "日志文件路径（默认输出到控制台）")
	flag.Parse()

	if *logPath != "" {
		f, err := os.Create(*logPath)
		if err == nil {
			defer f.Close()
			log.SetOutput(f)
		}
	}

	// 必须在创建窗口、枚举显示器之前设置为 per-monitor DPI aware：
	// 否则高 DPI 屏上 GetCursorPos 返回逻辑像素，光标会画到错误位置。
	if lvl := capture.EnsureDPIAware(); lvl != 2 {
		log.Printf("警告: DPI 感知级别为 %d（期望 2），高 DPI 下坐标可能错位", lvl)
	}

	ctx := context.Background()
	ds, err := capture.Displays(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "枚举显示器失败: %v\n", err)
		os.Exit(1)
	}
	if len(ds) == 0 {
		fmt.Fprintf(os.Stderr, "未找到显示器\n")
		os.Exit(1)
	}

	sel := ds[0]
	for _, d := range ds {
		if d.Primary {
			sel = d
			break
		}
	}
	if *displayIdx >= 0 && *displayIdx < len(ds) {
		sel = ds[*displayIdx]
	}
	log.Printf("采集目标: %s", sel.String())

	err = ui.RunPreview(ctx, ui.PreviewConfig{
		Display:   sel,
		FPS:       30,
		Cursor:    !*noCursor,
		Title:     "GoShare · 本地预览",
		ExitAfter: *exitAfter,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "预览失败: %v\n", err)
		os.Exit(1)
	}
	_ = time.Now
}
