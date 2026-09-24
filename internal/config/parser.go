package config

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"unicode/utf8"
)

// 顶层指令名。
const (
	directiveVersion   = "version"
	directiveLogLevel  = "log_level"
	directiveLogFormat = "log_format"
	directiveListen    = "listen"
	directiveAdmin     = "admin"
	directiveClientKey = "client_key"
	directiveProvider  = "provider"
	directiveImport    = "import"
)

// provider 与 endpoint 两级的指令名。
const (
	directiveAPIKey   = "api_key"
	directiveURL      = "url"
	directiveProtocol = "protocol"
	directiveTimeout  = "timeout"
	directiveModel    = "model"
	directiveEndpoint = "endpoint"
	directiveDiscover = "discover"
	directiveAllow    = "allow"
	directiveDeny     = "deny"
	directiveExpose   = "expose"
)

// 下面四张表分别是三层作用域的合法指令名，外加一份「能开启块的名字」。
//
// 它们是三处的唯一数据源：语法层的合法性判定、「未知指令」建议的候选集合、
// 以及 InstructionNames 对外报出的清单。三者若各有一份副本，迟早会互相矛盾，
// 而对外的清单一旦说谎，排障时被它带偏的人根本无从察觉。
var (
	globalDirectives = []string{
		directiveVersion, directiveLogLevel, directiveLogFormat, directiveListen,
		directiveAdmin, directiveClientKey, directiveProvider, directiveImport,
	}

	providerDirectives = []string{
		directiveAPIKey, directiveURL, directiveProtocol, directiveTimeout,
		directiveModel, directiveEndpoint, directiveDiscover, directiveAllow, directiveDeny,
		directiveExpose,
	}

	// endpointDirectives 是 endpoint 子块的合法指令名。
	//
	// url 在这一表里，但块内写它是错的——地址由块头给出。把它留在这里是为了让报错说清
	// 这件事（「地址已经由块头给出」），而不是把它归为未知指令，让人去找一个并不存在的
	// 拼写错误。真正拦它的是 applyEndpointLine。
	endpointDirectives = []string{
		directiveURL, directiveProtocol, directiveTimeout, directiveModel,
		directiveDiscover, directiveAllow, directiveDeny, directiveExpose,
	}

	// blockDirectives 是能开启一个块的指令名，只为对外声明能力清单而存在。
	blockDirectives = []string{directiveProvider, directiveEndpoint}
)

// line 是一行里的全部记号。
//
// 词法器不产出空行与纯注释行，因此每个 line 至少有一个记号，
// 且 tokens[0] 一定是这一行的指令名或块开启符。
type line struct {
	file   string
	no     int
	tokens []token
}

// groupLines 把线性记号序列按「文件 + 行号」切成行。
//
// 按文件也要分，是因为 import 会把多个文件的行拼在一起：只按行号分组会让主配置的
// 第 3 行与某个被导入文件的第 3 行合并成同一条指令，于是两行都读不成原意。
func groupLines(tokens []token) []line {
	var lines []line
	for _, t := range tokens {
		if n := len(lines); n > 0 && lines[n-1].no == t.line && lines[n-1].file == t.file {
			lines[n-1].tokens = append(lines[n-1].tokens, t)
			continue
		}
		lines = append(lines, line{file: t.file, no: t.line, tokens: []token{t}})
	}
	return lines
}

// parser 把行集落到 Config 上。
type parser struct {
	lines []line

	// pos 是当前待处理的行下标。
	pos int

	cfg *Config

	// getenv 用于展开 {env.NAME} 占位符。
	getenv func(string) string

	// seen 记下当前作用域里每条「只能写一次」的指令第一次出现的位置，
	// 用于报「不能写两次」时指出前一次在哪。进入 provider 块时换一张新表，
	// 因此不同渠道里的同一指令互不干扰。
	seen map[string]token
}

