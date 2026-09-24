package config

import (
	"fmt"
	"net"
	"strconv"
	"unicode/utf8"
)

// 本代语法接受的指令名。
//
// 集中成常量而不是散落的字面量，是为了让「改名一条指令」只需要动一处，
// 且编译器能指出所有未被改到的引用点。
const (
	directiveVersion  = "version"
	directiveLogLevel = "log_level"
	directiveListen   = "listen"
	directiveAdmin    = "admin"
)

// globalDirectives 是顶层接受的全部指令名。
//
// 它是三处的唯一数据源：语法层的合法性判定、InstructionNames 对外报出的清单、
// 以及「未知指令」建议的候选集合。三者若各有一份副本，迟早会互相矛盾，
// 而对外的清单一旦说谎，排障时被它带偏的人根本无从察觉。
var globalDirectives = []string{
	directiveVersion,
	directiveLogLevel,
	directiveListen,
	directiveAdmin,
}

// line 是一行里的全部记号。
//
// 词法器不产出空行与纯注释行，因此每个 line 至少有一个记号，
// 且 tokens[0] 一定是这一行的指令名或块开启符。
type line struct {
	no     int
	tokens []token
}

// groupLines 把线性记号序列按行号切成行。
//
// 它依赖词法器的一个性质：记号按行号非递减产出。同一行号连续出现的记号归为一行，
// 行号一变即开新行。这比在词法器里直接产出行的切片少一层结构，
// 也让「一行」这个概念只在一个地方定义。
func groupLines(tokens []token) []line {
	var lines []line
	for _, t := range tokens {
		if n := len(lines); n > 0 && lines[n-1].no == t.line {
			lines[n-1].tokens = append(lines[n-1].tokens, t)
			continue
		}
		lines = append(lines, line{no: t.line, tokens: []token{t}})
	}
	return lines
}

// parser 逐行把记号落到 Config 上。
type parser struct {
	file  string
	lines []line
	cfg   *Config

	// seen 记下每条已识别指令第一次出现的位置，用于报「不能写两次」时指出前一次在哪。
	// 未知指令不进入这张表：它们根本不构成一次声明，也就无所谓重复。
	seen map[string]token
}

// run 先判定配置语法代数，再逐行解析指令。
func (p *parser) run() error {
	if err := p.checkSchema(); err != nil {
		return err
	}
	for _, ln := range p.lines {
		// version 已在 checkSchema 里消费。这里跳过，是为了让「取值个数」
		// 这类断言只在一处发出，同一行不会因为两条路径各查一遍而报两次。
		if ln.tokens[0].kind == tokenWord && ln.tokens[0].text == directiveVersion {
			continue
		}
		if err := p.applyLine(ln); err != nil {
			return err
		}
	}
	return nil
}

// checkSchema 找出 version 声明并校验它落在支持区间内。
//
// 未声明时按 CurrentSchema 解析并记一条提醒，而不是报错：新增这条指令不该让
// 既有配置集体失效。但提醒仍然要给——「这份配置到底按哪一代在解析」是排障时
// 一定会问的问题，让它由一行输出直接回答，比让人去读源码里的常量便宜得多。
func (p *parser) checkSchema() error {
	declared := false
	for _, ln := range p.lines {
		head := ln.tokens[0]
		if head.kind != tokenWord || head.text != directiveVersion {
			continue
		}
		if declared {
			return errorf(p.file, head.line, head.col, "%s 只能声明一次", directiveVersion)
		}
		declared = true

		if len(ln.tokens) == 1 {
			return errorf(p.file, head.line, valueColumn(head),
				"%s 缺少代数取值（形如 %s 1）", directiveVersion, directiveVersion)
		}
		value := ln.tokens[1]
		if len(ln.tokens) > 2 {
			extra := ln.tokens[2]
			return errorf(p.file, extra.line, extra.col,
				"%s 只接受一个代数取值，多出来的是 %q", directiveVersion, extra.text)
		}
		got, err := strconv.Atoi(value.text)
		if err != nil {
			return errorf(p.file, value.line, value.col,
				"配置语法代数 %q 不是整数（形如 %s 1）", value.text, directiveVersion)
		}
		if got < MinSchema || got > CurrentSchema {
			return errorf(p.file, value.line, value.col,
				"配置语法代数 %d 不被本二进制支持（本二进制支持 %s）%s",
				got, schemaRange(), schemaAdvice(got))
		}
		p.cfg.Schema = got
		p.cfg.SchemaDeclared = true
	}

	if !declared {
		p.cfg.Warnings = append(p.cfg.Warnings, Warning{
			File: p.file,
			Msg: fmt.Sprintf("未声明 %s，按本二进制实现的代数 %d 解析；写一行 %s %d 可让这一点显式",
				directiveVersion, CurrentSchema, directiveVersion, CurrentSchema),
		})
	}
	return nil
}

