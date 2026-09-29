// 观看端画面缓冲池：把"每帧新分配 + 整帧拷贝 20MB"换成"固定两块复用 + 只同步变化过的条带"。
//
// 为什么值得单独一个文件：这段逻辑有两个容易写错的点，放在一起好交代 ——
//
//  1. **所有权**：交给界面渲染的那块，在界面上屏之前不能再被写（否则画面撕裂）。
//     所以缓冲在"池里"与"在界面上"之间流转，回来后才允许继续写。
//  2. **落后补偿**：一块缓冲在界面上待着的时候，解码画布还在继续更新。等它回到池里，
//     它已经落后了 —— 必须把"离开这段时间变化过的条带"补上。补的粒度是条带位图，
//     不是整帧，所以常态下每次只有几个条带（几十 KB）的拷贝。
//
// 实测背景：2K 一帧 NRGBA = 20MB。每帧新分配一次 = 600MB/s 的垃圾，
// 解码 goroutine 会被 GC 拖住 → 丢块丢帧；整帧拷贝本身也要 4~6ms。
package main

import (
	"image"

	"goshare/internal/codec"
)

// dispBufN 是池子大小：一块在界面上、一块在解码侧就够轮转。
const dispBufN = 2

// dispBuf 是一块可复用的画面缓冲。
type dispBuf struct {
	img   *image.NRGBA
	held  bool   // true = 已交给界面，不在池里
	fresh bool   // true = 还没同步过一次（需要整块来一份）
	dirty uint64 // 落后于解码画布的条带位图
}

// dispPool 管理观看端的画面缓冲。
//
// 并发约定：bufs / dirty / fresh 只由**解码 goroutine** 读写；
// 界面归还的缓冲经 back 通道跨界，由解码侧用 collect 收走。
type dispPool struct {
	bufs  []*dispBuf
	w, h  int
	tileH int
	nTile int
	back  chan *image.NRGBA
}

func newDispPool() *dispPool {
	return &dispPool{back: make(chan *image.NRGBA, dispBufN)}
}

// Reset 在分辨率（或条带切分）变化时重建池子 —— 旧尺寸的缓冲全部淘汰。
func (p *dispPool) Reset(w, h, tileH, nTiles int) {
	p.w, p.h, p.tileH, p.nTile = w, h, tileH, nTiles
	p.bufs = nil
	p.back = make(chan *image.NRGBA, dispBufN)
}

// Recycle 返回交给界面归还用的通道。
func (p *dispPool) Recycle() chan *image.NRGBA { return p.back }

// W / H 返回当前缓冲尺寸（0 表示尚未建立）。
func (p *dispPool) W() int { return p.w }
func (p *dispPool) H() int { return p.h }

// MarkDirty 记录"这些条带的内容变了"。
//
// ⚠️ 对**正在界面上显示**的那块也要标（更新的是表，不是像素）：
// 它回来时才知道该补哪些条带 —— 这是"不整帧拷贝"的关键。
func (p *dispPool) MarkDirty(mask uint64) {
	if mask == 0 {
		return
	}
	for _, b := range p.bufs {
		b.dirty |= mask
	}
}

// collect 收走界面还回来的缓冲（非阻塞）。
func (p *dispPool) collect() {
	for {
		select {
		case img := <-p.back:
			for _, b := range p.bufs {
				if b.img == img {
					b.held = false
					break
				}
			}
		default:
			return
		}
	}
}

// Take 取一块**已经与解码画布同步**的缓冲交给界面；没有可用缓冲时返回 nil
// （宁可这一帧不上屏 —— 界面显示的还是上一帧的完整画面，不闪不裂）。
func (p *dispPool) Take(src *codec.Decoder) *image.NRGBA {
	canvas := src.Image()
	if canvas == nil || p.w <= 0 {
		return nil
	}
	if canvas.Bounds().Dx() != p.w || canvas.Bounds().Dy() != p.h {
		return nil
	}
	p.collect()
	b := p.pick()
	if b == nil {
		return nil
	}
	mask := b.dirty
	if b.fresh {
		mask = allTiles(p.nTile)
		b.fresh = false
	}
	p.syncTiles(b.img, canvas, mask)
	b.dirty = 0
	b.held = true
	return b.img
}

// pick 找一块不在界面上的缓冲（不够就地分配）。
func (p *dispPool) pick() *dispBuf {
	for _, b := range p.bufs {
		if !b.held {
			return b
		}
	}
	if len(p.bufs) >= dispBufN {
		return nil
	}
	b := &dispBuf{img: image.NewNRGBA(image.Rect(0, 0, p.w, p.h)), fresh: true}
	p.bufs = append(p.bufs, b)
	return b
}

// syncTiles 把 src 中 mask 指定的条带行拷进 dst。
func (p *dispPool) syncTiles(dst, src *image.NRGBA, mask uint64) {
	if mask == 0 {
		return
	}
	stride := p.w * 4
	for i := 0; i < 64 && i < p.nTile; i++ {
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		y0 := i * p.tileH
		y1 := y0 + p.tileH
		if y1 > p.h {
			y1 = p.h
		}
		if y0 < 0 || y1 <= y0 || y1*stride > len(dst.Pix) || y1*stride > len(src.Pix) {
			continue
		}
		copy(dst.Pix[y0*stride:y1*stride], src.Pix[y0*stride:y1*stride])
	}
}

func allTiles(n int) uint64 {
	if n >= 64 {
		return ^uint64(0)
	}
	if n <= 0 {
		return 0
	}
	return 1<<uint(n) - 1
}