// run 先判定配置语法代数，再逐行解析。
func (p *parser) run() error {
	if err := p.checkSchema(); err != nil {
		return err
	}
	for p.pos < len(p.lines) {
		ln := p.lines[p.pos]
		head := ln.tokens[0]
		if head.kind != tokenWord {
			return errorf(ln.file, head.line, head.col,
				"这一行以 %q 开头；顶层只能是一条指令名", head.text)
		}
		switch head.text {
		case directiveVersion:
			// 已在 checkSchema 里消费。这里跳过，是为了让「取值个数」这类断言
			// 只在一处发出，同一行不会因为两条路径各查一遍而报两次。
			p.pos++
		case directiveProvider:
			if err := p.parseProvider(ln); err != nil {
				return err
			}
		case directiveImport:
			return errorf(ln.file, head.line, head.col,
				"import 未被展开（这是 nova 的内部错误，请上报）")
		default:
			if err := p.applyGlobal(ln); err != nil {
				return err
			}
			p.pos++
		}
	}
	return p.validate()
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
			return errorf(ln.file, head.line, head.col, "%s 只能声明一次", directiveVersion)
		}
		declared = true

		if len(ln.tokens) == 1 {
			return errorf(ln.file, head.line, valueColumn(head),
				"%s 缺少代数取值（形如 %s 1）", directiveVersion, directiveVersion)
		}
		value := ln.tokens[1]
		if len(ln.tokens) > 2 {
			extra := ln.tokens[2]
			return errorf(ln.file, extra.line, extra.col,
				"%s 只接受一个代数取值，多出来的是 %q", directiveVersion, extra.text)
		}
		got, err := strconv.Atoi(value.text)
		if err != nil {
			return errorf(ln.file, value.line, value.col,
				"配置语法代数 %q 不是整数（形如 %s 1）", value.text, directiveVersion)
		}
		if got < MinSchema || got > CurrentSchema {
			return errorf(ln.file, value.line, value.col,
				"配置语法代数 %d 不被本二进制支持（本二进制支持 %s）%s",
				got, schemaRange(), schemaAdvice(got))
		}
		p.cfg.Schema = got
		p.cfg.SchemaDeclared = true
	}

	if !declared {
		p.cfg.Warnings = append(p.cfg.Warnings, Warning{
			File: p.cfg.Path,
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

// applyGlobal 把一条顶层指令落到 Config 上。
func (p *parser) applyGlobal(ln line) error {
	head := ln.tokens[0]
	if !contains(globalDirectives, head.text) {
		return p.unknownDirective(head, globalDirectives)
	}
	// client_key 可以写多条（一份配置里给不同调用方各发一个 key 是正常用法），
	// 其余顶层指令在同一份配置里只能出现一次。
	if head.text != directiveClientKey {
		if err := p.rejectRepeat(head); err != nil {
			return err
		}
	}

	switch head.text {
	case directiveLogLevel:
		return p.setValue(ln, func(v token) error {
			switch v.text {
			case "debug", "info", "warn", "error":
				p.cfg.LogLevel = v.text
				return nil
			}
			return errorf(ln.file, v.line, v.col,
				"日志级别 %q 不认识，取值只能是 debug / info / warn / error", v.text)
		})

	case directiveLogFormat:
		return p.setValue(ln, func(v token) error {
			switch v.text {
			case "text", "json":
				p.cfg.LogFormat = v.text
				return nil
			}
			return errorf(ln.file, v.line, v.col,
				"日志格式 %q 不认识，取值只能是 text / json", v.text)
		})

	case directiveListen:
		return p.setValue(ln, func(v token) error {
			return p.setAddress(ln, v, &p.cfg.Listen)
		})

	case directiveAdmin:
		return p.setValue(ln, func(v token) error {
			if err := p.setAddress(ln, v, &p.cfg.Admin); err != nil {
				return err
			}
			p.warnUnsafeAdmin(ln)
			return nil
		})

	case directiveClientKey:
		return p.setValue(ln, func(v token) error {
			value, err := p.expand(v, ln)
			if err != nil {
				return err
			}
			if value == "" || strings.ContainsAny(value, " \t") {
				return errorf(ln.file, v.line, v.col,
					"client_key 的取值不能为空、也不能含空白")
			}
			p.cfg.ClientKeys = append(p.cfg.ClientKeys, value)
			return nil
		})

	default:
		// 走到这里说明 globalDirectives 与上面的 switch 不同步。
		// 报成未知指令而不是静默忽略：静默忽略会让配置里的一行彻底失去效果，
		// 而使用者以为它生效了。
		return p.unknownDirective(head, globalDirectives)
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
		File: ln.file,
		Line: ln.no,
		Msg: fmt.Sprintf("管理端点 %s 不是回环地址；它没有鉴权，能连上它的人就能改写这份运行中的配置",
			p.cfg.Admin),
	})
}

// validate 做整体语义校验，攒下那些不阻止加载的提醒。
//
// 它排在逐行解析之后：这些结论要看完整份配置才知道，而在解析中途报出来
// 会打断使用者「先把语法错误改完」的节奏。
func (p *parser) validate() error {
	if len(p.cfg.Providers) == 0 {
		p.cfg.Warnings = append(p.cfg.Warnings, Warning{
			File: p.cfg.Path,
			Msg:  "没有声明任何 provider：网关能起来，但没有任何上游可以转发",
		})
	}
	if len(p.cfg.ClientKeys) == 0 && !isLoopbackAddress(p.cfg.Listen) {
		p.cfg.Warnings = append(p.cfg.Warnings, Warning{
			File: p.cfg.Path,
			Msg: fmt.Sprintf(
				"listen %s 不是回环地址，且没有声明 client_key：任何能连上这个地址的人都能用这份配置里的上游凭据",
				p.cfg.Listen),
		})
	}
	return nil
}

// setValue 断言这一行是「指令 取值」形态，并把取值交给 setter。
//
// 缺取值与多取值在这里统一报出，各条指令因此不必重复这两条约束，
// 报错文案也自然保持一致。
func (p *parser) setValue(ln line, set func(token) error) error {
	head := ln.tokens[0]
	switch {
	case len(ln.tokens) == 1:
		return errorf(ln.file, head.line, valueColumn(head), "%s 缺少取值", head.text)
	case len(ln.tokens) > 2:
		extra := ln.tokens[2]
		return errorf(ln.file, extra.line, extra.col,
			"%s 只接受一个取值，多出来的是 %q", head.text, extra.text)
	}
	return set(ln.tokens[1])
}

// setAddress 校验并落下一个 host:port 形态的地址。
//
// 只收 host:port、不收 unix socket 路径：本版两个地址都直接交给 http.Server，
// 而它按 tcp 监听。收下一个写不出来的形态，只会在更晚的地方报出更晦涩的错。
func (p *parser) setAddress(ln line, value token, target *string) error {
	got, err := p.expand(value, ln)
	if err != nil {
		return err
	}
	_, port, err := net.SplitHostPort(got)
	if err != nil {
		return errorf(ln.file, value.line, value.col,
			"地址 %q 不是 host:port 形态（如 :8080、localhost:2026）：%v", got, err)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return errorf(ln.file, value.line, value.col,
			"地址 %q 的端口必须是 1 到 65535 之间的十进制数", got)
	}
	*target = got
	return nil
}

// expand 展开一个取值里的占位符。
//
// 本版只支持 {env.NAME}。变量未设置与「已设置但为空」在 Go 里无法区分（getenv 都返回
// 空串），两者一律按「没有可用取值」报错：把空凭据放行只会让上游回一个 401，
// 而那时已经看不出问题出在环境变量没导出上。
func (p *parser) expand(tok token, ln line) (string, error) {
	if tok.kind != tokenPlaceholder {
		return tok.text, nil
	}
	name, err := placeholderName(tok.text)
	if err != nil {
		return "", errorf(tok.file, tok.line, tok.col, "%s", err)
	}
	value := p.getenv(name)
	if value == "" {
		return "", errorf(tok.file, tok.line, tok.col,
			"环境变量 %s 未设置或为空（占位符 %s 无法展开）", name, tok.text)
	}
	return value, nil
}

// placeholderName 取出占位符里的变量名。
//
// 本版只支持 env 命名空间，其余命名空间直接报错而不是当作空值继续：写错命名空间
// （例如 {secret.K}）会被当成「变量未设置」，把人引到检查环境变量上去。
func placeholderName(raw string) (string, error) {
	inner := strings.TrimSuffix(strings.TrimPrefix(raw, "{"), "}")
	if !strings.HasPrefix(inner, "env.") {
		return "", fmt.Errorf("不支持的占位符 %s：本版只支持 {env.NAME}", raw)
	}
	name := strings.TrimPrefix(inner, "env.")
	if name == "" {
		return "", fmt.Errorf("占位符 %s 缺少变量名", raw)
	}
	return name, nil
}

// unknownDirective 报告一个不认识的指令名，并尽量给出最接近的合法指令。
//
// 给近似建议的理由：绝大多数「未知指令」是拼写错误，把候选直接摆出来比让人去翻
// 文档快得多。候选集合按当前作用域给，因此 provider 块里不会建议一条只属于顶层
// 的指令。候选离得太远时省略建议段，不留一句「最接近的是 ""」这样的空话。
func (p *parser) unknownDirective(head token, table []string) error {
	if suggestion := closestDirective(head.text, table); suggestion != "" {
		return errorf(head.file, head.line, head.col,
			"未知指令 %q，最接近的合法指令是 %q", head.text, suggestion)
	}
	return errorf(head.file, head.line, head.col, "未知指令 %q", head.text)
}

// rejectRepeat 拒绝同一条指令在同一作用域里出现两次，并指出前一次在哪。
//
// 「同一件事写两次，哪次生效」没有正确答案，因此不做后者覆盖：明确报错让人删掉
// 一行，比替他挑一个更省事，也更不容易在改配置时留下一条以为生效、实则被覆盖的指令。
func (p *parser) rejectRepeat(head token) error {
	if first, ok := p.seen[head.text]; ok {
		return errorf(head.file, head.line, head.col,
			"%s 已在 %s 第 %d 行声明过，不能写两次", head.text, first.file, first.line)
	}
	p.seen[head.text] = head
	return nil
}

// contains 报告一个名字是否在指令表里。
//
// 用线性扫描而不是 map：指令表只有几条，线性扫描没有「表与 map 不同步」的风险，
// 也就不必为这件事单写一个测试。
func contains(table []string, name string) bool {
	for _, item := range table {
		if item == name {
			return true
		}
	}
	return false
}

// isLoopbackAddress 判断一个 host:port 地址是否只绑回环接口。
//
// 空 host 视为「绑所有接口」而不是回环：`:8080` 在 net.Listen 里等价于
// 0.0.0.0:8080，把它算成回环会让一条本该出现的警告消失。
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

// valueColumn 是一行里取值所在（或本应所在）的列号。
//
// 用于「缺少取值」这类没有取值记号可指的报错：它指向指令名之后一格，
// 与「取值该从哪开始写」的位置一致，人照着这个列号看过去就知道缺了什么。
func valueColumn(head token) int {
	return head.col + utf8.RuneCountInString(head.text) + 1
}
