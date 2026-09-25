package stats

import (
	"strings"
	"unicode/utf8"
)

// unknownClient 是 User-Agent 缺失或无法归一化时的客户端名。
//
// 用具名取值而不是空串：空串在聚合结果里不可见（omitempty 会把它连同整条桶一起丢掉），
// 于是「有多少请求没带 User-Agent」这个问题会从统计里消失。
const unknownClient = "unknown"

// maxClientRunes 是归一化后客户端名的字符数上限。
//
// 截断防的是「产品段异常长」的怪值：它不该成为一条长键把输出撑大。
// 按 rune 计数，避免把一个多字节字符切成半截。
const maxClientRunes = 64

// NormalizeClient 把 User-Agent 归一化为一个有界的客户端产品名。
//
// 规则：取第一个 `/`、空格或 `(` 之前的部分，折叠为小写；取不到则归 unknown。
// 这是各家客户端自报名字的通行形态（claude-cli/1.0.3、python-requests/2.31.0、
// curl/8.5.0），因此产品段稳定而版本段漂移——版本进聚合键会让同一个客户端在每次
// 升级后变成一条新键。
//
// 归一化只用于聚合维度，原始 User-Agent 照旧保留在记录里，供 detail 查询与 agent 过滤。
func NormalizeClient(userAgent string) string {
	trimmed := strings.TrimSpace(userAgent)
	if trimmed == "" {
		return unknownClient
	}

	end := len(trimmed)
	for i, r := range trimmed {
		if r == '/' || r == ' ' || r == '(' {
			end = i
			break
		}
	}
	name := strings.ToLower(trimmed[:end])
	if name == "" {
		return unknownClient
	}
	if utf8.RuneCountInString(name) > maxClientRunes {
		name = string([]rune(name)[:maxClientRunes])
	}
	return name
}
