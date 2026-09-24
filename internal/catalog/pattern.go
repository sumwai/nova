package catalog

import "unicode"

// Match 报告模型 id 是否匹配一条过滤模式。
//
// 语义：
//   - 匹配整个 id，两端隐式锚定，不是子串包含。`gpt*` 只命中以 gpt 开头的 id，
//     `openai/gpt-4o` 要用 `openai/gpt-*`；
//   - `*` 匹配任意长度的任意字符，**包含 `/`**。模型 id 里的斜杠是名字的一部分
//     （openai/gpt-4o、accounts/fireworks/models/llama-v3p1-70b-instruct），不是路径分隔符；
//   - `?` 匹配恰好一个 Unicode 字符；
//   - 其余字符按字面量匹配；
//   - 不区分大小写。
//
// 不区分大小写与「模型 id 本身大小写敏感」是两件事：id 必须原样发给上游，
// 而过滤模式按大小写敏感匹配时，一次笔误的后果是一个模型静默消失，配置里看不出异常。
// 放宽的只是准入，暴露出去的 id 与发往上游的 id 都保持原样。
//
// 不收正则与字符类：过滤规则是每次启动都要被审计的东西，正则带来回溯风险与编译错误，
// 而方括号在模型 id 里并不罕见，把它当语法会与字面量冲突。
func Match(pattern, id string) bool {
	p := foldRunes(pattern)
	s := foldRunes(id)

	pi, si := 0, 0
	// star 记录最近一个星号在模式里的位置，mark 记录它当前吞掉到 id 的哪一格。
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

// foldRunes 把字符串切成 rune 并把每个 rune 折叠成小写。
//
// 先按 rune 切再逐个折叠，而不是先 strings.ToLower 再切：某些字符转小写后 rune 数会变，
// 那样算出来的下标与 `?` 的「一个字符」对不上。
func foldRunes(s string) []rune {
	folded := []rune(s)
	for i := range folded {
		folded[i] = unicode.ToLower(folded[i])
	}
	return folded
}
