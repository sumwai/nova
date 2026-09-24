package config

import (
	"fmt"
	"strconv"
	"strings"
)

// parseModelRoute 解析一个顶层 route 块；返回时 p.pos 指向块后的第一行。
//
// 块头是模式与花括号：`route <模式> {`。不含通配符时它就是那一个对外名，
// 含 * / ? 时匹配一组名字。规则本身不声明模型存在与否——它只回答「这个名字下
// 候选怎么排」，模型是否存在由 provider 块里的 model 决定。
func (p *parser) parseModelRoute(head line) error {
	if len(head.tokens) != 3 || head.tokens[2].kind != tokenBlockOpen {
		return errorf(head.file, head.tokens[0].line, head.tokens[0].col,
			"route 块写成 `route <对外名模式> {`；现在这一行是 %q", describeLine(head))
	}
	patternTok := head.tokens[1]
	if patternTok.kind != tokenWord {
		return errorf(head.file, patternTok.line, patternTok.col,
			"route 的模式不能是花括号")
	}
	pattern, err := p.expand(patternTok, head)
	if err != nil {
		return err
	}
	if err := checkModelPattern(pattern); err != nil {
		return errorf(head.file, patternTok.line, patternTok.col, "%s", err)
	}
	p.pos++

	route := ModelRoute{
		Pattern: pattern,
		File:    head.file,
		Line:    head.no,
		Col:     patternTok.col,
	}

	// 块体内的重复检测与外层分开：两个 route 块各写一次 balance 是正常的用法。
	outerSeen := p.seen
	p.seen = map[string]token{}
	defer func() { p.seen = outerSeen }()

	for {
		if p.pos >= len(p.lines) {
			return errorf(head.file, head.tokens[0].line, head.tokens[0].col,
				"route %s 块没有收尾的 }", pattern)
		}
		ln := p.lines[p.pos]
		if isBlockClose(ln) {
			p.pos++
			break
		}
		if _, ok := blockOpener(ln); ok {
			// 这里最可能写错的是把候选写成了 `provider x {`：provider 在这个作用域里
			// 是引用已有渠道，不会开块，因此按「带花括号的那一行」报出更准确的提示。
			return errorf(ln.file, ln.tokens[0].line, ln.tokens[0].col,
				"route 块里不能再开块：其中的 provider 是引用已有渠道，写成 `provider <名> [<权重>]` 即可")
		}
		if err := p.applyRouteLine(ln, &route); err != nil {
			return err
		}
		p.pos++
	}

	if len(route.Candidates) == 0 {
		return errorf(route.File, route.Line, route.Col,
			"route %s 没有列出任何候选渠道：规则没有作用对象，删掉整块即可回到按声明顺序回退",
			route.Pattern)
	}
	return p.addModelRoute(route)
}

// applyRouteLine 处理 route 块体里的一条指令。
func (p *parser) applyRouteLine(ln line, route *ModelRoute) error {
	head := ln.tokens[0]
	if head.kind != tokenWord {
		return errorf(ln.file, head.line, head.col,
			"route 块里只能写指令；这一行以 %q 开头", head.text)
	}
	if !contains(routeDirectives, head.text) {
		return p.unknownDirective(head, routeDirectives)
	}

	switch head.text {
	case directiveBalance:
		if err := p.rejectRepeat(head); err != nil {
			return err
		}
		if len(ln.tokens) > 1 {
			extra := ln.tokens[1]
			return errorf(ln.file, extra.line, extra.col,
				"%s 不接受取值：它把列出的候选从按顺序改为按权重轮询分摊", directiveBalance)
		}
		route.Balanced = true
		return nil

	case directiveProvider:
		return p.appendRouteCandidates(ln, route, false)

	case directiveFallback:
		return p.appendRouteCandidates(ln, route, true)

	default:
		return p.unknownDirective(head, routeDirectives)
	}
}

