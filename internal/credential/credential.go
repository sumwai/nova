// Package credential 按路由上的凭据引用取出明文密钥，并按本次路由的协议拼成请求头。
//
// 职责边界：
//   - 只负责「按引用选凭据、按本次路由的协议拼成请求头」这一件事，不读数据库、不读环境变量、不做解密；
//   - 同时提供 upstream.HeaderProvider 的契约实现，把凭据头与渠道级静态头合并后交给上游客户端。
//
// 之所以独立成包而不塞进 upstream：请求头的注入形态由上游协议决定，属配置与装配层的事实，
// upstream 只消费结果，不感知凭据从哪来、以什么形态拼装。
package credential

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/sumwai/nova/internal/domain"
)

// 凭据请求头的标准名。Authorization 是 OpenAI 系两家（Chat 与 Responses）的形态，
// x-api-key 是 Anthropic Messages 的形态。写成常量避免字面量在多处漂移。
const (
	headerAuthorization = "Authorization"
	headerXAPIKey       = "x-api-key"
)

// 凭据被打印时一律替换成占位符，且不含引用名与协议：列出任何一项都会让日志成为
// 「哪份密钥属于哪个渠道」的索引，排障需要时改由调用方显式打印引用名。
const (
	// 取值里的 credential 只是给读日志的人一个「脱敏结果」的标签，本身不含任何密钥；
	// 它按字面量形状命中了 gosec 的硬编码凭据启发式（G101），那是误报，故就地豁免。
	redactedCredential = "credential(<redacted>)" //nolint:gosec // G101：这是脱敏后的占位符，不是凭据。
	redactedProvider   = "provider(<redacted>)"
	redactedSecret     = "<redacted>"
)

// Credential 是一份上游渠道的凭据：只回答「用哪份密钥」。
//
// 协议不在这里：线协议是本次路由（即端点）的事实，同一份凭据可以服务走不同协议的多个端点
// （例如同一家的 OpenAI 兼容端点与 Anthropic 兼容端点共用一份密钥）。
// 若把协议钉在凭据上，一份凭据就只能对应一种注入形态，这个场景无从表达；
// 注入形态因此由 route.Protocol 决定（见 UpstreamHeaders），凭据只提供密钥本身。
type Credential struct {
	// APIKey 是明文密钥，由配置层展开 {env.NAME} 后给出。
	APIKey string
}

// String 实现 fmt.Stringer：凭据经 %v 或 %+v 打印时只出现占位符。
//
// 它与 LogValue 一起把「密钥不明文外泄」固化成类型自身的约束：本类型有导出的 APIKey 字段，
// 一旦有人用 %+v 或 slog.Any 打印就会把明文密钥写进日志，而泄漏一旦发生无法收回。
// 这两条方法覆盖 fmt 与 slog 两条常见打印路径，误打印时也只剩一个占位符。
// 它们不参与鉴权，注入上游的仍是 APIKey 本身，行为不受影响。
func (c Credential) String() string {
	return redactedCredential
}

// LogValue 实现 slog.LogValuer：凭据作为日志字段时只出现占位符。
//
// 与 String 同一目的，覆盖 structured logging 这条路径——它不经 fmt，Stringer 拦不住。
func (c Credential) LogValue() slog.Value {
	return slog.StringValue(redactedSecret)
}

// Provider 是凭据表的请求头提供者：按 route.CredentialRef 取出本次调用该用的那一份凭据。
//
// 表在装配期一次性建好，运行期只读，因此可并发使用。
// 引用名在表里查不到时返回错误，不退化成空密钥：
// 空密钥会把「装配漏了一个渠道的凭据」推迟成上游的 401，让使用者从一个与配置无关的症状出发排查。
type Provider struct {
	credentials map[string]Credential
}

// String 实现 fmt.Stringer：凭据表经 %v 或 %+v 打印时只出现占位符。
//
// 打印整张表会一次列出全部引用名与密钥，泄漏面比单份凭据更大，因此这里连引用名也不输出。
// 与 Credential 的两条方法同一目的：误打印不落明文，且不改变任何鉴权行为。
func (p Provider) String() string {
	return redactedProvider
}

