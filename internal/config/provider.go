package config

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sumwai/nova/internal/domain"
)

// childBlock 是一个尚未解析的块：块头行与块体行。
type childBlock struct {
	head line
	body []line
}

// parseProvider 解析一个 provider 块；返回时 p.pos 指向块后的第一行。
//
// 块体里有两类行：provider 级指令，与 endpoint 子块。前者中除了 api_key 之外的
// 端点指令（url / protocol / timeout / model）构成这条渠道的「默认端点」——
// 单端点时因此不必多写一层花括号，而多端点时每条端点的指令仍然只在它自己那一层，
// 「哪些模型走哪条端点」不会因为平铺而丢失。
func (p *parser) parseProvider(head line) error {
	if len(head.tokens) != 3 || head.tokens[2].kind != tokenBlockOpen {
		return errorf(head.file, head.tokens[0].line, head.tokens[0].col,
			"provider 块写成 `provider <名字> {`；现在这一行是 %q", describeLine(head))
	}
	nameTok := head.tokens[1]
	if nameTok.kind != tokenWord {
		return errorf(head.file, nameTok.line, nameTok.col, "provider 的名字不能是花括号")
	}
	p.pos++

	provider := Provider{Name: nameTok.text, File: nameTok.file, Line: nameTok.line, Col: nameTok.col}
	defaultEndpoint := Endpoint{
		Default: true,
		File:    nameTok.file,
		Line:    nameTok.line,
		Col:     nameTok.col,
	}
	var hasDefault bool
	var children []childBlock

	// 块体内的重复检测与外层分开：两个渠道里各写一次 url 是正常的用法。
	outerSeen := p.seen
	p.seen = map[string]token{}
	defer func() { p.seen = outerSeen }()

	for {
		if p.pos >= len(p.lines) {
			return errorf(head.file, head.tokens[0].line, head.tokens[0].col,
				"provider %s 块没有收尾的 }", provider.Name)
		}
		ln := p.lines[p.pos]
		if isBlockClose(ln) {
			p.pos++
			break
		}
		if opener, ok := blockOpener(ln); ok {
			p.pos++
			body, err := p.collectBlockBody(opener)
			if err != nil {
				return err
			}
			children = append(children, childBlock{head: opener, body: body})
			continue
		}
		if err := p.applyProviderLine(ln, &provider, &defaultEndpoint, &hasDefault); err != nil {
			return err
		}
		p.pos++
	}

	if hasDefault {
		provider.Endpoints = append(provider.Endpoints, defaultEndpoint)
	}
	for _, child := range children {
		endpoint, err := p.parseEndpointBlock(child)
		if err != nil {
			return err
		}
		provider.Endpoints = append(provider.Endpoints, endpoint)
	}

	if err := p.finishProvider(&provider); err != nil {
		return err
	}
	return p.addProvider(provider)
}

// applyProviderLine 处理 provider 块体里的一条 provider 级指令。
func (p *parser) applyProviderLine(
	ln line,
	provider *Provider,
	endpoint *Endpoint,
	hasDefault *bool,
) error {
	head := ln.tokens[0]
	if head.kind != tokenWord {
		return errorf(ln.file, head.line, head.col,
			"provider 块里只能写指令或 endpoint 子块；这一行以 %q 开头", head.text)
	}
	if !contains(providerDirectives, head.text) {
		return p.unknownDirective(head, providerDirectives)
	}
	// model 、allow / deny 与 expose 可以写多次，其余指令在同一个作用域里只能出现一次。
	// api_key 同样可以写多次：一条 api_key 声明一个账号，多账号是同一句指令的重复，
	// 不是另一种写法。
	if head.text != directiveModel && head.text != directiveAllow &&
		head.text != directiveDeny && head.text != directiveExpose &&
		head.text != directiveAPIKey {
		if err := p.rejectRepeat(head); err != nil {
			return err
		}
	}

	switch head.text {
	case directiveAPIKey:
		return p.appendAccount(ln, provider)

	case directiveBalance:
		return p.setBalance(ln, provider)

	case directiveEndpoint:
		// endpoint 子块走 blockOpener 分支；走到这里说明这一行没带 `{`。
		return errorf(ln.file, head.line, head.col,
			"endpoint 块写成 `endpoint <地址> {`；不带花括号的 endpoint 表达不了它提供哪些模型")

	default:
		if !*hasDefault {
			// 默认端点的位置取它第一条端点指令所在的行：候选顺序按声明位置排，
			// 用 provider 块头那一行会让它总是排到所有子块之前。
			endpoint.File = ln.file
			endpoint.Line = ln.no
			endpoint.Col = head.col
		}
		*hasDefault = true
		return p.applyEndpointLine(ln, endpoint)
	}
}

