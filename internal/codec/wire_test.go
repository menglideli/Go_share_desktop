package codec

import (
	"testing"
)

// TestChunkRoundTrip 钉住 GSJ2 的分块语义（P4-2）。
//
// 一个消息 = 一个可独立解析的分块，且**条带绝不跨块**：接收端收到任意一块
// 都能解出它带的条带，丢一块只丢那几条。这条契约一旦被破坏（比如按字节数
// 硬切），丢一块就会把两条带各截成半张 JPEG —— 表现为"花屏"，而不是"缺一块"。
func TestChunkRoundTrip(t *testing.T) {
	enc := NewEncoder(Config{Quality: 60, TileHeight: 32, Dirty: true})
	w, h := 320, 600
	img := make([]byte, w*h*4)
	for i := 3; i < len(img); i += 4 {
		img[i] = 255 // 不透明黑
	}
	f, err := enc.Encode(img, w, h)
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	parts := f.Split(MaxChunkPayload)
	if len(parts) == 0 {
		t.Fatal("Split 返回 0 块")
	}
	var gotTiles int
	for i, c := range parts {
		wire, err := c.Marshal()
		if err != nil {
			t.Fatalf("第 %d 块 Marshal 失败: %v", i, err)
		}
		back, err := UnmarshalFrame(wire)
		if err != nil {
			t.Fatalf("第 %d 块反解失败: %v", i, err)
		}
		// 每块都要自带完整帧头：seq/尺寸/条带总数/掩码一致。
		if back.Seq != f.Seq || back.W != f.W || back.H != f.H || back.TotalTiles != f.TotalTiles {
			t.Fatalf("第 %d 块帧头不一致: %+v vs %+v", i, back, f)
		}
		if int(back.ChunkTotal) != len(parts) || int(back.ChunkIdx) != i {
			t.Fatalf("第 %d 块序号 = %d/%d，期望 %d/%d", i, back.ChunkIdx, back.ChunkTotal, i, len(parts))
		}
		if len(back.Tiles) == 0 {
			t.Fatalf("第 %d 块没有条带", i)
		}
		gotTiles += len(back.Tiles)
	}
	if gotTiles != f.TotalTiles {
		t.Fatalf("分块条带合计 %d，期望 %d（有条带被拆开或丢失）", gotTiles, f.TotalTiles)
	}
}

// TestRepairTilesIsolation 验证"点名补哪几条，就只带哪几条"（R39 的精确补发）。
func TestRepairTilesIsolation(t *testing.T) {
	enc := NewEncoder(Config{Quality: 60, TileHeight: 32, Dirty: true})
	w, h := 320, 600
	img := make([]byte, w*h*4)
	for i := 3; i < len(img); i += 4 {
		img[i] = 255
	}
	if _, err := enc.Encode(img, w, h); err != nil {
		t.Fatalf("首帧失败: %v", err)
	}
	// 只请求 0 和 2 两条（无变化帧），编码器应把它们补出来。
	enc.RequestTiles(1<<0 | 1<<2)
	f, err := enc.Encode(img, w, h)
	if err != nil {
		t.Fatalf("补发帧失败: %v", err)
	}
	mask := uint64(0)
	for _, tile := range f.Tiles {
		mask |= 1 << uint(tile.Index)
	}
	if mask != 1<<0|1<<2 {
		t.Fatalf("补发帧带了条带位图 %#x，期望 %#x", mask, uint64(1<<0|1<<2))
	}
	// 补出来的数据要能正常解码（不是空壳）。
	if f.Bytes <= 0 {
		t.Fatal("补发帧字节数为 0")
	}
	// 真正解一遍：补发的条带必须能落进画布（不是空壳）。
	dec := NewDecoder(0)
	if _, err := dec.Decode(f); err != nil {
		t.Fatalf("补发帧解码失败: %v", err)
	}
	if dec.Missing()&(1<<0|1<<2) != 0 {
		t.Fatalf("补发后条带仍缺: %#x", dec.Missing())
	}
}
