package catalog

import "strings"

// Alias 是一条改名规则：把匹配 From 的上游 id 以 To 暴露给客户端。
//
// 它与 config 层的同名类型是两份数据：本包不依赖 internal/config，装配层负责把配置
// 拆成这些纯数据。File / Line 只为一件事存在——把「这条规则没命中任何模型」的提醒
// 指回配置文件里的那一行。
type Alias struct {
	// From 是上游 id 的模式，含恰好一个 `*` 作捕获。
	From string

	// To 是对外名模式，含 0 或 1 个 `*`（用捕获值填充）。
	To string

	// File / Line 是规则在配置文件里的位置，仅用于提醒定位。
	File string
	Line int
}

// applyAliases 按声明顺序取第一条命中的规则，返回对外名与命中的规则下标。
//
// 第一条命中即生效而不是「最后一条覆盖」：多条规则并存时（先特例、后通配）能表达分层，
// 而覆盖语义要求书写者先读懂全部规则才知道最终取值。
// 下标为 -1 表示没有任何规则命中，对外名保持上游 id。
func applyAliases(aliases []Alias, id string) (string, int) {
	for i, alias := range aliases {
		if name, ok := alias.expose(id); ok {
			return name, i
		}
	}
	return id, -1
}

// expose 用 From 匹配一个上游 id，命中时返回按 To 生成的对外名。
func (a Alias) expose(id string) (string, bool) {
	before, after, ok := splitCapture(a.From)
	if !ok {
		return "", false
	}
	captured, ok := captureBetween(before, after, id)
	if !ok {
		return "", false
	}
	// To 里至多一个 `*`（配置层已校验），因此替换一次即可。
	return strings.Replace(a.To, "*", captured, 1), true
}

// splitCapture 把「恰好含一个 *」的模式切成捕获前的前缀与后缀。
//
// 模式形状由配置层保证；这里再查一遍是为了「本包被单独使用时也不会静默给出半个结果」——
// 一个含两个 * 的模式没有定义的捕获区间，返回 false 由调用方按未命中处理。
func splitCapture(pattern string) (before, after string, ok bool) {
	star := strings.Index(pattern, "*")
	if star < 0 {
		return "", "", false
	}
	before, after = pattern[:star], pattern[star+1:]
	if strings.Contains(after, "*") {
		return "", "", false
	}
	return before, after, true
}

// captureBetween 报告 id 是否以 before 开头、以 after 结尾，命中时返回中间那段原文。
//
// 比较不区分大小写（与 allow / deny 同一口径），返回的捕获值取自 id 原文：
// 拼出来的是要发给客户端的名字，不该被过滤语法的宽松改写。
//
// 按 rune 比较与切分（foldRunes 逐 rune 折叠，长度不变），因此非 ASCII 的模型名
// 不会在下标上错位。
func captureBetween(before, after, id string) (string, bool) {
	foldedID := foldRunes(id)
	foldedBefore := foldRunes(before)
	foldedAfter := foldRunes(after)
	if len(foldedID) < len(foldedBefore)+len(foldedAfter) {
		return "", false
	}
	for i := range foldedBefore {
		if foldedID[i] != foldedBefore[i] {
			return "", false
		}
	}
	for i := range foldedAfter {
		if foldedID[len(foldedID)-len(foldedAfter)+i] != foldedAfter[i] {
			return "", false
		}
	}
	captured := []rune(id)[len(foldedBefore) : len(foldedID)-len(foldedAfter)]
	return string(captured), true
}
