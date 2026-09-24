# GoShare · 长期项目记忆

内网桌面共享客户端：Go + Gio（UI）+ pion/webrtc（传输）+ 标准库 MJPEG（编码）。
工作区 `D:\sihuo-project\Go_share_desktop`，**有 git 仓库**（main + origin/main，阶段 0/1/2 均已提交）。

## 硬约束（改代码前必看 `PLAN.md` 第 9 节完整版）

1. **画面类改动必须用像素证据收口**：`cmd/shot` 截图 + `cmd/pixprobe` 逐环节统计。
   日志里的"渲染了 N 帧（画面 true）"和 `visible=1` **在画面被完全盖住时依然通过**——
   R24（观看窗全黑）就是这样穿过了整个阶段 2 的验证。
2. **Gio 的 `paint.PaintOp` 填充的是"当前裁剪区域"**，`layout.Rigid` 不会替你设裁剪。
   面板/状态栏背景必须"先量高度再按高度 clip"，否则色块铺满整窗、盖掉视频画面。
   同理：`gtx.Constraints.Max` 在 Rigid 里是整窗尺寸，不能当子项高度用。
3. **禁用 `screencapture.Options.ShowsCursor`** —— 会强制回退 GDI，采集 4.8ms → 58.3ms。光标一律自绘。
4. **`capture.EnsureDPIAware()` 必须在枚举显示器、创建窗口之前调用**，否则 `GetCursorPos`
   返回逻辑像素、光标错位一个 scale（本机 1.75 倍），而且错误完全自洽、极难发现。
5. **不要从外部 `SetWindowPos` 移动 Gio 窗口** —— 移动后不再产生帧事件（实测停在 10 帧）。
   演示用「采集副屏、观看窗留主屏」规避自摄入。
6. 性能测量必须用**变化的帧**（喂同一帧会让编码器大量 skip，得出乐观数字）；
   模拟桌面变化要用结构化内容（文本/窗口/色块），不要用随机噪声。
7. 构建 `screencapture` 需 `GOSUMDB=sum.golang.google.cn`。

## 文档入口

| 文件 | 内容 |
|---|---|
| `PLAN.md` | 计划 + 风险登记 R1~R26 + 各阶段已知约束 |
| `VERIFY.md` | 阶段 0 技术验证报告 + 阶段 2 修复记录（第 10 节） |
| `.workbuddy/memory/YYYY-MM-DD.md` | 每日工作日志（追加写） |

## 常用验证工具（`cmd/`）

| 工具 | 用途 |
|---|---|
| `capcheck` / `codeccheck` / `netcheck` | 采集 / 编解码 / 端到端 自检 |
| `shot` | 截指定显示器存 PNG |
| `pixprobe` | 逐环节统计非黑占比/平均亮度，定位"黑在哪一环" |
| `viewertest` | 隔离渲染层与数据层（`-minimal` 走最小循环对照） |
| `sharedemo` | 真实链路演示（默认采集副屏、观看窗在主屏），`-diag` 开像素诊断 |
