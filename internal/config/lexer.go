package config

import "strings"

// tokenKind 是词法记号的种类。
type tokenKind int

const (
	// tokenWord 是一个指令名或一个取值。
	// 加了引号的取值已经去掉引号并解开转义，因此同一个词无论怎么写都是同一个记号。
	tokenWord tokenKind = iota

	// tokenPlaceholder 是 {env.NAME} 形式的占位符，text 保留花括号原样。
	//
	// 它单独成一种记号而不是并进 tokenWord，是因为「这是个占位符」与「这是个恰好长得
	// 像占位符的字面量」是两件事：后者被引号包住时是数据，不该被展开成环境变量。
	tokenPlaceholder

	// tokenBlockOpen 与 tokenBlockClose 是 `{` 与 `}`。
	//
	// 它们切成独立记号，是为了让「块开在哪、闭合在哪」由结构回答而不是靠缩进或行序去猜：
	// 缩进对配置文件来说太脆（一次误按 Tab 就换了个含义），而块界要靠报错去发现也太晚。
	tokenBlockOpen
	tokenBlockClose
)

// token 是一个词法记号。
//
// file/line/col 用于报错定位：line 与 col 从 1 起算，col 按 rune 计数，
// 因此含中文的行也能报出人眼可数的列号。
//
// file 挂在记号上而不是由调用方统一给出，是因为 import 会把多个文件的行拼在一起：
// 拼接之后「这一行来自哪个文件」只有记号自己回答得了，否则来自被导入文件的错误
// 会被指到主配置的某个行号上，人照着去找只会看到一段毫不相干的配置。
type token struct {
	kind tokenKind
	text string
	file string
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
				if placeholderFollows(runes, col) {
					value, next, err := lexPlaceholder(runes, col, lineNo, file)
					if err != nil {
						return nil, err
					}
					tokens = append(tokens, token{tokenPlaceholder, value, file, lineNo, col + 1})
					col = next
					break
				}
				tokens = append(tokens, token{tokenBlockOpen, "{", file, lineNo, col + 1})
				col++
			case r == '}':
				tokens = append(tokens, token{tokenBlockClose, "}", file, lineNo, col + 1})
				col++
			case r == '"':
				value, next, err := lexQuoted(runes, col, lineNo, file)
				if err != nil {
					return nil, err
				}
				tokens = append(tokens, token{tokenWord, value, file, lineNo, col + 1})
				col = next
			default:
				start := col
				for col < len(runes) && !isDelimiter(runes[col]) {
					col++
				}
				tokens = append(tokens, token{tokenWord, string(runes[start:col]), file, lineNo, start + 1})
			}
		}
	}
	return tokens, nil
}

// placeholderFollows 报告 `{` 是不是一个占位符的开头。
//
// 判据是 `{` 后面紧跟 `env.`，而不是「后面不是空白」：后者会把紧贴取值的块开启
// （例如 `endpoint chat{`）也当成占位符起点，于是那一行剩下的内容全被吞进这个
// 「占位符」里，报错会指向一个谁也不认识的变量名。
//
// 只认 env. 的代价是命名空间写错时（`{secret.K}`）会被当成块开启，报出「块没有收尾的 }」。
// 那种写法本来就不该出现，而这个代价换来的是块开启永远可靠——后者是常用路径。
func placeholderFollows(runes []rune, start int) bool {
	const namespace = "env."
	begin := start + 1
	end := begin + len(namespace)
	if end > len(runes) {
		return false
	}
	return string(runes[begin:end]) == namespace
}

// lexPlaceholder 从 runes[start] 的 `{` 开始读一个占位符，返回原样文本与下一个待扫描的列号。
//
// 占位符不跨行：`{` 到本行末尾都没等到 `}` 就是错误，而不是继续到下一行去找。
// 跨行会让「少写一个 `}`」的后果扩散到后面几行，报出的位置离真正写错的地方很远。
func lexPlaceholder(runes []rune, start, lineNo int, file string) (string, int, error) {
	for i := start + 1; i < len(runes); i++ {
		if runes[i] == '}' {
			return string(runes[start : i+1]), i + 1, nil
		}
	}
	return "", 0, errorf(file, lineNo, start+1, "占位符缺少收尾的 }")
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
