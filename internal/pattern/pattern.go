// Package pattern 提供通配模式匹配。
//
// 它是全仓唯一的通配语义实现：模型准入（allow / deny / expose）、选路规则
// （route 的块头）与统计过滤（按客户端、User-Agent、渠道等筛记录）共用同一套
// 匹配规则。三处各写一份时，「什么算命中」会随使用者而漂移，而这类漂移只在
// 某个模式恰好处于两份实现的差异区间时才暴露，排查时极难定位。
package pattern

import "unicode"

// Match 报告 value 是否匹配一条通配模式。
//
// 语义：
//   - 匹配整个 value，两端隐式锚定，不是子串包含。`gpt*` 只命中以 gpt 开头的值，
//     `openai/gpt-4o` 要用 `openai/gpt-*`；
//   - `*` 匹配任意长度的任意字符，**包含 `/`**。带斜杠的值里，斜杠是取值的一部分
//     （模型 id、User-Agent 的产品段都是），不是路径分隔符；
//   - `?` 匹配恰好一个 Unicode 字符；
//   - 其余字符按字面量匹配；
//   - 不区分大小写。
//
// 不区分大小写，而取值本身可能大小写敏感，两者是两件事：取值必须原样使用，
// 而模式按大小写敏感匹配时，一次笔误的后果是一批数据静默消失，规则里看不出异常。
// 放宽的只是匹配，取值本身保持原样。
//
// 不收正则与字符类：这些规则要能被快速审计，正则带来回溯风险与编译错误，
// 而方括号在模型 id 一类取值里并不罕见，把它当语法会与字面量冲突。
func Match(pattern, value string) bool {
	p := FoldRunes(pattern)
	s := FoldRunes(value)

	pi, si := 0, 0
	// star 记录最近一个星号在模式里的位置，mark 记录它当前吞掉到 value 的哪一格。
	// 失配时回退到「星号多吞一个字符」，这是通配匹配的标准回溯，不需要递归
	// 也不会在多个星号下退化成指数时间。
	star, mark := -1, 0
	for si < len(s) {
		switch {
		case pi < len(p) && (p[pi] == '?' || p[pi] == s[si]):
			pi++
			si++
		case pi < len(p) && p[pi] == '*':
			star = pi
			mark = si
			pi++
		case star >= 0:
			pi = star + 1
			mark++
			si = mark
		default:
			return false
		}
	}
	// 模式尾部剩下的星号匹配空串，因此可以整段跳过；剩任何别的字符都算不匹配。
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}

// FoldRunes 把字符串切成 rune 并把每个 rune 折叠成小写。
//
// 先按 rune 切再逐个折叠，而不是先 strings.ToLower 再切：某些字符转小写后 rune 数会变，
// 那样算出来的下标与 `?` 的「一个字符」对不上。返回切片的长度与原字符串的 rune 数相同，
// 因此调用方可以按下标比较或切分，用于不区分大小写的定位。
func FoldRunes(s string) []rune {
	folded := []rune(s)
	for i := range folded {
		folded[i] = unicode.ToLower(folded[i])
	}
	return folded
}
