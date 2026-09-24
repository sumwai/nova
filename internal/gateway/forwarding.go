package gateway

import (
	"context"
	"fmt"
	"net/url"

	"github.com/sumwai/nova/internal/adapters/anthropic"
	"github.com/sumwai/nova/internal/adapters/openaichat"
	"github.com/sumwai/nova/internal/adapters/openairesponses"
	"github.com/sumwai/nova/internal/config"
	"github.com/sumwai/nova/internal/credential"
	"github.com/sumwai/nova/internal/domain"
	"github.com/sumwai/nova/internal/pipeline"
	"github.com/sumwai/nova/internal/transport"
)

// 本文件是 config 包与各转发包之间唯一的接缝：provider / endpoint / model 在这里被
// 翻译成 domain.Route，凭据在这里建表，三个协议适配器在这里成单例。各转发包因此都
// 不认识 config 包，配置里的指令名也不会渗进转发逻辑。

// clientProtocols 是网关对外暴露的三种客户端协议，顺序即路径判定顺序。
//
// 只保留这一份清单：客户端路径、客户端适配器与协议名都由它派生，
// 多写一处就会在将来加协议时漏改一处。
var clientProtocols = []domain.Protocol{
	domain.ProtocolOpenAIChat,
	domain.ProtocolOpenAIResponses,
	domain.ProtocolAnthropicMessages,
}

// newAdapters 为三种协议各建一个适配器单例。
//
// 适配器内部不持有跨请求的业务状态（流式状态由 NewStream 派生），因此可按协议共享；
// 每个请求重新构造一遍只是白花的分配。
func newAdapters() map[domain.Protocol]domain.Adapter {
	return map[domain.Protocol]domain.Adapter{
		domain.ProtocolOpenAIChat:        openaichat.New(),
		domain.ProtocolOpenAIResponses:   openairesponses.New(),
		domain.ProtocolAnthropicMessages: anthropic.New(),
	}
}

// protocolForPath 把客户端请求路径映射为协议。
//
// 第二个返回值报告这个路径是否受支持。未注册路径返回 false，由入口层按 404 处理，
// 而不是当成某种缺省协议：猜错协议会给出一个格式对不上、原因却看不出来的错误体。
func protocolForPath(path string) (domain.Protocol, bool) {
	for _, protocol := range clientProtocols {
		if protocol.EndpointPath() == path {
			return protocol, true
		}
	}
	return "", false
}

// adapterResolver 供入口层按请求路径取客户端适配器。
//
// 路径到协议的对应关系不可配置：网关只暴露约定的三个端点，多开一个入口就多一份
// 要维护的事实来源，而「哪些路径能进来」本该是网关的固定形状。
func adapterResolver(adapters map[domain.Protocol]domain.Adapter) transport.AdapterResolver {
	return func(path string) (domain.Adapter, bool) {
		protocol, ok := protocolForPath(path)
		if !ok {
			return nil, false
		}
		adapter, exists := adapters[protocol]
		return adapter, exists
	}
}

// routesByModel 由配置构造「对外模型名 → 候选端点」的选路表。
//
// 一条候选对应一个「provider × endpoint × model」组合：同一条端点下的多个 model
// 各成一条候选，它们共用地址、协议、超时与凭据，只有上游模型名不同；同一个对外名
// 出现在多条端点下时，它们按声明顺序构成这个名字的回退链路。
//
// 复用 config 的 ModelNames 与 Routes 而不是自己遍历 Providers：选路的依据只有
// 对外模型名这一件事，它的去重与排序规则该由配置包定义一次。
func routesByModel(cfg *config.Config) map[string][]domain.Route {
	table := make(map[string][]domain.Route)
	for _, name := range cfg.ModelNames() {
		for _, route := range cfg.Routes(name) {
			table[name] = append(table[name], domain.Route{
				UpstreamID:    upstreamID(route.Provider, route.Endpoint),
				Protocol:      route.Endpoint.Protocol,
				UpstreamModel: route.Upstream,
				BaseURL:       route.Endpoint.URL,
				Timeout:       route.Endpoint.Timeout,
				CredentialRef: route.Provider,
			})
		}
	}
	return table
}

// upstreamID 是端点在上游尝试记录与熔断键里的标识：provider 名 + 空格 + 地址。
//
// nova 的端点没有名字（块头就是地址），因此标识里放地址。地址一律走 redactAddress：
// 地址原文可能带 userinfo，而这个标识会进日志。
//
// 它既不可逆也不是单射：一个本身就形如「provider 地址」的地址，与「两段拼起来」在
// 肉眼上无法区分。它只供观测使用，消费者不得解析它，也不得由它反推 provider 与地址。
func upstreamID(provider string, endpoint *config.Endpoint) string {
	return provider + " " + redactAddress(endpoint.URL)
}

