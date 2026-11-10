// FEC 校验块的线格式（P1-7 A 方案）。
//
// 为什么用独立的 magic "GSF1" 而不是给 GSJ2 加个 flag：旧观众端看到不认识的
// magic 会直接拒绝这条消息（UnmarshalFrame 报错），**零回归**；
// 而如果复用 GSJ2 的 header，旧端会把校验字节当条带解析，出现花屏。
package rtc

import (
	"encoding/binary"
	"errors"
)

var fecMagic = [4]byte{'G', 'S', 'F', '1'}

// FECGroup 是一组数据块的异或校验。
//
// 组成员是帧内下标 [Group*DataCount, Group*DataCount+DataCount) 的分块。
// Lengths 是每块的**原始长度**：校验块按最长块补零，恢复时必须按原长度裁剪，
// 否则全零尾巴会被 GSJ2 解析器当成"长度为 0 的条带"。
type FECGroup struct {
	Seq       uint64
	TS        uint32
	Group     uint16
	DataCount uint16
	Lengths   []uint32
	Parity    []byte
}

const fecHeaderLen = 4 + 8 + 4 + 2 + 2 + 2 + 4 // magic/seq/ts/group/dataCount/len(Lengths)/parityLen

// MarshalFEC 编码一个校验块。
func MarshalFEC(g FECGroup) []byte {
	b := make([]byte, fecHeaderLen+4*len(g.Lengths)+len(g.Parity))
	copy(b[0:4], fecMagic[:])
	binary.BigEndian.PutUint64(b[4:12], g.Seq)
	binary.BigEndian.PutUint32(b[12:16], g.TS)
	binary.BigEndian.PutUint16(b[16:18], g.Group)
	binary.BigEndian.PutUint16(b[18:20], g.DataCount)
	binary.BigEndian.PutUint16(b[20:22], uint16(len(g.Lengths)))
	binary.BigEndian.PutUint32(b[22:26], uint32(len(g.Parity)))
	off := fecHeaderLen
	for _, n := range g.Lengths {
		binary.BigEndian.PutUint32(b[off:off+4], n)
		off += 4
	}
	copy(b[off:], g.Parity)
	return b
}

// ParseFEC 解析校验块。第一个返回值 ok=false 表示"这不是 FEC 消息"（是普通分块）。
func ParseFEC(p []byte) (FECGroup, bool, error) {
	if len(p) < 4 || p[0] != 'G' || p[1] != 'S' || p[2] != 'F' || p[3] != '1' {
		return FECGroup{}, false, nil
	}
	if len(p) < fecHeaderLen {
		return FECGroup{}, true, errors.New("rtc: FEC 消息过短")
	}
	g := FECGroup{
		Seq:       binary.BigEndian.Uint64(p[4:12]),
		TS:        binary.BigEndian.Uint32(p[12:16]),
		Group:     binary.BigEndian.Uint16(p[16:18]),
		DataCount: binary.BigEndian.Uint16(p[18:20]),
	}
	nLen := int(binary.BigEndian.Uint16(p[20:22]))
	parLen := int(binary.BigEndian.Uint32(p[22:26]))
	if nLen > int(g.DataCount) || parLen < 0 || fecHeaderLen+4*nLen+parLen > len(p) {
		return FECGroup{}, true, errors.New("rtc: FEC 长度不一致")
	}
	off := fecHeaderLen
	for i := 0; i < nLen; i++ {
		g.Lengths = append(g.Lengths, binary.BigEndian.Uint32(p[off:off+4]))
		off += 4
	}
	g.Parity = p[off : off+parLen]
	return g, true, nil
}