// schemaRange 把支持区间排成人读的文本。
//
// 区间退化成单点时不写成「1 到 1」：那种写法要读者自己判断它是不是打错了，
// 而这里恰恰是在告诉读者「只有一个选择」。
func schemaRange() string {
	if MinSchema == CurrentSchema {
		return strconv.Itoa(CurrentSchema)
	}
	return fmt.Sprintf("%d 到 %d", MinSchema, CurrentSchema)
}

// schemaAdvice 给出「该怎么办」的那半句，按声明偏向哪一侧分开写。
//
// 两侧的处置完全不同：代数偏高说明 nova 太旧，需要升级程序；代数偏低说明配置
// 太旧，需要改配置。写成同一句「版本不匹配」等于把判断该动哪一边的活推回给使用者。
func schemaAdvice(got int) string {
	if got > CurrentSchema {
		return "；这份配置按更高的代数书写，请升级 nova，或把配置改回本二进制支持的代数"
	}
	return "；本二进制已不再接受这个旧代数，请把配置改到本二进制支持的代数"
}

// applyLine 把一行指令落到 Config 上。
func (p *parser) applyLine(ln line) error {
	head := ln.tokens[0]
	if head.kind != tokenWord {
		return errorf(p.file, head.line, head.col,
			"这一行以 %q 开头；这里只能是一条指令名", head.text)
	}
	if !isKnownDirective(head.text) {
		return p.unknownDirective(head)
	}
	if err := p.rejectRepeat(head); err != nil {
		return err
	}

	switch head.text {
	case directiveLogLevel:
		return p.setValue(ln, func(v token) error {
			switch v.text {
			case "debug", "info", "warn", "error":
				p.cfg.LogLevel = v.text
				return nil
			}
			return errorf(p.file, v.line, v.col,
				"日志级别 %q 不认识，取值只能是 debug / info / warn / error", v.text)
		})
	case directiveListen:
		return p.setValue(ln, func(v token) error {
			return p.setAddress(v, &p.cfg.Listen)
		})
	case directiveAdmin:
		return p.setValue(ln, func(v token) error {
			if err := p.setAddress(v, &p.cfg.Admin); err != nil {
				return err
			}
			p.warnUnsafeAdmin(ln)
			return nil
		})
	default:
		// 走到这里说明 globalDirectives 与上面的 switch 不同步。
		// 报成未知指令而不是静默忽略：静默忽略会让配置里的一行彻底失去效果，
		// 而使用者以为它生效了。
		return p.unknownDirective(head)
	}
}

// warnUnsafeAdmin 在管理端点绑到非回环地址时记一条提醒。
//
// 管理端点没有鉴权，能连上它的人就能改写这份运行中的配置。这里只提醒而不报错：
// 在内网里绑一个固定地址是合理用法，拦下来会让它变成做不到的事。
func (p *parser) warnUnsafeAdmin(ln line) {
	if isLoopbackAddress(p.cfg.Admin) {
		return
	}
	p.cfg.Warnings = append(p.cfg.Warnings, Warning{
		File: p.file,
		Line: ln.no,
		Msg: fmt.Sprintf("管理端点 %s 不是回环地址；它没有鉴权，能连上它的人就能改写这份运行中的配置",
			p.cfg.Admin),
	})
}