// appendAccount 追加一个账号。
//
// 形态是 `api_key <密钥> [<权重>]`：与 model 的双记号同形，第二取值是对第一取值的补充。
// 只有这一种写法——具名账号块与它表达的是同一件事，两种写法并存会让「用哪个」
// 成为每次书写的额外决定，而两者能表达的东西完全一样。
func (p *parser) appendAccount(ln line, provider *Provider) error {
	head := ln.tokens[0]
	switch {
	case len(ln.tokens) < 2:
		return errorf(ln.file, head.line, valueColumn(head),
			"%s 缺少密钥（形如 %s {env.OPENAI_KEY}；要加权就写 %s {env.OPENAI_KEY} 3）",
			directiveAPIKey, directiveAPIKey, directiveAPIKey)
	case len(ln.tokens) > 3:
		extra := ln.tokens[3]
		return errorf(ln.file, extra.line, extra.col,
			"%s 至多接受两个取值（密钥与分摊权重），多出来的是 %q", directiveAPIKey, extra.text)
	}

	value, err := p.expand(ln.tokens[1], ln)
	if err != nil {
		return err
	}

	account := Account{
		APIKey: value,
		Weight: 1,
		Index:  len(provider.Accounts) + 1,
		File:   ln.file,
		Line:   ln.no,
		Col:    head.col,
	}
	if len(ln.tokens) == 3 {
		weightTok := ln.tokens[2]
		weight, err := strconv.Atoi(weightTok.text)
		if err != nil || weight < 1 {
			return errorf(ln.file, weightTok.line, weightTok.col,
				"分摊权重 %q 不是正整数（形如 %s {env.OPENAI_KEY} 3）",
				weightTok.text, directiveAPIKey)
		}
		account.Weight = weight
	}
	provider.Accounts = append(provider.Accounts, account)
	return nil
}

// setBalance 处理 balance 指令。
//
// 它不接受取值：账号池只有两种调度——不写时的「按声明顺序」与写了时的「按权重分摊」——
// 把两种调度收进一个取值域，会让同一件事有「写 balance」与「写 balance xxx」两种表达。
func (p *parser) setBalance(ln line, provider *Provider) error {
	if len(ln.tokens) > 1 {
		extra := ln.tokens[1]
		return errorf(ln.file, extra.line, extra.col,
			"%s 不接受取值：它把账号池从按声明顺序改为按权重轮询分摊", directiveBalance)
	}
	provider.Balanced = true
	return nil
}

// parseEndpointBlock 解析一个 endpoint 子块。块头就是这条端点的地址。
func (p *parser) parseEndpointBlock(child childBlock) (Endpoint, error) {
	head := child.head
	if len(head.tokens) != 3 {
		return Endpoint{}, errorf(head.file, head.tokens[0].line, head.tokens[0].col,
			"endpoint 块写成 `endpoint <地址> {`；块头既是这条端点的地址，也是它的标识")
	}
	urlTok := head.tokens[1]
	if urlTok.kind != tokenWord {
		return Endpoint{}, errorf(head.file, urlTok.line, urlTok.col,
			"endpoint 的地址不能是花括号")
	}
	value, err := p.expand(urlTok, head)
	if err != nil {
		return Endpoint{}, err
	}

	endpoint := Endpoint{URL: value, File: head.file, Line: head.no, Col: urlTok.col}

	// 子块里的重复检测与 provider 级分开：两个端点各写一次 timeout 是正常的。
	outerSeen := p.seen
	p.seen = map[string]token{}
	defer func() { p.seen = outerSeen }()

	for _, ln := range child.body {
		tk := ln.tokens[0]
		if tk.kind != tokenWord {
			return Endpoint{}, errorf(ln.file, tk.line, tk.col,
				"endpoint 块里只能写指令；这一行以 %q 开头", tk.text)
		}
		if !contains(endpointDirectives, tk.text) {
			return Endpoint{}, p.unknownDirective(tk, endpointDirectives)
		}
		if tk.text != directiveModel && tk.text != directiveAllow &&
			tk.text != directiveDeny && tk.text != directiveExpose {
			if err := p.rejectRepeat(tk); err != nil {
				return Endpoint{}, err
			}
		}
		if err := p.applyEndpointLine(ln, &endpoint); err != nil {
			return Endpoint{}, err
		}
	}
	return endpoint, nil
}

