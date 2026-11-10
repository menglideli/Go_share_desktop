package rtc

import (
	"bytes"
	"testing"

	"goshare/internal/codec"
)

// TestFECRecovery 在 rtc 层确定性地验证 P1-7 的恢复路径：
// 三个数据块 + 一个 XOR 校验块，丢掉其中一块，onMedia 必须用校验把它补回来，
// 并且补回来的字节与原始分块**逐字节一致**（否则解码会花屏而不是"缺一块"）。
func TestFECRecovery(t *testing.T) {
	base := codec.Frame{
		Seq: 7, TS: 1000, W: 320, H: 240, TileH: 32, TotalTiles: 3,
		ChunkTotal: 3, FEC: true,
	}
	mk := func(idx int, data string) []byte {
		f := base
		f.ChunkIdx = uint8(idx)
		f.Tiles = []codec.Tile{{Index: idx, Y0: idx * 32, Data: []byte(data)}}
		b, err := f.Marshal()
		if err != nil {
			t.Fatalf("marshal %d: %v", idx, err)
		}
		return b
	}
	chunks := [][]byte{mk(0, "aaaa"), mk(1, "bbbbbb"), mk(2, "cc")}

	parity, _ := codec.XORParity(chunks)
	lens := []uint32{uint32(len(chunks[0])), uint32(len(chunks[1])), uint32(len(chunks[2]))}
	fec := MarshalFEC(FECGroup{
		Seq: 7, TS: 1000, Group: 0, DataCount: 3, Lengths: lens, Parity: parity,
	})

	p := &Peer{fecFrames: map[uint64]*fecFrame{}, frameIn: make(chan []byte, 4)}
	// 丢第 1 块：只送 0、2，再送校验。
	p.onMedia(chunks[0])
	p.onMedia(chunks[2])
	// 正常分块本身也会进邮箱：先清空，留下的才是"恢复出来的那块"。
	for len(p.frameIn) > 0 {
		<-p.frameIn
	}
	p.onMedia(fec)

	select {
	case got := <-p.frameIn:
		if !bytes.Equal(got, chunks[1]) {
			t.Fatalf("恢复的分块与原始不一致（%d vs %d 字节）", len(got), len(chunks[1]))
		}
		if p.Stats().FECRecovered != 1 {
			t.Fatalf("FECRecovered = %d，期望 1", p.Stats().FECRecovered)
		}
	default:
		t.Fatal("没有产出恢复的分块")
	}
}

// TestFECNoRecoverWhenTwoMissing：一组丢两块时 XOR 救不了，绝不能产出垃圾块。
func TestFECNoRecoverWhenTwoMissing(t *testing.T) {
	base := codec.Frame{Seq: 9, W: 320, H: 240, TileH: 32, TotalTiles: 3, ChunkTotal: 3, FEC: true}
	mk := func(idx int, data string) []byte {
		f := base
		f.ChunkIdx = uint8(idx)
		f.Tiles = []codec.Tile{{Index: idx, Data: []byte(data)}}
		b, _ := f.Marshal()
		return b
	}
	chunks := [][]byte{mk(0, "aa"), mk(1, "bb"), mk(2, "cc")}
	parity, _ := codec.XORParity(chunks)
	fec := MarshalFEC(FECGroup{Seq: 9, Group: 0, DataCount: 3,
		Lengths: []uint32{uint32(len(chunks[0])), uint32(len(chunks[1])), uint32(len(chunks[2]))}, Parity: parity})

	p := &Peer{fecFrames: map[uint64]*fecFrame{}, frameIn: make(chan []byte, 4)}
	p.onMedia(chunks[0]) // 丢两块
	for len(p.frameIn) > 0 {
		<-p.frameIn
	}
	p.onMedia(fec)
	select {
	case <-p.frameIn:
		t.Fatal("丢两块时不应产出恢复块")
	default:
	}
}
