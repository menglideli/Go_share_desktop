// 光标独立图层（P0-3）的线格式。
//
// 为什么把光标从画面里拆出来：增量帧的成本 = "变化的条带面积"，而鼠标移动会
// 让整条横带重编（32px 条带下 2.6~3.6 Mbps @30fps）。拆出来后位置只是一条
// 25 字节的 ctl 消息，60Hz 也只有 ~12 kbps；形状只在变化时发一次。
//
// 兼容策略：观看端用 CtlHello 声明能力，分享端**只对声明了的观众**关闭
// "画进帧"并开始发光标。没声明的旧观众端行为完全不变（画面里仍有光标）。
package rtc

import (
	"encoding/binary"
	"encoding/json"
)

// CtlCursorShape / CtlCursorPos 的定义在 peer.go 的 CtlType 常量里。

// CursorShape 是一个可缓存的鼠标光标位图（BGRA，top-down，带 alpha）。
type CursorShape struct {
	ID   uint32 // 内容哈希：相同形状 id 相同
	W, H int
	HotX int // 热点（像素）
	HotY int
	Pix  []byte // W*H*4
}

// MarshalCursorShape 编码形状消息：16 字节头 + 位图。
//
//	0..3  id    4..5 w    6..7 h    8..9 hotX   10..11 hotY   12..15 len
func MarshalCursorShape(s CursorShape) []byte {
	b := make([]byte, 16+len(s.Pix))
	binary.BigEndian.PutUint32(b[0:4], s.ID)
	binary.BigEndian.PutUint16(b[4:6], uint16(s.W))
	binary.BigEndian.PutUint16(b[6:8], uint16(s.H))
	binary.BigEndian.PutUint16(b[8:10], uint16(int16(s.HotX)))
	binary.BigEndian.PutUint16(b[10:12], uint16(int16(s.HotY)))
	binary.BigEndian.PutUint32(b[12:16], uint32(len(s.Pix)))
	copy(b[16:], s.Pix)
	return b
}

// ParseCursorShape 解析形状消息。
func ParseCursorShape(p []byte) (CursorShape, bool) {
	if len(p) < 16 {
		return CursorShape{}, false
	}
	n := int(binary.BigEndian.Uint32(p[12:16]))
	if n < 0 || 16+n > len(p) || n != int(binary.BigEndian.Uint16(p[4:6]))*int(binary.BigEndian.Uint16(p[6:8]))*4 {
		return CursorShape{}, false
	}
	return CursorShape{
		ID:   binary.BigEndian.Uint32(p[0:4]),
		W:    int(binary.BigEndian.Uint16(p[4:6])),
		H:    int(binary.BigEndian.Uint16(p[6:8])),
		HotX: int(int16(binary.BigEndian.Uint16(p[8:10]))),
		HotY: int(int16(binary.BigEndian.Uint16(p[10:12]))),
		Pix:  p[16 : 16+n],
	}, true
}

// CursorPos 是一次光标采样。
//
// X/Y 是光标热点在**虚拟屏幕**的绝对坐标；RX/RY 是这一帧采集区域的左上角。
// 分成两块发是为了换区域/换屏时不用重新协商：观看端用 X-RX 得到画面内坐标。
type CursorPos struct {
	TS  uint32 // 分享端时钟毫秒（低 32 位，与 Frame.TS 同一时间轴）
	ID  uint32 // 形状 id；Visible=false 时无意义
	X   int32
	Y   int32
	RX  int32
	RY  int32
	Vis bool
}

// MarshalCursorPos 编码位置消息（25 字节）。
func MarshalCursorPos(p CursorPos) []byte {
	b := make([]byte, 25)
	binary.BigEndian.PutUint32(b[0:4], p.TS)
	binary.BigEndian.PutUint32(b[4:8], p.ID)
	if p.Vis {
		b[8] = 1
	}
	binary.BigEndian.PutUint32(b[9:13], uint32(p.X))
	binary.BigEndian.PutUint32(b[13:17], uint32(p.Y))
	binary.BigEndian.PutUint32(b[17:21], uint32(p.RX))
	binary.BigEndian.PutUint32(b[21:25], uint32(p.RY))
	return b
}

// ParseCursorPos 解析位置消息。
func ParseCursorPos(p []byte) (CursorPos, bool) {
	if len(p) < 25 {
		return CursorPos{}, false
	}
	return CursorPos{
		TS:  binary.BigEndian.Uint32(p[0:4]),
		ID:  binary.BigEndian.Uint32(p[4:8]),
		Vis: p[8]&1 != 0,
		X:   int32(binary.BigEndian.Uint32(p[9:13])),
		Y:   int32(binary.BigEndian.Uint32(p[13:17])),
		RX:  int32(binary.BigEndian.Uint32(p[17:21])),
		RY:  int32(binary.BigEndian.Uint32(p[21:25])),
	}, true
}

// HelloInfo 是 CtlHello 的 payload（JSON，向后兼容：未知字段忽略）。
type HelloInfo struct {
	// Cursor 表示观看端能自己合成光标（会处理 CtlCursorShape/Pos）。
	Cursor bool `json:"cursor,omitempty"`
}

// MarshalHello 编码 CtlHello。
func MarshalHello(h HelloInfo) []byte {
	b, _ := json.Marshal(h)
	return b
}

// ParseHello 解析 CtlHello。
func ParseHello(p []byte) (HelloInfo, bool) {
	var h HelloInfo
	if err := json.Unmarshal(p, &h); err != nil {
		return HelloInfo{}, false
	}
	return h, true
}
