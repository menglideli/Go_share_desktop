package rtc

import "testing"

// TestFECRoundTrip 钉住校验块的字节布局与"非 FEC 消息"的判定。
func TestFECRoundTrip(t *testing.T) {
	in := FECGroup{Seq: 42, TS: 123456, Group: 3, DataCount: 8,
		Lengths: []uint32{10, 20, 30}, Parity: []byte{1, 2, 3, 4}}
	got, ok, err := ParseFEC(MarshalFEC(in))
	if err != nil || !ok {
		t.Fatalf("解析失败: ok=%v err=%v", ok, err)
	}
	if got.Seq != in.Seq || got.Group != in.Group || got.DataCount != in.DataCount || len(got.Parity) != 4 {
		t.Fatalf("头不一致: %+v", got)
	}
	for i := range in.Lengths {
		if got.Lengths[i] != in.Lengths[i] {
			t.Fatalf("长度表不一致: %v", got.Lengths)
		}
	}
	// 普通分块必须被判成"不是 FEC"，不能误吞。
	if _, ok, _ := ParseFEC([]byte("GSJ2rest")); ok {
		t.Fatal("普通分块被误判成 FEC")
	}
	// 截断的 FEC 要报错（不能把半截校验拿去恢复）。
	if _, ok, err := ParseFEC(MarshalFEC(in)[:20]); !ok || err == nil {
		t.Fatal("截断的 FEC 应报错")
	}
}