// applyEndpointLine 把一条端点指令落到端点上。
//
// provider 级的默认端点与 endpoint 子块共用它，因此「地址要完整」「协议要与地址一致」
// 「同一个端点里模型名不重复」这些规则只有一份实现，两条路径不会各长出一套。
func (p *parser) applyEndpointLine(ln line, endpoint *Endpoint) error {
	head := ln.tokens[0]
	// 子块的块头已经给过地址，块内再写 url 就是给这条端点两个地址。
	if head.text == directiveURL && endpoint.URL != "" {
		return errorf(ln.file, head.line, head.col,
			"这条端点的地址已经由 endpoint 块头给出，块内不能再写 url")
	}

	switch head.text {
	case directiveURL:
		return p.setValue(ln, func(v token) error {
			value, err := p.expand(v, ln)
			if err != nil {
				return err
			}
			endpoint.URL = value
			return nil
		})

	case directiveProtocol:
		return p.setValue(ln, func(v token) error {
			protocol, err := parseProtocol(v.text)
			if err != nil {
				return errorf(ln.file, v.line, v.col, "%s；取值只能是 %s",
					err, strings.Join(protocolNames(), " / "))
			}
			endpoint.Protocol = protocol
			return nil
		})

	case directiveTimeout:
		return p.setValue(ln, func(v token) error {
			d, err := time.ParseDuration(v.text)
			if err != nil || d <= 0 {
				return errorf(ln.file, v.line, v.col,
					"超时 %q 不是正的时长（形如 30s、2m）", v.text)
			}
			endpoint.Timeout = d
			return nil
		})

	case directiveModel:
		return p.appendModel(ln, endpoint)

	case directiveDiscover:
		return p.appendDiscover(ln, endpoint)

	case directiveAllow:
		return p.appendPattern(ln, endpoint, false)

	case directiveDeny:
		return p.appendPattern(ln, endpoint, true)

	case directiveExpose:
		return p.appendAlias(ln, endpoint)

	default:
		return p.unknownDirective(head, endpointDirectives)
	}
}

// appendModel 追加一条模型映射。
//
// 形态是 `model <对外名> [<上游名>]`，只有这一种写法。块形态与一行写法并存会让
// 同一件事有两种表达，而两者能表达的东西完全一样，多出来的那种只会在每次书写时
// 多出一个「用哪个」的决定。
func (p *parser) appendModel(ln line, endpoint *Endpoint) error {
	head := ln.tokens[0]
	if len(ln.tokens) < 2 {
		return errorf(ln.file, head.line, valueColumn(head),
			"model 缺少对外模型名（形如 model gpt-5；要改写上游名就写 model gpt-5 gpt-5-2025-01-01）")
	}
	if len(ln.tokens) > 3 {
		extra := ln.tokens[3]
		return errorf(ln.file, extra.line, extra.col,
			"model 至多接受两个取值（对外名与上游名），多出来的是 %q", extra.text)
	}

	name, err := p.expand(ln.tokens[1], ln)
	if err != nil {
		return err
	}
	upstream := name
	if len(ln.tokens) == 3 {
		upstream, err = p.expand(ln.tokens[2], ln)
		if err != nil {
			return err
		}
	}

	for _, existing := range endpoint.Models {
		if existing.Name != name {
			continue
		}
		return errorf(ln.file, ln.tokens[1].line, ln.tokens[1].col,
			"这条端点里的对外模型名 %q 已经声明过；同名候选按端点之间的声明顺序回退，"+
				"同一个端点里再写一次没有可区分的含义", name)
	}
	endpoint.Models = append(endpoint.Models, Model{Name: name, Upstream: upstream})
	p.warnWildcardModel(ln, name, upstream)
	return nil
}

