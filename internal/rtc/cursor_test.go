package rtc

import "testing"

// TestCursorShapeRoundTrip 钉住光标形状消息的字节布局（P0-3）。
//
// 布局写错在单机测试里"看起来能用"，只有跨端才暴露 —— 和时钟 payload 一样，
// 顺序/宽度都属于线格式契约。
func TestCursorShapeRoundTrip(t *testing.T) {
	w, h := 3, 2
	pix := make([]byte, w*h*4)
	for i := range pix {
		pix[i] = byte(i)
	}
	in := CursorShape{ID: 0xDEADBEEF, W: w, H: h, HotX: 5, HotY: -3, Pix: pix}
	out, ok := ParseCursorShape(MarshalCursorShape(in))
	if !ok {
		t.Fatal("解析失败")
	}
	if out.ID != in.ID || out.W != in.W || out.H != in.H || out.HotX != in.HotX || out.HotY != in.HotY {
		t.Fatalf("头不一致: %+v", out)
	}
	if len(out.Pix) != len(pix) {
		t.Fatalf("位图长度 %d，期望 %d", len(out.Pix), len(pix))
	}
	for i := range pix {
		if out.Pix[i] != pix[i] {
			t.Fatalf("位图第 %d 字节不一致", i)
		}
	}
	// 长度与 w*h*4 不一致时必须拒绝（防止用坏包去申请大缓冲）。
	bad := MarshalCursorShape(in)
	bad[12+3] ^= 0xFF
	if _, ok := ParseCursorShape(bad[:16]); ok {
		t.Fatal("截断的位图不应被接受")
	}
}

// TestCursorPosRoundTrip 钉住位置消息（60Hz，25 字节）。
func TestCursorPosRoundTrip(t *testing.T) {
	in := CursorPos{TS: 123456, ID: 42, X: -1500, Y: 900, RX: 100, RY: 200, Vis: true}
	out, ok := ParseCursorPos(MarshalCursorPos(in))
	if !ok {
		t.Fatal("解析失败")
	}
	if out != in {
		t.Fatalf("往返不一致: %+v vs %+v", out, in)
	}
	// 不可见时 ID 无意义，但其余字段仍要能正常往返。
	hidden := in
	hidden.Vis = false
	if out2, ok := ParseCursorPos(MarshalCursorPos(hidden)); !ok || out2.Vis {
		t.Fatal("隐藏状态的可见位不对")
	}
	if _, ok := ParseCursorPos(make([]byte, 24)); ok {
		t.Fatal("长度不足 25 字节应解析失败")
	}
}

// TestHelloRoundTrip 钉住能力声明（未知字段要能忽略，方便以后加能力）。
func TestHelloRoundTrip(t *testing.T) {
	h, ok := ParseHello(MarshalHello(HelloInfo{Cursor: true}))
	if !ok || !h.Cursor {
		t.Fatalf("Hello 往返失败: %+v", h)
	}
	if h2, ok := ParseHello([]byte(`{"cursor":false,"future":1}`)); !ok || h2.Cursor {
		t.Fatal("未知字段应被忽略，cursor=false 应保持 false")
	}
}