// appendRouteCandidates 追加主用候选或回退候选。
//
// 取值形态是「渠道名 [权重] 渠道名 [权重] …」：一行可以只写一个渠道名，
// 也可以一次列出多个；权重写在它所属渠道名之后，省略即 1。这与 model 的双记号、
// api_key 的双记号同一条惯例：第二取值是对前一个取值的补充。
//
// 代价是渠道名不能是纯数字——它会被当成前一个渠道的权重。配置期就报出这一点，
// 而不是让一个叫 "3" 的渠道变成别人身上的权重。
//
// fallback 为真时记下的是回退候选：它在主用候选全部失败之后才轮到，也不参与轮转。
func (p *parser) appendRouteCandidates(ln line, route *ModelRoute, fallback bool) error {
	head := ln.tokens[0]
	if len(ln.tokens) < 2 {
		return errorf(ln.file, head.line, valueColumn(head),
			"%s 缺少渠道名（形如 %s relay；也可以一行列多个：%s a b c）",
			head.text, head.text, head.text)
	}

	for index := 1; index < len(ln.tokens); {
		nameTok := ln.tokens[index]
		if nameTok.kind != tokenWord {
			return errorf(ln.file, nameTok.line, nameTok.col, "渠道名不能是花括号")
		}
		if looksLikeWeight(nameTok.text) {
			return errorf(ln.file, nameTok.line, nameTok.col,
				"%s 的取值 %q 是纯数字，会被当成前一个渠道的权重；渠道名请用带字母的名字",
				head.text, nameTok.text)
		}

		candidate := RouteCandidate{
			Provider: nameTok.text,
			Weight:   1,
			Fallback: fallback,
			File:     ln.file,
			Line:     ln.no,
			Col:      nameTok.col,
		}
		for _, existing := range route.Candidates {
			if existing.Provider != candidate.Provider {
				continue
			}
			return errorf(ln.file, nameTok.line, nameTok.col,
				"渠道 %q 已经在这条规则里列过（%s 第 %d 行）；同一个候选写两次没有可区分的含义",
				candidate.Provider, existing.File, existing.Line)
		}
		index++

		// 渠道名之后紧跟的数字是它的权重。
		if index < len(ln.tokens) && looksLikeWeight(ln.tokens[index].text) {
			weightTok := ln.tokens[index]
			weight, err := strconv.Atoi(weightTok.text)
			if err != nil || weight < 1 {
				return errorf(ln.file, weightTok.line, weightTok.col,
					"分摊权重 %q 不是正整数（形如 %s relay 3）", weightTok.text, head.text)
			}
			candidate.Weight = weight
			index++
		}
		route.Candidates = append(route.Candidates, candidate)
	}
	return nil
}

// looksLikeWeight 报告一个取值是不是纯十进制数字。
//
// 只认 0-9：带符号或小数点的写法一律当渠道名处理，因为它们在模型名与渠道名里
// 都可能是合法字符，把它们当成权重反而会吞揉一个本该报「渠道不存在」的名字。
func looksLikeWeight(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// checkModelPattern 校验一条对外名模式。
//
// 只拒空模式与含空白的模式：对外名里不会有空串也不会有空白，这两种模式匹配不到
// 任何东西，写下来只会在「为什么这条规则没生效」上耗掉一轮排查。
func checkModelPattern(pattern string) error {
	switch {
	case pattern == "":
		return fmt.Errorf("route 的模式不能为空")
	case strings.ContainsAny(pattern, " \t"):
		return fmt.Errorf("route 的模式 %q 不能含空白", pattern)
	}
	return nil
}

// addModelRoute 把一条规则收进配置，并拒绝完全相同的模式。
//
// 相同的模式不会被第二条执行（第一条命中即生效），因此它必然是一份写着但不起作用的
// 配置；报错让人删掉，比让一条规则静默失效更省事。
func (p *parser) addModelRoute(route ModelRoute) error {
	for _, existing := range p.cfg.ModelRoutes {
		if existing.Pattern != route.Pattern {
			continue
		}
		return errorf(route.File, route.Line, route.Col,
			"route %s 已在 %s 第 %d 行声明过；第一条命中的规则生效，第二条永远不会被用到",
			route.Pattern, existing.File, existing.Line)
	}
	p.cfg.ModelRoutes = append(p.cfg.ModelRoutes, route)
	return nil
}

// validateModelRoutes 校验规则引用的渠道确实存在。
//
// 它排在逐行解析之后：route 块可以写在 provider 块之前，因此只有整份配置解析完
// 才能回答「这个渠道名有没有对应的声明」。
func (p *parser) validateModelRoutes() error {
	for i := range p.cfg.ModelRoutes {
		route := &p.cfg.ModelRoutes[i]
		for _, candidate := range route.Candidates {
			if providerNamed(p.cfg.Providers, candidate.Provider) {
				continue
			}
			return errorf(candidate.File, candidate.Line, candidate.Col,
				"route %s 引用的渠道 %q 没有对应的 provider 块", route.Pattern, candidate.Provider)
		}
	}
	return nil
}

// providerNamed 报告是否存在这个名字的渠道。
func providerNamed(providers []Provider, name string) bool {
	for i := range providers {
		if providers[i].Name == name {
			return true
		}
	}
	return false
}