// warnWildcardModel 在 model 的名字里出现通配符时记一条提醒。
//
// model 的两个记号都按字面量使用：`model sensenova/* *` 表示「对外名就叫 sensenova/*、
// 发往上游时写 *」，不是模式匹配。想按模式筛上游清单的是 allow / deny，两者容易搞混，
// 而混错的后果是一个永远不会被客户端请求到的对外名，加上一个上游看不懂的上游名。
//
// 只提醒不报错：含 * 或 ? 的名字仍是合法的对外名，拦截它会让「上游真有一个这种名字」
// 这种罕见情形变得无法表达。
func (p *parser) warnWildcardModel(ln line, name, upstream string) {
	offending := ""
	switch {
	case strings.ContainsAny(name, "*?"):
		offending = fmt.Sprintf("对外名 %q", name)
	case strings.ContainsAny(upstream, "*?"):
		offending = fmt.Sprintf("上游名 %q", upstream)
	default:
		return
	}
	p.cfg.Warnings = append(p.cfg.Warnings, Warning{
		File: ln.file,
		Line: ln.no,
		Msg: fmt.Sprintf("%s 含通配符，但 model 的两个记号都按字面量使用、不做通配匹配；"+
			"要按模式筛选上游清单请用 discover 下的 allow / deny", offending),
	})
}

// appendDiscover 解析 discover 指令。
//
// 它接受 0 或 1 个取值：省略地址时清单地址由端点 url 推导（见 deriveListingURL），
// 写了地址则用它。两种形态表达的是同一件事——「这条端点的模型来自上游清单」——
// 差异只在地址从哪来，因此收在同一条指令里而不是拆成两条。
func (p *parser) appendDiscover(ln line, endpoint *Endpoint) error {
	head := ln.tokens[0]
	if len(ln.tokens) > 2 {
		extra := ln.tokens[2]
		return errorf(ln.file, extra.line, extra.col,
			"%s 至多接受一个取值（清单接口地址），多出来的是 %q",
			directiveDiscover, extra.text)
	}

	spec := discoverOf(endpoint, head)
	spec.Declared = true
	spec.File = ln.file
	spec.Line = ln.no
	spec.Col = head.col
	if len(ln.tokens) == 1 {
		return nil
	}

	value, err := p.expand(ln.tokens[1], ln)
	if err != nil {
		return err
	}
	if err := checkHTTPURL(value); err != nil {
		return errorf(ln.file, ln.tokens[1].line, ln.tokens[1].col,
			"清单地址 %q %s", value, err)
	}
	spec.URL = value
	return nil
}

// appendPattern 追加一条过滤模式。
//
// allow 与 deny 共用本函数：两者的语法与校验完全一致，差异只在写进哪一张列表，
// 因此不需要为「本来就能给同一张列表加一项」写两遍。
func (p *parser) appendPattern(ln line, endpoint *Endpoint, deny bool) error {
	spec := discoverOf(endpoint, ln.tokens[0])
	return p.setValue(ln, func(v token) error {
		value, err := p.expand(v, ln)
		if err != nil {
			return err
		}
		if err := checkPattern(value); err != nil {
			return errorf(ln.file, v.line, v.col, "%s", err)
		}
		if deny {
			spec.Deny = append(spec.Deny, value)
		} else {
			spec.Allow = append(spec.Allow, value)
		}
		return nil
	})
}

// appendAlias 解析 expose 指令：`expose <上游模式> <对外名模式>`。
//
// 两个取值缺一不可：只写一个模式表达不出「改成什么」，而猜一个（例如把捕获值原样保留）
// 会让一个只想写过滤规则的人得到一条静默的改名规则。
func (p *parser) appendAlias(ln line, endpoint *Endpoint) error {
	head := ln.tokens[0]
	switch {
	case len(ln.tokens) < 3:
		return errorf(ln.file, head.line, valueColumn(head),
			"%s 需要两个取值（形如 expose * sensenova/*）", directiveExpose)
	case len(ln.tokens) > 3:
		extra := ln.tokens[3]
		return errorf(ln.file, extra.line, extra.col,
			"%s 至多接受两个取值（上游模式与对外名模式），多出来的是 %q",
			directiveExpose, extra.text)
	}

	from, err := p.expand(ln.tokens[1], ln)
	if err != nil {
		return err
	}
	to, err := p.expand(ln.tokens[2], ln)
	if err != nil {
		return err
	}
	if err := checkExposeFrom(from); err != nil {
		return errorf(ln.file, ln.tokens[1].line, ln.tokens[1].col, "%s", err)
	}
	if err := checkExposeTo(to); err != nil {
		return errorf(ln.file, ln.tokens[2].line, ln.tokens[2].col, "%s", err)
	}

	spec := discoverOf(endpoint, head)
	spec.Expose = append(spec.Expose, Alias{
		From: from,
		To:   to,
		File: ln.file,
		Line: ln.no,
		Col:  ln.tokens[1].col,
	})
	return nil
}

