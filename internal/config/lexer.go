package config

import "strings"

// tokenKind 是词法记号的种类。
type tokenKind int

const (
	// tokenWord 是一个指令名或一个取值。
	// 加了引号的取值已经去掉引号并解开转义，因此同一个词无论怎么写都是同一个记号。
	tokenWord tokenKind = iota

	// tokenBlockOpen 与 tokenBlockClose 是 `{` 与 `}`。
	//
	// 本代语法还没有块，词法器仍把它们切成独立记号，理由是错误质量：
	// `provider openai {` 若被当成一串普通取值，报出来的会是「provider 只接受 1 个取值，
	// 多出来的是 "{""」这类答非所问的话；切成独立记号后，语法层才能明确说出
	// 「本代语法还不支持块」。
	tokenBlockOpen
	tokenBlockClose
)

// token 是一个词法记号。
//
// line 与 col 都从 1 起算，col 按 rune 计数。位置挂在记号上而不是挂在行上，
// 因为语法层需要指出的往往是一行里的某个具体取值，而不是整行。
type token struct {
	kind tokenKind
	text string
	line int
	col  int
}

// lex 把配置源码切成记号序列。
//
// 它不认识任何指令名：哪些词合法由语法层判定，词法器只管把源码切对。
// 空行与纯注释行不产出任何记号，因此「有没有这一行」在下游不可见，
// 位置信息全部来自记号自身的 line。
func lex(src []byte, file string) ([]token, error) {
	var tokens []token
	for i, text := range splitLines(src) {
		lineNo := i + 1
		runes := []rune(text)
		for col := 0; col < len(runes); {
			switch r := runes[col]; {
			case r == ' ' || r == '\t':
				col++
			case r == '#':
				// 注释吃掉本行剩余内容，包括其中的花括号与引号。
				col = len(runes)
			case r == '{':
				tokens = append(tokens, token{tokenBlockOpen, "{", lineNo, col + 1})
				col++
			case r == '}':
				tokens = append(tokens, token{tokenBlockClose, "}", lineNo, col + 1})
				col++
			case r == '"':
				value, next, err := lexQuoted(runes, col, lineNo, file)
				if err != nil {
					return nil, err
				}
				tokens = append(tokens, token{tokenWord, value, lineNo, col + 1})
				col = next
			default:
				start := col
				for col < len(runes) && !isDelimiter(runes[col]) {
					col++
				}
				tokens = append(tokens, token{tokenWord, string(runes[start:col]), lineNo, start + 1})
			}
		}
	}
	return tokens, nil
}

// isDelimiter 报告一个 rune 能否终止一个未加引号的取值。
//
// 引号也是终止符：`"a"b` 因此被切成两个记号，而不是一个内容含引号的怪词。
// `#` 同样在内，它出现在取值中间时意味着本行剩余部分是注释，取值不可能再延续。
func isDelimiter(r rune) bool {
	switch r {
	case ' ', '\t', '{', '}', '"', '#':
		return true
	}
	return false
}

// lexQuoted 解析一个双引号取值，返回去引号、解转义后的正文与下一个待扫描的列号。
//
// 只认 \" 与 \\ 两个转义，其它反斜杠组合报错而不是原样保留：原样保留会让
// Windows 路径这类输入静默变成另一种含义（`C:\new` 里的 \n 变成换行），
// 而这种错误在配置文件里极难看出来。
//
// 未闭合的引号报错位置指向起始引号，而不是行尾：出错的是这个词，
// 指向行尾会让人以为是这一行少了什么，而不是这个词本身没写完。
func lexQuoted(runes []rune, start, lineNo int, file string) (string, int, error) {
	var b strings.Builder
	for i := start + 1; i < len(runes); i++ {
		switch r := runes[i]; r {
		case '"':
			return b.String(), i + 1, nil
		case '\\':
			if i+1 >= len(runes) {
				return "", 0, errorf(file, lineNo, start+1, "引号里的转义不完整：以反斜杠结尾")
			}
			next := runes[i+1]
			if next != '"' && next != '\\' {
				return "", 0, errorf(file, lineNo, i+1,
					"引号里的转义 \\%c 不认识，只支持 \\\" 与 \\\\", next)
			}
			b.WriteRune(next)
			i++
		default:
			b.WriteRune(r)
		}
	}
	return "", 0, errorf(file, lineNo, start+1, "引号没有闭合")
}

// splitLines 按 LF 切行并去掉行尾的 CR。
//
// 在这里统一 CRLF 与 LF，而不是让每个下游各自 TrimSpace：
// 两份写法报出的行列因此完全相同，Windows 上编辑过的文件不会把列号整体推后一位。
func splitLines(src []byte) []string {
	lines := strings.Split(string(src), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\r")
	}
	return lines
}
