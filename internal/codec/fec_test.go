package codec

import (
	"bytes"
	"testing"
)

// TestXORParityRecover 覆盖"组内丢任意一块都能恢复"，包括长度不同的情况。
func TestXORParityRecover(t *testing.T) {
	chunks := [][]byte{
		[]byte("aaaa"),
		[]byte("bbbbbbbb"),
		[]byte("cc"),
	}
	par, n := XORParity(chunks)
	if n != 8 || len(par) != 8 {
		t.Fatalf("校验长度 = %d/%d，期望 8", n, len(par))
	}
	for miss := range chunks {
		others := make([][]byte, 0, len(chunks)-1)
		for i, c := range chunks {
			if i != miss {
				others = append(others, c)
			}
		}
		got, ok := XORRecover(others, par, len(chunks[miss]))
		if !ok {
			t.Fatalf("丢第 %d 块恢复失败", miss)
		}
		if !bytes.Equal(got, chunks[miss]) {
			t.Fatalf("丢第 %d 块恢复结果 %q，期望 %q", miss, got, chunks[miss])
		}
	}
	// wantLen 超过校验长度：拒绝，而不是返回垃圾。
	if _, ok := XORRecover(nil, par, len(par)+1); ok {
		t.Fatal("wantLen 越界应当失败")
	}
}