// checkExposeFrom 校验上游模式：恰一个 `*` 作捕获，不收 `?`。
func checkExposeFrom(pattern string) error {
	if err := checkPattern(pattern); err != nil {
		return err
	}
	if strings.Contains(pattern, "?") {
		return fmt.Errorf("上游模式 %q 不收 ?：改名要捕获一段原文，与「匹配一个字符」混用时"+
			"「捕获得哪一段」没有确定答案", pattern)
	}
	if strings.Count(pattern, "*") != 1 {
		return fmt.Errorf("上游模式 %q 必须含恰好一个 *（它决定捕获哪一段）", pattern)
	}
	return nil
}

// checkExposeTo 校验对外名模式：至多一个 `*`，不收 `?`。
func checkExposeTo(pattern string) error {
	if err := checkPattern(pattern); err != nil {
		return err
	}
	if strings.Contains(pattern, "?") {
		return fmt.Errorf("对外名模式 %q 不收 ?：捕获值只由 * 填充", pattern)
	}
	if strings.Count(pattern, "*") > 1 {
		return fmt.Errorf("对外名模式 %q 至多含一个 *", pattern)
	}
	return nil
}

// discoverOf 取端点的发现描述，没有就建一个占位。
//
// allow / deny 可能先于 discover 出现，因此占位记录的是当前第一条相关指令的位置；
// discover 后来出现时会把位置改成它自己那一行。占位只在「一直没有 discover」时
// 被当成错误对象，那时第一个位置就是报错该指的地方。
func discoverOf(endpoint *Endpoint, head token) *Discover {
	if endpoint.Discover == nil {
		endpoint.Discover = &Discover{File: head.file, Line: head.line, Col: head.col}
	}
	return endpoint.Discover
}

// checkPattern 校验一条过滤模式。
//
// 只拒空模式与含空白的模式：模型 id 里既不会有空串也不会有空白，这两种模式匹配不到
// 任何东西，写下来只会在「为什么这个模型不在清单里」上耗掉一轮排查。
func checkPattern(value string) error {
	switch {
	case value == "":
		return fmt.Errorf("过滤模式不能为空")
	case strings.ContainsAny(value, " \t"):
		return fmt.Errorf("过滤模式 %q 不能含空白", value)
	}
	return nil
}

// checkHTTPURL 报告一个地址是不是带主机名的 http(s) 地址。
func checkHTTPURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return fmt.Errorf("不是带主机名的 http(s) 地址")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("只支持 http 与 https")
	}
	return nil
}

// listingSegment 是清单接口路径的末段。
//
// 清单接口的末段都是它，因此推导规则只有一条：裁掉协议端点段，拼上这个末段。
const listingSegment = "/models"

// deriveListingURL 从端点地址推导清单地址：按协议端点段裁掉尾段，再拼 /models。
//
// 版本根原样保留，因此 /v1、/api/coding/paas/v4、/api/v3 走的是同一条规则；
// query 与 fragment 丢弃，清单接口不接受端点级参数。裁的判据与协议推导共用
// domain.Protocol.EndpointSegment，两处不会各认一套。
//
// 拿不准时返回 false，由调用方报「请显式写 discover <地址>」，不猜一个地址。
func deriveListingURL(endpointURL string, protocol domain.Protocol) (string, bool) {
	segment := protocol.EndpointSegment()
	if segment == "" {
		return "", false
	}
	// 带模型名占位符的地址是模板，不是一条确定的端点地址：按它推导出的清单地址里
	// 会留着占位符，直接请求必然 404。这类端点必须显式写 discover <地址>。
	if strings.Contains(endpointURL, modelPlaceholder) {
		return "", false
	}
	parsed, err := url.Parse(endpointURL)
	if err != nil || parsed.Host == "" || parsed.Scheme == "" {
		return "", false
	}
	trimmed := strings.TrimSuffix(parsed.Path, "/")
	if !strings.HasSuffix(trimmed, segment) {
		return "", false
	}
	parsed.Path = strings.TrimSuffix(trimmed, segment) + listingSegment
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), true
}

