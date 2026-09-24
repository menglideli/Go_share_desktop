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
8. **pion 的 ICE 状态回调里绝不做会等待 ICE 的动作**（典型：同步 `peer.Close()`）——
   回调占着 ICE agent 的内部 goroutine，Close 又要等 agent 收尾 → 互等死锁，
   分享端**静默卡死**（无 panic 无日志，只有后续接入超时能看出）。清理一律 `go cleanup(...)`。
9. **幂等别用 `sync.Once`**：`cleanup → Server.Drop → closeFn → cleanup` 是同 goroutine 重入，
   `once.Do` 永久阻塞。用 `mu + bool flag`。
10. **别持锁回调**：先摘记录、解锁、再回调。`Drop`（移除+回调）与 `Detach`（只移除）语义要分开，
    否则形成 Drop→closeFn→cleanup 的回环。
11. **DXGI 首帧可能是未初始化黑帧**，必须预热丢弃（`Source.Warmup()`，有限次数+超时），
    且预热期**不更新 `Last()`**——否则静止桌面会把黑帧当心跳发出去，观众永远等不到画面。
12. **验证接入功能时，长生命周期进程用 `run_in_background` 起**。用 `cmd &` 起的子进程
    会在 Bash 工具调用返回时被回收，导致"并发在线"之类的验证目标静默失效。
13. **锁屏时 `cmd/shot` 截到的是锁屏画面（纯蓝）**，像素证据无效 —— "有像素证据"与
    "像素证据有效"是两件事。

## 文档入口

| 文件 | 内容 |
|---|---|
| `PLAN.md` | 计划 + 风险登记 R1~R28 + 各阶段已知约束 |
| `VERIFY.md` | 阶段 0 技术验证报告 + 阶段 2 修复记录（第 10 节）+ 阶段 3 验证（第 11 节） |
| `.workbuddy/memory/YYYY-MM-DD.md` | 每日工作日志（追加写） |

## 模块地图

| 包 | 职责 |
|---|---|
| `internal/capture` | DXGI/GDI 采集、区域裁切、自绘光标、DPI、首帧预热 |
| `internal/codec` | BGRA→I420 转换、MJPEG 条带并行编解码（dirty tile 增量） |
| `internal/rtc` | pion Peer 封装、分片重组、背压丢帧、等待候选收齐 |
| `internal/pipeline` | 采集→编码→发送管线，四档预设（最大/流畅60/流畅30/急速） |
| `internal/ui` | Gio 预览窗（分享端）与观看窗（观看端） |
| `internal/netif` | 多网卡枚举 + 可分享地址排序 |
| `internal/signal` | HTTP 信令、授权码、观众管理（Drop/Detach） |
| `internal/discover` | UDP 广播自动发现 |
| `internal/clip` | 剪贴板一键复制（纯 syscall） |

## 常用验证工具（`cmd/`）

| 工具 | 用途 |
|---|---|
| `capcheck` / `codeccheck` / `netcheck` | 采集（12 项）/ 编解码 / 端到端 自检 |
| `sigcheck` | 信令自检（29 项） |
| `sigdemo` | 端到端接入演示：`-mode host` / `-mode watch -addr|‑scan` |
| `shot` | 截指定显示器存 PNG |
| `pixprobe` | 逐环节统计非黑占比/平均亮度，定位"黑在哪一环" |
| `viewertest` | 隔离渲染层与数据层（`-minimal` 走最小循环对照） |
| `sharedemo` | 真实链路演示（默认采集副屏、观看窗在主屏），`-diag` 开像素诊断 |
