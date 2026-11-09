// 观看端呈现调度：把"收到就显示"改成"到点才显示"（P0-5 多端一致）。
//
// 为什么必须单独一个 goroutine：解码在 rtc 的 OnFrames 回调里同步做
// （2K 一帧 10~20ms），在那里等待会直接吃掉解码产能。这里只做定时与交接，
// 单次耗时是微秒级。
//
// 三条语义与 P0-5 的验收判据一一对应：
//
//  1. **按 due 排序的小队列**：每帧的 due 都等于 `帧 TS + D`（换算到本端时钟），
//     到点各自上屏，于是同一帧会在同一时刻出现在所有观众屏幕上。
//     ⚠️ 不能用"只留最新一帧"的单槽：新帧的 due 总是比上一帧晚一个帧间隔，
//     一覆盖就把计时器往后推，30fps + D=80ms 时**永远等不到到期**（实测画面
//     冻在某一帧，日志里"覆盖"一直涨而"上屏"不涨）。
//  2. **深度上限 3**：D 只铺得开 2~3 帧；再多就说明解码/网络已经落后，
//     这时丢最新的一帧（保时间轴单调），绝不排队等 —— 排队就是"延迟越拖越长"。
//  3. **迟到即上屏**：due 已过立刻交给界面，不补播旧帧。宁可跳一帧，
//     也不要让画面越落越远。
package main

import (
	"context"
	"image"
	"sync/atomic"
	"time"
)

// playoutDepth 是"等上屏"队列的深度上限。
//
// 3 的依据：D=80ms / 30fps ≈ 2.4 帧。再多只会在拥塞时变成额外延迟。
const playoutDepth = 3

// playoutItem 是一帧"已解码、等着按统一时间线上屏"的画面。
type playoutItem struct {
	img *image.NRGBA
	seq uint64
	ts  uint32 // 分享端的编码完成时刻（毫秒，低 32 位）
	due time.Time
}

// playout 是带小队列的呈现调度器。
type playout struct {
	in      chan playoutItem
	recycle chan *image.NRGBA
	// dropped 统计"因为队列满/来不及上屏被丢掉"的帧数：
	// D 偏小或解码跟不上时它会涨，是判断播放缓冲是否够用的直接指标。
	dropped atomic.Uint64
}

func newPlayout(recycle chan *image.NRGBA) *playout {
	return &playout{in: make(chan playoutItem, 1), recycle: recycle}
}

// push 交一帧待上屏（非阻塞）。缓冲所有权随之转交给调度器。
func (p *playout) push(it playoutItem) {
	select {
	case p.in <- it:
		return
	default:
	}
	// 槽里已经有一帧：丢掉更旧的那帧，换成最新的（调度器每 33ms 就会取走）。
	select {
	case old := <-p.in:
		p.release(old.img)
		p.dropped.Add(1)
	default:
	}
	select {
	case p.in <- it:
	default:
		// 极端并发下还是塞不进去（消费者刚好取走/又放回）：这一帧直接还池。
		p.release(it.img)
		p.dropped.Add(1)
	}
}

// release 把不再使用的画面缓冲还给解码侧（非阻塞：池满就交给 GC）。
func (p *playout) release(img *image.NRGBA) {
	if img == nil {
		return
	}
	select {
	case p.recycle <- img:
	default:
	}
}

// run 按 due 逐帧上屏，直到 ctx 结束。
func (p *playout) run(ctx context.Context, present func(playoutItem)) {
	var q []playoutItem
	for {
		now := time.Now()
		for len(q) > 0 && !q[0].due.After(now) {
			present(q[0])
			q = q[1:]
		}

		var timer *time.Timer
		var timerC <-chan time.Time
		if len(q) > 0 {
			timer = time.NewTimer(time.Until(q[0].due))
			timerC = timer.C
		}
		select {
		case it := <-p.in:
			if timer != nil {
				timer.Stop()
			}
			if len(q) >= playoutDepth {
				// 队列满了：丢这一帧（保时间轴单调，绝不把等待时间往后推）。
				p.release(it.img)
				p.dropped.Add(1)
				break
			}
			q = insertByDue(q, it)
		case <-timerC:
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		}
	}
}

// insertByDue 把一帧插到按 due 升序的位置（网络乱序窗口很小，线性插入足够）。
func insertByDue(q []playoutItem, it playoutItem) []playoutItem {
	i := len(q)
	for i > 0 && q[i-1].due.After(it.due) {
		i--
	}
	q = append(q, playoutItem{})
	copy(q[i+1:], q[i:])
	q[i] = it
	return q
}
