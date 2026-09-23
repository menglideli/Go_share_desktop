package capture

import (
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// 本文件实现鼠标光标自绘。
//
// 为什么必须自己做：阶段 0 实测，screencapture 的 Options.ShowsCursor 会
// 强制后端从 DXGI Duplication 回退到 GDI，采集耗时 4.8ms → 58.3ms（12 倍），
// 帧率掉到 17fps。所以禁用该选项，改为拿到 DXGI 帧后自己把光标合成上去。

const (
	cursorShowing = 0x00000001
	dibRGBColors  = 0
)

var (
	user32 = windows.NewLazySystemDLL("user32.dll")
	gdi32  = windows.NewLazySystemDLL("gdi32.dll")
	shcore = windows.NewLazySystemDLL("shcore.dll")

	pGetCursorInfo    = user32.NewProc("GetCursorInfo")
	pGetIconInfo      = user32.NewProc("GetIconInfo")
	pGetObjectW       = gdi32.NewProc("GetObjectW")
	pGetDIBits        = gdi32.NewProc("GetDIBits")
	pCreateCompatDC   = gdi32.NewProc("CreateCompatibleDC")
	pDeleteDC         = gdi32.NewProc("DeleteDC")
	pDeleteObject     = gdi32.NewProc("DeleteObject")
	pGetProcessDpiAwr = shcore.NewProc("GetProcessDpiAwareness")
	pSetProcessDpiAwr = shcore.NewProc("SetProcessDpiAwareness")
)

const processPerMonitorDPIAware = 2

type point struct{ X, Y int32 }

type cursorInfo struct {
	CbSize      uint32
	Flags       uint32
	HCursor     windows.Handle
	PtScreenPos point
}

type iconInfo struct {
	FIcon    int32
	XHotspot uint32
	YHotspot uint32
	HbmMask  windows.Handle
	HbmColor windows.Handle
}

type gdiBitmap struct {
	Type       int32
	Width      int32
	Height     int32
	WidthBytes int32
	Planes     uint16
	BitsPixel  uint16
	Bits       uintptr
}

type bitmapInfoHeader struct {
	BiSize          uint32
	BiWidth         int32
	BiHeight        int32
	BiPlanes        uint16
	BiBitCount      uint16
	BiCompression   uint32
	BiSizeImage     uint32
	BiXPelsPerMeter int32
	BiYPelsPerMeter int32
	BiClrUsed       uint32
	BiClrImportant  uint32
}

// cursorArt 是解码后的光标位图：紧凑 BGRA，top-down，带 alpha。
type cursorArt struct {
	w, h      int
	hotX      int
	hotY      int
	pix       []byte // w*h*4, BGRA
	monochrome bool
}

var (
	cursorMu    sync.Mutex
	cursorCache = map[windows.Handle]*cursorArt{}

	// 共享 DC 必须用独立的 once：dc() 是在已持有 cursorMu 的情况下被调用的
	// （getArt → decodeCursor → dc），若这里也抢 cursorMu 会立刻自锁。
	dcOnce   sync.Once
	sharedDC windows.Handle
)

// CursorPos 返回光标在虚拟屏幕坐标系中的位置（物理像素）。
// 若进程非 DPI aware，GetCursorPos 返回的是逻辑像素，需按 DPI 放大。
func CursorPos() (x, y int, visible bool) {
	var ci cursorInfo
	ci.CbSize = uint32(unsafe.Sizeof(ci))
	r, _, _ := pGetCursorInfo.Call(uintptr(unsafe.Pointer(&ci)))
	if r == 0 {
		return 0, 0, false
	}
	vis := ci.Flags&cursorShowing != 0 && ci.HCursor != 0
	return int(ci.PtScreenPos.X), int(ci.PtScreenPos.Y), vis
}

// CursorHandle 返回当前光标句柄（用于缓存键）。
func CursorHandle() windows.Handle {
	var ci cursorInfo
	ci.CbSize = uint32(unsafe.Sizeof(ci))
	r, _, _ := pGetCursorInfo.Call(uintptr(unsafe.Pointer(&ci)))
	if r == 0 {
		return 0
	}
	return ci.HCursor
}

// CursorShape 描述当前光标的尺寸与热点，用于诊断与测试断言。
type CursorShape struct {
	W, H       int
	HotX, HotY int
	Monochrome bool
	Visible    bool
}

// CursorShapeOf 返回当前光标形状。Visible 为 false 表示光标未显示（如全屏应用）。
func CursorShapeOf() CursorShape {
	var ci cursorInfo
	ci.CbSize = uint32(unsafe.Sizeof(ci))
	if r, _, _ := pGetCursorInfo.Call(uintptr(unsafe.Pointer(&ci))); r == 0 {
		return CursorShape{}
	}
	if ci.Flags&cursorShowing == 0 || ci.HCursor == 0 {
		return CursorShape{}
	}
	art := getArt(ci.HCursor)
	if art == nil {
		return CursorShape{}
	}
	return CursorShape{
		W: art.w, H: art.h,
		HotX: art.hotX, HotY: art.hotY,
		Monochrome: art.monochrome,
		Visible:    true,
	}
}

// DrawCursor 把光标叠加到 dst（紧凑 BGRA，dw×dh）。
// originX/originY 是 dst 左上角对应的虚拟屏幕坐标，用于把光标位置换算到 dst 坐标。
//
// 导出以便诊断工具做「光标落点」断言：把鼠标移到已知坐标，
// 比较绘制前后的差异包围盒是否覆盖该点。
func DrawCursor(dst []byte, dw, dh, originX, originY int) {
	h := CursorHandle()
	if h == 0 {
		return
	}
	var ci cursorInfo
	ci.CbSize = uint32(unsafe.Sizeof(ci))
	if r, _, _ := pGetCursorInfo.Call(uintptr(unsafe.Pointer(&ci))); r == 0 {
		return
	}
	if ci.Flags&cursorShowing == 0 {
		return
	}

	art := getArt(ci.HCursor)
	if art == nil || art.w == 0 || art.h == 0 {
		return
	}

	// 热点对齐到 dst 坐标
	dx := int(ci.PtScreenPos.X) - art.hotX - originX
	dy := int(ci.PtScreenPos.Y) - art.hotY - originY

	for y := 0; y < art.h; y++ {
		ty := dy + y
		if ty < 0 || ty >= dh {
			continue
		}
		for x := 0; x < art.w; x++ {
			tx := dx + x
			if tx < 0 || tx >= dw {
				continue
			}
			si := (y*art.w + x) * 4
			a := art.pix[si+3]
			if a == 0 {
				continue
			}
			di := (ty*dw + tx) * 4
			if a == 255 {
				dst[di] = art.pix[si]
				dst[di+1] = art.pix[si+1]
				dst[di+2] = art.pix[si+2]
				dst[di+3] = 255
				continue
			}
			ia := 255 - int(a)
			dst[di] = byte((int(art.pix[si])*int(a) + int(dst[di])*ia) / 255)
			dst[di+1] = byte((int(art.pix[si+1])*int(a) + int(dst[di+1])*ia) / 255)
			dst[di+2] = byte((int(art.pix[si+2])*int(a) + int(dst[di+2])*ia) / 255)
			dst[di+3] = byte((int(a)*255 + int(dst[di+3])*ia) / 255)
		}
	}
}

// getArt 解码光标位图并缓存（同一句柄只解码一次）。
func getArt(h windows.Handle) *cursorArt {
	cursorMu.Lock()
	defer cursorMu.Unlock()
	if a, ok := cursorCache[h]; ok {
		return a
	}
	a := decodeCursor(h)
	if a != nil {
		cursorCache[h] = a
	}
	return a
}

// PurgeCursorCache 清空缓存。光标主题变更时应调用。
func PurgeCursorCache() {
	cursorMu.Lock()
	cursorCache = map[windows.Handle]*cursorArt{}
	cursorMu.Unlock()
}

func dc() windows.Handle {
	dcOnce.Do(func() {
		h, _, _ := pCreateCompatDC.Call(0)
		sharedDC = windows.Handle(h)
	})
	return sharedDC
}

// decodeCursor 从 HCURSOR 解码出带 alpha 的 BGRA 位图。
func decodeCursor(h windows.Handle) *cursorArt {
	var ii iconInfo
	if r, _, _ := pGetIconInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&ii))); r == 0 {
		return nil
	}
	defer func() {
		if ii.HbmMask != 0 {
			pDeleteObject.Call(uintptr(ii.HbmMask))
		}
		if ii.HbmColor != 0 {
			pDeleteObject.Call(uintptr(ii.HbmColor))
		}
	}()

	// 取 mask 尺寸（单色光标时 mask 高为图标高的 2 倍：AND 上半、XOR 下半）
	var bm gdiBitmap
	if r, _, _ := pGetObjectW.Call(uintptr(ii.HbmMask), uintptr(unsafe.Sizeof(bm)), uintptr(unsafe.Pointer(&bm))); r == 0 {
		return nil
	}
	mw, mh := int(bm.Width), int(bm.Height)
	if mw <= 0 || mh <= 0 {
		return nil
	}

	hdc := dc()
	if hdc == 0 {
		return nil
	}

	maskStride := ((mw + 31) / 32) * 4
	maskBits := make([]byte, maskStride*mh)
	if !getDIBits(hdc, ii.HbmMask, mw, mh, maskBits, 1) {
		return nil
	}

	var art *cursorArt
	if ii.HbmColor != 0 {
		art = decodeColorCursor(hdc, ii, mw, mh, maskBits, maskStride)
	} else {
		art = decodeMonoCursor(mw, mh, maskBits, maskStride)
	}
	if art == nil {
		return nil
	}
	art.hotX = int(ii.XHotspot)
	art.hotY = int(ii.YHotspot)
	return art
}