// finishProvider 校验一条渠道，并在协议省略时从地址把它推出来。
func (p *parser) finishProvider(provider *Provider) error {
	if len(provider.Accounts) == 0 {
		return errorf(provider.File, provider.Line, provider.Col,
			"provider %s 没有任何凭据：至少需要一条 %s",
			provider.Name, directiveAPIKey)
	}
	if len(provider.Endpoints) == 0 {
		return errorf(provider.File, provider.Line, provider.Col,
			"provider %s 没有任何端点：端点由 url 与 model 声明，"+
				"既可以写在 provider 一级，也可以写成 endpoint 子块", provider.Name)
	}
	for i := range provider.Endpoints {
		if err := p.finishEndpoint(provider, &provider.Endpoints[i]); err != nil {
			return err
		}
	}
	// 按声明位置排序：同名模型之间的回退顺序就是它。
	sort.SliceStable(provider.Endpoints, func(i, j int) bool {
		return provider.Endpoints[i].Line < provider.Endpoints[j].Line
	})
	p.warnAccountPolicy(provider)
	return nil
}

// warnAccountPolicy 在账号池与调度声明对不上时记一条提醒。
//
// 两种对不上都是「写下的配置没有作用对象」：balance 对着一个账号无处分摊，
// 权重在按声明顺序调度时轮不到生效。它们不阻止加载——单账号加 balance 是一份
// 将来准备扩到多账号的配置——但不能静默，否则使用者以为写下的那个数已经生效了。
func (p *parser) warnAccountPolicy(provider *Provider) {
	if provider.Balanced && len(provider.Accounts) == 1 {
		p.cfg.Warnings = append(p.cfg.Warnings, Warning{
			File: provider.File,
			Line: provider.Line,
			Msg: fmt.Sprintf("provider %s 写了 %s，但只声明了一个账号，没有可分摊的对象",
				provider.Name, directiveBalance),
		})
		return
	}
	if provider.Balanced {
		return
	}
	for _, account := range provider.Accounts {
		if account.Weight == 1 {
			continue
		}
		p.cfg.Warnings = append(p.cfg.Warnings, Warning{
			File: account.File,
			Line: account.Line,
			Msg: fmt.Sprintf("provider %s 未写 %s，账号按声明顺序调度，这条 %s 的权重没有作用",
				provider.Name, directiveBalance, directiveAPIKey),
		})
		return
	}
}

// finishEndpoint 校验一条端点并补齐缺省值。
func (p *parser) finishEndpoint(provider *Provider, endpoint *Endpoint) error {
	where := endpointWhere(provider, endpoint)
	fail := func(format string, args ...any) error {
		return errorf(endpoint.File, endpoint.Line, endpoint.Col, format, args...)
	}

	if endpoint.URL == "" {
		return fail("%s 缺少 url", where)
	}
	parsed, err := url.Parse(endpoint.URL)
	if err != nil || parsed.Host == "" {
		return fail("%s 的地址 %q 不是带主机名的 http(s) 地址", where, endpoint.URL)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fail("%s 的地址 %q 只支持 http 与 https", where, endpoint.URL)
	}

	derived, ok := protocolForURLPath(parsed.Path)
	if !ok {
		return fail("%s 的地址 %q 末段认不出线协议（能识别的末段是 %s）",
			where, endpoint.URL, strings.Join(protocolSegments(), " / "))
	}
	switch {
	case endpoint.Protocol == "":
		endpoint.Protocol = derived
	case endpoint.Protocol != derived:
		return fail("%s 写了 protocol %s，但地址 %q 推出的是 %s："+
			"地址与协议各说各话时，网关按哪种线格式与上游通信没有正确答案",
			where, endpoint.Protocol, endpoint.URL, derived)
	}

	if endpoint.Protocol == domain.ProtocolGemini {
		if err := p.finishGeminiEndpoint(provider, endpoint); err != nil {
			return err
		}
	}

	if err := p.finishDiscovery(provider, endpoint); err != nil {
		return err
	}
	if len(endpoint.Models) == 0 && endpoint.Discover == nil {
		return fail("%s 没有声明任何 model；端点没有对外模型名就没有选路依据；"+
			"模型由上游清单决定时写一行 discover", where)
	}
	if endpoint.Timeout <= 0 {
		endpoint.Timeout = defaultTimeout
	}
	return nil
}