// LogValue 实现 slog.LogValuer：凭据表作为日志字段时只出现占位符。
//
// 与 String 同一目的，覆盖 structured logging 这条路径——它不经 fmt，Stringer 拦不住。
func (p Provider) LogValue() slog.Value {
	return slog.StringValue(redactedSecret)
}

// New 用「引用名 → 凭据」的表构造请求头提供者，引用名即 domain.Route.CredentialRef。
//
// 入参整表拷贝：调用方之后继续持有并修改自己的表，不会改变已生效的凭据表。
// 允许空表与空密钥：空表使任何引用都取不到凭据并在请求上游前报错；
// 空密钥照原样注入空值头，让「渠道配了但密钥为空」以 401 暴露，而不是在装配期让网关起不来。
func New(credentials map[string]Credential) *Provider {
	table := make(map[string]Credential, len(credentials))
	for ref, cred := range credentials {
		table[ref] = cred
	}
	return &Provider{credentials: table}
}

// UpstreamHeaders 实现 upstream.HeaderProvider 的契约。
//
// 每次调用都返回一个全新的 http.Header（调用方会直接写入，不得返回共享 map），
// 内容 = 本次凭据头 + route.Headers。注入形态取自 route.Protocol：协议是路由（端点）的事实，
// 凭据只回答用哪份密钥；合并规则里凭据头是被上游用来鉴权的唯一来源，
// 因此与 route.Headers 同名冲突时以凭据头为准，配置里的静态头不能把它覆盖掉。
func (p *Provider) UpstreamHeaders(_ context.Context, route domain.Route) (http.Header, error) {
	cred, ok := p.credentials[route.CredentialRef]
	if !ok {
		return nil, domain.NewError(domain.CodeInternal,
			fmt.Sprintf("路由的凭据引用 %q 不在凭据表里", route.CredentialRef))
	}
	headers, credentialHeader, err := credentialHeaders(route.Protocol, cred.APIKey)
	if err != nil {
		return nil, err
	}
	mergeRouteHeaders(headers, credentialHeader, route.Headers)
	return headers, nil
}

// credentialHeaders 按本次路由的协议产出一份全新的凭据头，并返回凭据头的标准名。
//
// 第二个返回值供合并阶段跳过同名静态头；不支持的协议按平台内部错误返回，
// 不去猜一种注入形态——猜错会把凭据泄露到错误的请求头里，
// 而「协议取值为空」这类装配缺陷也会因此变成显式失败，而不是悄悄发一个必然 401 的请求。
func credentialHeaders(protocol domain.Protocol, apiKey string) (http.Header, string, error) {
	headers := make(http.Header)
	switch protocol {
	case domain.ProtocolOpenAIChat, domain.ProtocolOpenAIResponses:
		headers.Set(headerAuthorization, "Bearer "+apiKey)
		return headers, headerAuthorization, nil
	case domain.ProtocolAnthropicMessages:
		headers.Set(headerXAPIKey, apiKey)
		return headers, http.CanonicalHeaderKey(headerXAPIKey), nil
	default:
		return nil, "", domain.NewError(domain.CodeInternal,
			fmt.Sprintf("协议 %q 不支持凭据注入", string(protocol)))
	}
}

// mergeRouteHeaders 把渠道级静态头并入凭据头。
//
// routeHeaders 为 nil 时直接返回，不制造空写。写入时按标准名规整键名并单独复制值切片：
// http.Header 的键可能来自手写 map 字面量而非 Set，键名大小写不可信；同时复制值切片
// 使返回值与 route.Headers 完全脱钩，调用方改一个不会影响另一个。
func mergeRouteHeaders(dst http.Header, credentialHeader string, routeHeaders http.Header) {
	for name, values := range routeHeaders {
		if credentialHeader != "" && strings.EqualFold(name, credentialHeader) {
			continue
		}
		dst[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
	}
}
