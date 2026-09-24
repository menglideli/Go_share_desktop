package signal

import (
	"crypto/rand"
	"crypto/subtle"
	"math/big"
)

// 授权码字母表：故意剔除 0/1 —— 手抄时最容易和 O/l 混。
// 6 位 → 8^6 ≈ 26 万种，配合失败限次足够拦住局域网内的误连与扫端口。
const (
	codeDigits = "23456789"
	codeLen    = 6
)

// NewCode 生成一个新授权码。
func NewCode() string {
	b := make([]byte, codeLen)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(codeDigits))))
		if err != nil {
			// crypto/rand 失败意味着系统熵源不可用，此时程序已无法正常安全工作，
			// 不该悄悄退化成可预测的码。用一个固定位兜底但从不相等也不现实，
			// 所以直接返回空串，由调用方判定为失败。
			return ""
		}
		b[i] = codeDigits[n.Int64()]
	}
	return string(b)
}

// CodeEqual 常量时间比较，避免通过响应时间侧信道逐位猜码。
func CodeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
