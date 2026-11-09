// P0-5 多端一致性的胶水：统一上屏、落后回报、分享端汇总。
//
// 三条链路合起来才叫"同步观看"：
//
//	观众 → 分享端  ctl ping/pong      → 时钟偏移（clock.go）
//	观众本地       ts + D 到期才上屏   → 同一时刻显示同一帧（playout.go）
//	观众 → 分享端  ctl 统计（500ms 对齐）→ 谁掉队一眼可见
//
// 这里只做接线与小计算，真正的定时/解码分别在 playout.go 与 main.go 里。
package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"goshare/internal/rtc"
	"goshare/internal/ui"
)

// presentFrame 是播放调度器的上屏回调（跑在 playout 的 goroutine 上）。
//
// 它做的三件事顺序不能反：先记账（seq/late 是"这一刻显示了什么"的证据），
// 再交给界面（所有权一并转交，界面用完会经 Recycle 还池），最后按 500ms 桶
// 回报给分享端。
func (a *app) presentFrame(it playoutItem) {
	late := time.Since(it.due).Milliseconds()
	if late < 0 {
		late = 0
	}
	a.wDispSeq.Store(it.seq)
	a.wDispLate.Store(late)
	a.shell.PushFrame(it.img)
	a.reportShown(it.seq, it.ts, late)
}

// reportShown 把"当前显示帧号 / 该帧的编码时刻 / 迟到毫秒"回报给分享端。
//
// 采样时刻按**分享端时钟**的 500ms 桶对齐：三个观众即使各自 0.5 秒报一次，
// 也会在同一个桶里采样，分享端拿到的 seq 才是"同一时刻"的，可以直接比较。
func (a *app) reportShown(seq uint64, ts uint32, lateMS int64) {
	if a.playoutMS <= 0 {
		return
	}
	off, ok := a.clock.Offset()
	if !ok {
		// 时钟没同步上也照报，但用本地时钟对齐 —— 分享端只能看"大致的落后"，
		// 不能做严格同帧比较。这比完全不报更有用（至少能看到谁掉队）。
		off = 0
	}
	at := uint32(int64(time.Now().UnixMilli()) + int64(off))
	bucket := at / 500
	if bucket == a.wRepBucket {
		return
	}
	a.wRepBucket = bucket

	p := a.peerBox.Load()
	if p == nil {
		return
	}
	b, err := json.Marshal(viewerStat{Seq: seq, TS: ts, LateMS: lateMS, DMS: a.playoutMS, At: at})
	if err != nil {
		return
	}
	_ = p.SendControl(rtc.CtlStats, b)
}

// pingClock 发一次时钟同步请求（观众端）。
func (a *app) pingClock(p *rtc.Peer) {
	if p == nil {
		return
	}
	t0 := uint32(time.Now().UnixMilli())
	_ = p.SendControl(rtc.CtlTimePing, rtc.TimePayload(t0, 0, 0, 0))
}

// parseViewerStat 解析观众回报的 CtlStats payload。
func parseViewerStat(payload []byte) (viewerStat, bool) {
	var st viewerStat
	if err := json.Unmarshal(payload, &st); err != nil {
		return viewerStat{}, false
	}
	return st, true
}

func (a *app) setViewerStat(token string, st viewerStat) {
	st.recvAt = time.Now()
	a.vstatMu.Lock()
	if a.vstat == nil {
		a.vstat = make(map[string]viewerStat)
	}
	a.vstat[token] = st
	a.vstatMu.Unlock()
}

// viewerStatOf 取某观众最近一次的回报；超过 3 秒没更新视为未知（别拿旧值误导人）。
func (a *app) viewerStatOf(token string) (viewerStat, bool) {
	a.vstatMu.Lock()
	defer a.vstatMu.Unlock()
	st, ok := a.vstat[token]
	if !ok || time.Since(st.recvAt) > 3*time.Second {
		return viewerStat{}, false
	}
	return st, true
}

func (a *app) clearViewerStat(token string) {
	a.vstatMu.Lock()
	delete(a.vstat, token)
	a.vstatMu.Unlock()
}

// syncLine 把若干观众的"显示帧号 / 迟到 / 落后帧数"拼成一条日志证据。
//
// 这是 P0-5 验收判据的取数方式：同一行里的 seq 来自同一批 500ms 桶采样，
// 它们相等（或差 1 帧）就说明多端确实在同一时刻显示同一帧。
func syncLine(rows []ui.ViewerRow) string {
	var b strings.Builder
	n := 0
	for _, r := range rows {
		if !r.SyncKnown {
			continue
		}
		if n > 0 {
			b.WriteString(" · ")
		}
		fmt.Fprintf(&b, "%s #%d 晚%dms 落后%dms", r.Name, r.Seq, r.LateMS, r.BehindMS)
		n++
	}
	return b.String()
}