// getDIBits 以 bottom-up 方式取位；返回后调用方需注意行序。
func getDIBits(hdc, hbmp windows.Handle, w, h int, buf []byte, bitCount uint16) bool {
	var bi bitmapInfoHeader
	bi.BiSize = uint32(unsafe.Sizeof(bi))
	bi.BiWidth = int32(w)
	bi.BiHeight = int32(h) // 正 = bottom-up
	bi.BiPlanes = 1
	bi.BiBitCount = bitCount
	bi.BiCompression = 0 // BI_RGB
	r, _, _ := pGetDIBits.Call(
		uintptr(hdc), uintptr(hbmp),
		0, uintptr(h),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&bi)),
		dibRGBColors,
	)
	return r != 0
}

func decodeColorCursor(hdc windows.Handle, ii iconInfo, mw, mh int, maskBits []byte, maskStride int) *cursorArt {
	var bm gdiBitmap
	if r, _, _ := pGetObjectW.Call(uintptr(ii.HbmColor), uintptr(unsafe.Sizeof(bm)), uintptr(unsafe.Pointer(&bm))); r == 0 {
		return nil
	}
	w, h := int(bm.Width), int(bm.Height)
	if w <= 0 || h <= 0 {
		return nil
	}
	bits := make([]byte, w*h*4)
	if !getDIBits(hdc, ii.HbmColor, w, h, bits, 32) {
		return nil
	}
	art := &cursorArt{w: w, h: h, pix: make([]byte, w*h*4)}
	// DIB 是 bottom-up：第 srcY 行对应输出的第 h-1-srcY 行
	for y := 0; y < h; y++ {
		sy := h - 1 - y
		srow := sy * w * 4
		drow := y * w * 4
		for x := 0; x < w; x++ {
			si := srow + x*4
			di := drow + x*4
			// mask 位为 1 → 该像素透明
			mbit := maskBit(maskBits, maskStride, x, sy)
			if mbit {
				art.pix[di+3] = 0
				continue
			}
			b, g, r_, a := bits[si], bits[si+1], bits[si+2], bits[si+3]
			art.pix[di] = b
			art.pix[di+1] = g
			art.pix[di+2] = r_
			if a == 0 {
				a = 255 // 32bpp 图标常见 alpha 未填充，视为不透明
			}
			art.pix[di+3] = a
		}
	}
	return art
}