// finishDiscovery 校验端点的模型发现描述，并在省略地址时把清单地址推导出来。
func (p *parser) finishDiscovery(provider *Provider, endpoint *Endpoint) error {
	spec := endpoint.Discover
	if spec == nil {
		return nil
	}
	where := endpointWhere(provider, endpoint)
	if !spec.Declared {
		return errorf(spec.File, spec.Line, spec.Col,
			"%s 写了 allow / deny / expose 却没有 discover：这些规则要有一条 discover 才有作用对象",
			where)
	}
	// 一条过滤规则都不写的端点会把上游清单里的模型全数暴露给客户端。
	// 这不是错误（“上游有什么就用什么”是合理用法），但它是“准入集合完全由上游决定”，
	// 值得在启动时说一句。
	if len(spec.Allow) == 0 && len(spec.Deny) == 0 {
		p.cfg.Warnings = append(p.cfg.Warnings, Warning{
			File: spec.File,
			Line: spec.Line,
			Msg: fmt.Sprintf("%s 声明了 discover 但没有 allow / deny：清单里的模型会全部暴露给客户端",
				where),
		})
	}
	if spec.URL != "" {
		return nil
	}
	derived, ok := deriveListingURL(endpoint.URL, endpoint.Protocol)
	if !ok {
		return errorf(endpoint.File, endpoint.Line, endpoint.Col,
			"%s 的地址 %q 推不出清单地址；请显式写 discover <清单地址>", where, endpoint.URL)
	}
	spec.URL = derived
	spec.Derived = true
	return nil
}

// modelPlaceholder 是 Gemini 端点地址里必须出现的模型名占位符。
//
// 写成取值位置上的字面记号而不是另加一条配置指令：模型名是选路的产物（一条端点下
// 可以有多个 model），地址是端点级事实，两者只能靠一处占位符在发送前合到一起。
const modelPlaceholder = "{model}"

// finishGeminiEndpoint 校验 Gemini 端点地址的两项特殊要求。
//
// 它拦的两件事都会在运行期变成「发出去的地址不对」：没有占位符时所有模型都打到同一个
// 模型上；断言清单形状又推不出清单地址时，端点要么装配失败要么拿回一份看不懂的清单。
// 在这里报出比让上游回一个 404 更容易定位。
func (p *parser) finishGeminiEndpoint(provider *Provider, endpoint *Endpoint) error {
	where := endpointWhere(provider, endpoint)
	if endpoint.Discover != nil {
		return errorf(endpoint.File, endpoint.Line, endpoint.Col,
			"%s 是 gemini 端点，本版不支持 discover：Gemini 的清单响应形状（models 数组）"+
				"与 OpenAI / Anthropic 不同，且端点地址带模型名占位符、推不出清单地址；"+
				"请用 model 逐条声明（需要过滤时先列举可用 id）", where)
	}
	if !strings.Contains(endpoint.URL, modelPlaceholder) {
		return errorf(endpoint.File, endpoint.Line, endpoint.Col,
			"%s 是 gemini 端点，地址里必须用 %s 指代本次请求的模型名："+
				"Gemini 把模型名与动作都写在路径上，请求体里没有这两项，缺失时会把所有模型都发到同一个模型上",
			where, modelPlaceholder)
	}
	return nil
}

// addProvider 把一条校验过的渠道收进配置，并拒绝重名。
//
// 名字虽然不参与选路，但它出现在日志与每一条相关错误的文案里。重名会让那些输出
// 指向两条都有可能的位置，排障时会先在这里绕一圈。
func (p *parser) addProvider(provider Provider) error {
	for _, existing := range p.cfg.Providers {
		if existing.Name != provider.Name {
			continue
		}
		return errorf(provider.File, provider.Line, provider.Col,
			"provider %s 已在 %s 第 %d 行声明过；让它独一无二，日志与报错才指得准",
			provider.Name, existing.File, existing.Line)
	}
	p.cfg.Providers = append(p.cfg.Providers, provider)
	return nil
}

