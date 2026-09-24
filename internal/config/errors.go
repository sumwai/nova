package config

import "fmt"

// Error 是一条带位置的配置错误。
//
// Line 与 Col 都从 1 起算，Col 按 rune 计数：含中文的行也能报出人眼可数的列号，
// 否则「第 20 列」会落在看起来完全正确的位置上。
// Line 为 0 表示这条错误不指向某一行（例如文件读不出来），不是「第 0 行」。
type Error struct {
	File string
	Line int
	Col  int
	Msg  string
}

// Error 按「有多少位置信息就说多少」排版。
//
// 四种前缀形式共用一条规则：位置在前、消息在后，且各级之间用冒号分隔，
// 因此编辑器与终端能直接把它当成「文件:行:列」的跳转目标。
func (e *Error) Error() string {
	switch {
	case e.File == "" && e.Line == 0:
		return e.Msg
	case e.File == "":
		return fmt.Sprintf("%d:%d: %s", e.Line, e.Col, e.Msg)
	case e.Line == 0:
		return fmt.Sprintf("%s: %s", e.File, e.Msg)
	default:
		return fmt.Sprintf("%s:%d:%d: %s", e.File, e.Line, e.Col, e.Msg)
	}
}

// errorf 是全包唯一构造 *Error 的出口。
//
// 收敛成一个函数而不是各处直接写 &Error{}：位置参数的顺序（file, line, col）
// 一旦被某个调用点写反，错误就会指向一个看似合理但完全错误的位置，
// 而这种错误只有在真的出错时才暴露。
func errorf(file string, line, col int, format string, args ...any) *Error {
	return &Error{File: file, Line: line, Col: col, Msg: fmt.Sprintf(format, args...)}
}

// maxSuggestionDistance 是「还值得给建议」的编辑距离上限，按输入长度分档。
//
// 短词的两次编辑足以把它变成另一个毫不相干的指令，那种建议只会误导；
// 长词差两个字符则多半仍是拼写问题。
func maxSuggestionDistance(n int) int {
	if n <= 4 {
		return 1
	}
	return 2
}

// closestDirective 在候选指令表里挑出与输入编辑距离最近的一个。
//
// 候选表按当前作用域传入：顶层、provider 块与 endpoint 块各自合法，
// 因此 provider 块里拼错一条指令时，不会被建议一条只属于顶层的指令。
//
// 距离超过阈值时返回空串，由调用方省略建议段：宁可只说「未知指令」，
// 也不给一个离题万里的候选。
//
// 并列时取字典序更小的名字，使结果与候选表的遍历顺序无关——否则把指令表
// 重新排序会让同一个拼写错误报出不同的建议。
func closestDirective(input string, table []string) string {
	best, bestDistance := "", -1
	for _, name := range table {
		d := editDistance(input, name)
		if bestDistance < 0 || d < bestDistance || (d == bestDistance && name < best) {
			best, bestDistance = name, d
		}
	}
	if bestDistance < 0 || bestDistance > maxSuggestionDistance(len([]rune(input))) {
		return ""
	}
	return best
}

// editDistance 是两个字符串之间的 Levenshtein 编辑距离，按 rune 比较。
//
// 按 rune 而不是按字节：一个汉字占多个字节，按字节算会让「改了半个汉字」
// 报出好几位距离，建议因此被阈值挡掉。
//
// 只保留前一行与当前一行，空间是 O(len(b))；候选表很小，不值得为速度换更复杂的算法。
func editDistance(a, b string) int {
	ar, br := []rune(a), []rune(b)
	prev := make([]int, len(br)+1)
	cur := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		cur[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, min(cur[j-1]+1, prev[j-1]+cost))
		}
		prev, cur = cur, prev
	}
	return prev[len(br)]
}
