package codec

import "testing"

// TestDownscaleBGRA 验证盒式缩放：尺寸取偶、通道平均正确、非法输入报错。
func TestDownscaleBGRA(t *testing.T) {
	// 4×4 纯色（BGRA = 10,20,30,255）→ 2×2，颜色必须保持不变。
	w, h := 4, 4
	src := make([]byte, w*h*4)
	for i := 0; i < len(src); i += 4 {
		src[i], src[i+1], src[i+2], src[i+3] = 10, 20, 30, 255
	}
	dst, ow, oh, err := DownscaleBGRA(src, w, h, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if ow != 2 || oh != 2 || len(dst) != 2*2*4 {
		t.Fatalf("尺寸 = %dx%d（%d 字节），期望 2x2/16", ow, oh, len(dst))
	}
	if dst[0] != 10 || dst[1] != 20 || dst[2] != 30 || dst[3] != 255 {
		t.Fatalf("纯色缩放后颜色变了: %v", dst[:4])
	}
	// 2/3 比例：1800 → 1200，1080 → 720，且都是偶数。
	if _, ow, oh, err := DownscaleBGRA(make([]byte, 320*1800*4), 320, 1800, 2, 3); err != nil || ow != 212 || oh != 1200 {
		t.Fatalf("2/3 缩放 = %dx%d（err=%v），期望 212x1200", ow, oh, err)
	}
	// 非法输入
	if _, _, _, err := DownscaleBGRA(src, w, h, 2, 2); err == nil {
		t.Fatal("num >= den 应当报错")
	}
	if _, _, _, err := DownscaleBGRA(src[:4], w, h, 1, 2); err == nil {
		t.Fatal("输入过短应当报错")
	}
}
