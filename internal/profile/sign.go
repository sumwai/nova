package profile

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
)

// PublicKey 是一条具名 ed25519 公钥。
//
// 公钥只在本包内定义；从配置解析出来的公钥由调用方转成这个形状传入，
// 因此本包不依赖 internal/config，也不知道 Novafile 里 source 指令的语法。
// Name 用于回报「这次签名由哪条公钥命中」，只在同一个源内唯一即可。
type PublicKey struct {
	Name string
	Key  ed25519.PublicKey
}

// VerifyIndex 校验索引原始字节上的分离签名，返回命中的公钥名。
//
// 签名对象是 index.yaml 的原始字节，**不做任何规范化**：不重新序列化、不裁剪空白、
// 不统一换行。重新排版或改一个换行都会让签名失效，这正是「签的是字节」应有之义；
// 若先解析再序列化，签名覆盖的就不再是发送方实际发布的内容。
//
// sig 允许两种形态：base64 文本（标准与 URL 两种字母表都接受，容忍末尾换行），
// 或恰好 ed25519.SignatureSize 字节的原始签名——长度先判，64 字节按原始签名处理。
// 逐个公钥做 ed25519.Verify，命中即返回其 Name；全部不命中返回固定文案，
// 不区分「哪条公钥差一点对上」，避免把公钥集合试探成签名预言机。
func VerifyIndex(raw, sig []byte, keys []PublicKey) (string, *Error) {
	if len(keys) == 0 {
		return "", &Error{Msg: "该源未登记公钥"}
	}
	signature, sigErr := decodeSignature(sig)
	if sigErr != nil {
		return "", sigErr
	}
	for _, key := range keys {
		// 长度不对的公钥直接跳过：ed25519.Verify 对非标准长度会 panic，
		// 而公钥集合由调用方给出，本包不替它崩溃。
		if len(key.Key) != ed25519.PublicKeySize {
			continue
		}
		if ed25519.Verify(key.Key, raw, signature) {
			return key.Name, nil
		}
	}
	return "", &Error{Msg: "索引签名与已登记公钥均不匹配"}
}

// decodeSignature 把签名体解成 ed25519 签名。
//
// 先判原始 64 字节：这个长度与任何 base64 文本都不可能混淆（88 字符才编码 64 字节），
// 因此不必猜。否则按标准与 URL 两种字母表依次尝试，并接受去掉填充的变体；
// 都解不开按「base64 解码失败」报，解码成功但长度不对则单独报长度。
func decodeSignature(sig []byte) ([]byte, *Error) {
	if len(sig) == ed25519.SignatureSize {
		return sig, nil
	}
	text := string(bytes.TrimSpace(sig))

	var (
		decoded []byte
		lastErr error
	)
	for _, encoding := range []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	} {
		value, err := encoding.DecodeString(text)
		if err == nil {
			decoded = value
			break
		}
		lastErr = err
	}
	if decoded == nil {
		return nil, &Error{Msg: fmt.Sprintf("签名 base64 解码失败：%v", lastErr)}
	}
	if len(decoded) != ed25519.SignatureSize {
		return nil, &Error{Msg: fmt.Sprintf("签名解码后是 %d 字节，ed25519 签名必须是 %d 字节",
			len(decoded), ed25519.SignatureSize)}
	}
	return decoded, nil
}