func decodeMonoCursor(mw, mh int, maskBits []byte, maskStride int) *cursorArt {
	// 单色光标：mask 上半 = AND，下半 = XOR，高度各为 mh/2
	h := mh / 2
	if h <= 0 {
		return nil
	}
	art := &cursorArt{w: mw, h: h, pix: make([]byte, mw*h*4), monochrome: true}
	for y := 0; y < h; y++ {
		sy := h - 1 - y // bottom-up 翻转
		for x := 0; x < mw; x++ {
			and := maskBit(maskBits, maskStride, x, sy)
			xor := maskBit(maskBits, maskStride, x, sy+h)
			di := (y*mw + x) * 4
			if and {
				// AND=1 → 透明（保留屏幕原本内容）
				art.pix[di+3] = 0
				continue
			}
			if xor {
				art.pix[di], art.pix[di+1], art.pix[di+2] = 255, 255, 255
			} // 否则为黑色（已初始化为 0）
			art.pix[di+3] = 255
		}
	}
	return art
}

// maskBit 取 1bpp mask 中 (x,y) 的位；MSB 对应最左像素。
func maskBit(bits []byte, stride, x, y int) bool {
	off := y*stride + x/8
	if off < 0 || off >= len(bits) {
		return false
	}
	return bits[off]&(0x80>>(x%8)) != 0
}

// DPIAwareness 返回当前进程的 DPI 感知级别：
// 0=unaware（GetCursorPos 返回逻辑像素）、1=system aware、2=per-monitor aware。
func DPIAwareness() int {
	var v uint32
	r, _, _ := pGetProcessDpiAwr.Call(uintptr(windows.CurrentProcess()), uintptr(unsafe.Pointer(&v)))
	if r != 0 {
		return -1 // 查询失败（如 shcore 不可用）
	}
	return int(v)
}

// EnsureDPIAware 把进程标记为 per-monitor DPI aware，返回设置后的级别。
//
// 这是 P0-2，不做会出两件事：
//  1. 高 DPI 屏（本机 175%）上 GetCursorPos 返回逻辑像素，而采集帧是物理像素，
//     光标会画到错误位置（差 1.75 倍）—— 且这种错误在「只看自绘结果」时
//     完全自洽，很难发现。
//  2. 窗口在高 DPI 下会被系统拉伸，界面发虚。
//
// 必须在创建任何窗口之前调用；若进程已被标记（或被外部 manifest 设过），
// SetProcessDpiAwareness 会返回 E_ACCESSDENIED，此时沿用现有级别即可。
func EnsureDPIAware() int {
	if v := DPIAwareness(); v == processPerMonitorDPIAware {
		return v
	}
	// HRESULT: S_OK=0 表示成功；E_ACCESSDENIED 表示已设置过（或已被 manifest 固定）
	pSetProcessDpiAwr.Call(uintptr(processPerMonitorDPIAware))
	return DPIAwareness()
}
