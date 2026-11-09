// 观看端时钟同步：估计"分享端时钟 - 本端时钟"的偏移与 ctl 往返时延。
//
// 为什么需要它：P0-5 要求多个观众在同一时刻显示同一帧，而每帧自带的 TS 是
// **分享端**的时钟。只有把分享端时间轴换算到本端时钟，才能算出"这一帧应该
// 在本地几点几毫秒上屏"。
//
// 方法就是 NTP 的四时间戳 + 最小 RTT 过滤：ctl 是可靠有序通道，排队会让某次
// 样本的 RTT 偏大（偏移估计随之失真）；取最近 20 秒内 RTT 最小的那个样本，
// 它的排队延迟最小、偏移最可信。局域网实测偏差 < 1ms。
//
// 放在 rtc 包里是为了让自检工具（cmd/netcheck）能跑**同一份**估计代码 ——
// 复制一份到测试里，测的就不是生产路径了。
package rtc

import (
	"sync"
	"time"
)

// Clock 保存时钟偏移的估计值（并发安全）。
type Clock struct {
	mu      sync.Mutex
	bestRTT int32 // 目前最好的样本 RTT（毫秒）
	bestOff int32 // 对应样本算出的偏移（分享端 - 本端，毫秒）
	bestAt  time.Time
	samples int
	lastAt  time.Time
}

// Observe 消化一次 ping/pong 往返。
//
// t0 = 本端发出，t1 = 分享端收到，t2 = 分享端回复，t3 = 本端收到。
// 所有差值都用 uint32 环绕运算再转 int32 —— 与 Frame.TS 的 32 位语义一致。
func (c *Clock) Observe(t0, t1, t2, t3 uint32) {
	rtt := int32((t3 - t0) - (t2 - t1))
	if rtt < 0 {
		// 时间戳自相矛盾（时钟跳变 / 坏样本）：丢掉，不要污染估计。
		return
	}
	off := (int32(t1-t0) + int32(t2-t3)) / 2
	now := time.Now()

	c.mu.Lock()
	defer c.mu.Unlock()
	c.samples++
	c.lastAt = now
	// 最小 RTT 过滤：只有更优的样本，或旧样本已经过期（时钟漂移）才采纳。
	if c.samples == 1 || rtt < c.bestRTT || now.Sub(c.bestAt) > 20*time.Second {
		c.bestRTT, c.bestOff, c.bestAt = rtt, off, now
	}
}

// Offset 返回（分享端 - 本端）毫秒偏移，以及当前是否已有可信估计。
func (c *Clock) Offset() (int32, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// 至少两个样本才好信；超过 10 秒没有新样本（ctl 堵了 / 对端不响应）就退回不同步。
	if c.samples < 2 || time.Since(c.lastAt) > 10*time.Second {
		return 0, false
	}
	return c.bestOff, true
}

// RTT 返回当前估计的 ctl 往返（毫秒），仅用于诊断。
func (c *Clock) RTT() int32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bestRTT
}

// Wait 返回距离"该帧应在本地时钟上屏"还有多久；<=0 表示已经晚了。
//
// d 是播放缓冲（P0-5 的 D）。没有同步上的时候返回 false，调用方应当立即上屏 ——
// 宁可不同步，也不能把画面扣住不放。
func (c *Clock) Wait(ts uint32, d time.Duration) (time.Duration, bool) {
	off, ok := c.Offset()
	if !ok {
		return 0, false
	}
	delta := waitMS(uint32(time.Now().UnixMilli()), ts, uint32(off), int64(d/time.Millisecond))
	// 超过 ±5 秒说明估计已经不可信（时钟被改过 / 样本坏了）。
	// 这时必须立刻上屏：用一个坏偏移把画面临时扣住，表现为"界面卡死"。
	if delta < -5000 || delta > 5000 {
		return 0, false
	}
	return time.Duration(delta) * time.Millisecond, true
}

// waitMS 是 Wait 的纯计算部分：now/ts/off 都是毫秒，off = 分享端 - 本端。
//
// 全程 uint32 环绕 + int32 解释：Frame.TS 只有 32 位，两端墙钟不需要一致，
// 只要差值在 ±24.8 天内就是准的。抽出来单独测，避免"只有真机跑一夜才回归"。
func waitMS(now, ts, off uint32, d int64) int32 {
	due := uint32(int64(ts)+d) - off // 换到本端时钟
	return int32(due - now)
}
