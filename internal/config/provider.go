package config

import (
	"fmt"
	"net/url"
	"sort"
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
	// model 可以写多次，其余指令在同一个作用域里只能出现一次。
	if head.text != directiveModel {
		if err := p.rejectRepeat(head); err != nil {
			return err
		}
	}

	switch head.text {
	case directiveAPIKey:
		return p.setValue(ln, func(v token) error {
			value, err := p.expand(v, ln)
			if err != nil {
				return err
			}
			provider.APIKey = value
			return nil
		})

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
		if tk.text != directiveModel {
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
	return nil
}

// finishProvider 校验一条渠道，并在协议省略时从地址把它推出来。
func (p *parser) finishProvider(provider *Provider) error {
	if provider.APIKey == "" {
		return errorf(provider.File, provider.Line, provider.Col,
			"provider %s 缺少 api_key", provider.Name)
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
	return nil
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

	if len(endpoint.Models) == 0 {
		return fail("%s 没有声明任何 model；端点没有对外模型名就没有选路依据", where)
	}
	if endpoint.Timeout <= 0 {
		endpoint.Timeout = defaultTimeout
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
		segment := protocol.EndpointSegment()
		if segment != "" && strings.HasSuffix(trimmed, segment) {
			return protocol, true
		}
	}
	return "", false
}

// protocolSegments 列出能被识别成协议的地址末段，用于报错文案。
func protocolSegments() []string {
	out := make([]string, 0, len(allProtocols()))
	for _, protocol := range allProtocols() {
		out = append(out, protocol.EndpointSegment())
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