// collectBlockBody 收集一个块的块体；返回时 p.pos 指向块后的第一行。
func (p *parser) collectBlockBody(opener line) ([]line, error) {
	var body []line
	for {
		if p.pos >= len(p.lines) {
			return nil, errorf(opener.file, opener.tokens[0].line, opener.tokens[0].col,
				"%s 块没有收尾的 }", opener.tokens[0].text)
		}
		ln := p.lines[p.pos]
		if isBlockClose(ln) {
			p.pos++
			return body, nil
		}
		if _, ok := blockOpener(ln); ok {
			return nil, errorf(ln.file, ln.tokens[0].line, ln.tokens[0].col,
				"%s 块里不能再开块", opener.tokens[0].text)
		}
		body = append(body, ln)
		p.pos++
	}
}

// isBlockClose 报告一行是否只是一个块结束符。
func isBlockClose(ln line) bool {
	return len(ln.tokens) == 1 && ln.tokens[0].kind == tokenBlockClose
}

// blockOpener 报告一行是否在开一个块。
//
// 判据是这一行以 `{` 收尾。`provider x {` 与 `endpoint <地址> {` 都落在这一条上；
// 行里出现的占位符 `{env.K}` 是与取值并列的独立记号，不会让它误判成块头。
func blockOpener(ln line) (line, bool) {
	if len(ln.tokens) < 2 {
		return line{}, false
	}
	return ln, ln.tokens[len(ln.tokens)-1].kind == tokenBlockOpen
}

// endpointWhere 把一条端点描述成人读的定位文本。
//
// 用于那些指向整条端点、而没有某一列可指的错误：把「是哪条端点」写进消息里，
// 比只给一个行号更容易在有一堆端点的配置里对上号。
func endpointWhere(provider *Provider, endpoint *Endpoint) string {
	if endpoint.Default {
		return fmt.Sprintf("provider %s 的默认端点", provider.Name)
	}
	return fmt.Sprintf("provider %s 的端点 %s", provider.Name, endpoint.URL)
}

// protocolForURLPath 从上游地址的路径推导线协议。
//
// 比的是端点段而不是整条路径：版本根由各家供应商自定（OpenAI 系是 /v1，
// 智谱是 /api/coding/paas/v4，火山方舟是 /api/v3），拿完整路径去比会把它们一并拒掉，
// 而它们的线格式其实完全一致。
func protocolForURLPath(path string) (domain.Protocol, bool) {
	trimmed := strings.TrimSuffix(path, "/")
	for _, protocol := range allProtocols() {
		for _, segment := range protocol.EndpointSegments() {
			if segment != "" && strings.HasSuffix(trimmed, segment) {
				return protocol, true
			}
		}
	}
	return "", false
}

// protocolSegments 列出能被识别成协议的地址末段，用于报错文案。
func protocolSegments() []string {
	var out []string
	for _, protocol := range allProtocols() {
		out = append(out, protocol.EndpointSegments()...)
	}
	return out
}

// protocolNames 列出协议名，用于报错文案。
func protocolNames() []string {
	out := make([]string, 0, len(allProtocols()))
	for _, protocol := range allProtocols() {
		out = append(out, string(protocol))
	}
	return out
}

// allProtocols 是本版支持的线协议。
//
// 顺序固定：报错文案里列出的可选协议、地址末段的比对顺序都用它，
// 因此同一条错误在两次运行里给出同样的措辞与同样的建议。
func allProtocols() []domain.Protocol {
	return []domain.Protocol{
		domain.ProtocolOpenAIChat,
		domain.ProtocolOpenAIResponses,
		domain.ProtocolAnthropicMessages,
		domain.ProtocolGemini,
	}
}

// parseProtocol 把指令取值解析成线协议。
func parseProtocol(value string) (domain.Protocol, error) {
	protocol := domain.Protocol(value)
	if !protocol.Valid() {
		return "", fmt.Errorf("线协议 %q 不认识", value)
	}
	return protocol, nil
}

// describeLine 把一行还原成便于放进错误消息的简短文本。
func describeLine(ln line) string {
	parts := make([]string, len(ln.tokens))
	for i, t := range ln.tokens {
		parts[i] = t.text
	}
	return strings.Join(parts, " ")
}
