package rtc

import "testing"

// TestTimePayloadRoundTrip 钉住时钟同步 payload 的字节布局。
//
// 为什么值得单测：四个时间戳的**顺序**是跨版本兼容的一部分（旧端会忽略这类
// ctl 消息，新端之间必须完全一致）。顺序写错在单机上测不出来 ——
// 两端都错会互相抵消成"偏移 0"，看起来一切正常，只有跨机器才暴露。
func TestTimePayloadRoundTrip(t *testing.T) {
	b := TimePayload(0x01020304, 0xAABBCCDD, 1, 0xFFFFFFFF)
	if len(b) != 16 {
		t.Fatalf("payload 长度 = %d，期望 16", len(b))
	}
	t0, t1, t2, t3, ok := ParseTimePayload(b)
	if !ok {
		t.Fatal("解析失败")
	}
	if t0 != 0x01020304 || t1 != 0xAABBCCDD || t2 != 1 || t3 != 0xFFFFFFFF {
		t.Fatalf("往返不一致: %08x %08x %08x %08x", t0, t1, t2, t3)
	}
}

func TestParseTimePayloadShort(t *testing.T) {
	if _, _, _, _, ok := ParseTimePayload(make([]byte, 15)); ok {
		t.Fatal("长度不足 16 字节时应当解析失败")
	}
}