// setValue 断言这一行是「指令 取值」形态，并把取值交给 setter。
//
// 缺取值与多取值在这里统一报出，各条指令因此不必重复这两条约束，
// 报错文案也自然保持一致。
func (p *parser) setValue(ln line, set func(token) error) error {
	head := ln.tokens[0]
	switch {
	case len(ln.tokens) == 1:
		return errorf(p.file, head.line, valueColumn(head), "%s 缺少取值", head.text)
	case len(ln.tokens) > 2:
		extra := ln.tokens[2]
		return errorf(p.file, extra.line, extra.col,
			"%s 只接受一个取值，多出来的是 %q", head.text, extra.text)
	}
	return set(ln.tokens[1])
}

// setAddress 校验并落下一个 host:port 形态的地址。
//
// 只收 host:port、不收 unix socket 路径：本版两个地址都直接交给 http.Server，
// 而它按 tcp 监听。收下一个写不出来的形态，只会在更晚的地方报出更晦涩的错。
func (p *parser) setAddress(value token, target *string) error {
	_, port, err := net.SplitHostPort(value.text)
	if err != nil {
		return errorf(p.file, value.line, value.col,
			"地址 %q 不是 host:port 形态（如 :8080、localhost:2026）：%v", value.text, err)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return errorf(p.file, value.line, value.col,
			"地址 %q 的端口必须是 1 到 65535 之间的十进制数", value.text)
	}
	*target = value.text
	return nil
}

// isLoopbackAddress 判断一个 host:port 地址是否只绑回环接口。
//
// 空 host 视为「绑所有接口」而不是回环：`:2026` 在 net.Listen 里等价于
// 0.0.0.0:2026，把它算成回环会让一条本该出现的警告消失。
func isLoopbackAddress(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// unknownDirective 报告一个不认识的指令名，并尽量给出最接近的合法指令。
//
// 给近似建议的理由：绝大多数「未知指令」是拼写错误，把候选直接摆出来比让人去翻
// 文档快得多。候选离得太远时省略建议段，不留一句「最接近的是 ""」这样的空话。
func (p *parser) unknownDirective(head token) error {
	if suggestion := closestDirective(head.text); suggestion != "" {
		return errorf(p.file, head.line, head.col,
			"未知指令 %q，最接近的合法指令是 %q", head.text, suggestion)
	}
	return errorf(p.file, head.line, head.col, "未知指令 %q", head.text)
}

// rejectRepeat 拒绝同一条指令出现两次，并指出前一次在哪一行。
//
// 「同一件事写两次，哪次生效」没有正确答案，因此不做后者覆盖：明确报错让人删掉
// 一行，比替他挑一个更省事，也更不容易在改配置时留下一条以为生效、实则被覆盖的指令。
func (p *parser) rejectRepeat(head token) error {
	if first, ok := p.seen[head.text]; ok {
		return errorf(p.file, head.line, head.col,
			"%s 已在第 %d 行声明过，不能写两次", head.text, first.line)
	}
	p.seen[head.text] = head
	return nil
}

// isKnownDirective 报告一个指令名是否在本代语法里。
//
// 用线性扫描而不是 map：指令表只有几条，线性扫描没有「表与 map 不同步」的风险，
// 也就不必为这件事单写一个测试。
func isKnownDirective(name string) bool {
	for _, d := range globalDirectives {
		if d == name {
			return true
		}
	}
	return false
}

// valueColumn 是一行里取值所在（或本应所在）的列号。
//
// 用于「缺少取值」这类没有取值记号可指的报错：它指向指令名之后一格，
// 与「取值该从哪开始写」的位置一致，人照着这个列号看过去就知道缺了什么。
func valueColumn(head token) int {
	return head.col + utf8.RuneCountInString(head.text) + 1
}
