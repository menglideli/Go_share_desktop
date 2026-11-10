// XOR 校验（P1-7 的 A 方案）：一组数据块配一个异或校验块。
//
// 为什么先做 XOR 而不是 Reed-Solomon：不引第三方依赖、实现 20 行、正好覆盖
// "Wi-Fi 上一条消息（≈12 个 MTU 分片）丢失"这个最常见的单块丢失；一组丢 2 块
// 以上才需要 RS，等有无线实测数据再换。
//
// 校验块的尾部会补零（各数据块长度不同），恢复时按调用方给的原长度裁剪 ——
// 不能把补零丢给 GSJ2 解析器：全零刚好是一个"长度为 0 的条带记录"，
// 会被误当成第 0 号条带。
package codec

// XORParity 计算一组数据块的异或校验，返回校验块与所用长度（最长块）。
func XORParity(chunks [][]byte) ([]byte, int) {
	maxLen := 0
	for _, c := range chunks {
		if len(c) > maxLen {
			maxLen = len(c)
		}
	}
	par := make([]byte, maxLen)
	for _, c := range chunks {
		for i, b := range c {
			par[i] ^= b
		}
	}
	return par, maxLen
}

// XORRecover 用校验块与其余数据块恢复缺失块，按 wantLen 裁剪后返回。
// 调用方必须确保"除缺失块外的同组数据齐全"，否则恢复结果无意义。
func XORRecover(others [][]byte, parity []byte, wantLen int) ([]byte, bool) {
	if wantLen < 0 || wantLen > len(parity) {
		return nil, false
	}
	out := make([]byte, len(parity))
	copy(out, parity)
	for _, c := range others {
		for i, b := range c {
			out[i] ^= b
		}
	}
	return out[:wantLen], true
}