// redactAddress 抹掉地址里的 userinfo 口令，供日志与标识使用。
//
// 用 url.Redacted 而不是自己切字符串：它按 RFC 3986 的 userinfo 文法处理，口令之外
// 的字符原样保留。解析失败时原样返回而不报错——日志脱敏不该成为转发失败的原因，
// 而配置层已经校验过地址形态，走到这里说明它是我们没预料到的写法。
func redactAddress(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User == nil {
		return raw
	}
	return parsed.Redacted()
}

// credentialsByRef 把配置里的渠道整理成「引用名 → 凭据」表。
//
// 引用名取 provider 名：它同时是 domain.Route.CredentialRef 的取值，两张表因此天然
// 对齐，不需要另建一套标识。凭据只带密钥，注入形态由本次路由的协议决定
// （见 internal/credential），因此这里不需要也不该记协议。
func credentialsByRef(providers []config.Provider) map[string]credential.Credential {
	table := make(map[string]credential.Credential, len(providers))
	for i := range providers {
		table[providers[i].Name] = credential.Credential{APIKey: providers[i].APIKey}
	}
	return table
}

// modelRouteResolver 是按对外模型名选路的实现。
type modelRouteResolver struct {
	routes map[string][]domain.Route
}

// Candidates 返回该 model 命中某个对外名时的候选渠道。
//
// 同协议的候选排在最前、跨协议的候选按原顺序留在链尾当回退（见
// domain.PreferRoutesForProtocol）：首选同协议可以省掉重建，把请求按原协议的字段原样
// 上发；跨协议的候选一条都不丢，因此同一个名字在两个协议端点下都可用时，另一个协议
// 仍是前一条失败后的后备。
//
// 未命中任何对外名时返回空候选与 nil 错误，把「该回什么错误码」留给流水线统一判定：
// 选路层只回答「有哪些候选」，不决定对外呈现。
func (r modelRouteResolver) Candidates(_ context.Context, req *domain.Request) ([]domain.Route, error) {
	if req == nil {
		return nil, nil
	}
	candidates := r.routes[req.Model]
	if len(candidates) == 0 {
		return nil, nil
	}
	// 返回值必须是新切片：候选表会被并发读，就地排序会让一个请求的选路次序
	// 影响另一个请求正在用的那份数据。
	return domain.PreferRoutesForProtocol(req.Protocol, candidates), nil
}

// forwarderOptions 组装流水线的依赖与边界。
func forwarderOptions(
	routes map[string][]domain.Route,
	adapters pipeline.AdapterLookup,
	caller domain.UpstreamCaller,
) pipeline.Options {
	return pipeline.Options{
		Adapters: adapters,
		Upstream: caller,
		Routes:   modelRouteResolver{routes: routes},
		// Observer 与 Breaker 都留零值，两者的理由不同：
		// 前者等日志输出重新设计时再接，现在没有能承载尝试记录的日志实现；
		// 后者上游本来就没有在生产装配里生效过（它的 router 包是死代码，没有搬过来）。
		MaxAttempts: maxUpstreamAttempts(routes),
	}
}

// maxUpstreamAttempts 返回单请求最多发起的上游尝试次数：整条候选链的长度。
//
// 上限必须覆盖整条链：流水线按「每条候选一次尝试」推进，上限取一个与候选数无关的
// 小常量时，排在它之后的候选永远轮不到，配置里写下的回退链路形同虚设。
//
// 这不会让「多配一个候选」无端多出重试：候选循环的推进条件是「本次失败且可重试」，
// 每条候选仍最多尝试一次，上限只是把循环的边界放到链尾。
func maxUpstreamAttempts(routes map[string][]domain.Route) int {
	longest := 0
	for _, candidates := range routes {
		if len(candidates) > longest {
			longest = len(candidates)
		}
	}
	if longest < 1 {
		return 1
	}
	return longest
}

// upstreamAdapterLookup 供上游客户端与流水线按上游协议取适配器。
//
// 返回未命名的函数类型，而不是某个包的命名类型：internal/upstream 与 internal/pipeline
// 各自定义了同形的命名类型（前者用 AdapterLookup 取响应解码器，后者用它做请求定稿）。
// 返回未命名类型，同一份实现就能直接满足两边，不必在某一边做一次无意义的类型转换。
func upstreamAdapterLookup(adapters map[domain.Protocol]domain.Adapter) func(domain.Protocol) (domain.Adapter, error) {
	return func(protocol domain.Protocol) (domain.Adapter, error) {
		adapter, ok := adapters[protocol]
		if !ok {
			return nil, fmt.Errorf("没有协议 %q 的适配器", string(protocol))
		}
		return adapter, nil
	}
}
