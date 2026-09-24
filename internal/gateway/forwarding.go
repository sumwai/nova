package gateway

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"

	"github.com/sumwai/nova/internal/adapters/anthropic"
	"github.com/sumwai/nova/internal/adapters/openaichat"
	"github.com/sumwai/nova/internal/adapters/openairesponses"
	"github.com/sumwai/nova/internal/catalog"
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

// adapterResolver 供入口层按请求取客户端适配器。
//
// 协议在转发路径上由路径决定，在模型清单上由请求头决定，两者都在这里回答；
// 清单路径因此不需要在入口层另挂一个只为了编码错误的适配器查找。
func adapterResolver(adapters map[domain.Protocol]domain.Adapter) transport.AdapterResolver {
	return func(r *http.Request) (domain.Adapter, bool) {
		protocol, ok := protocolForRequest(r)
		if !ok {
			return nil, false
		}
		adapter, exists := adapters[protocol]
		return adapter, exists
	}
}

// protocolForRequest 判定一次请求的客户端协议。
//
// 转发路径按路径判定，与既有行为一致；模型清单在 OpenAI 与 Anthropic 下是同一条路径，
// 判据改为 anthropic-version 头。带该头却请求转发路径的请求仍按路径判定：
// 头判定只作用于模型清单那一条路径。
func protocolForRequest(r *http.Request) (domain.Protocol, bool) {
	if protocol, ok := protocolForPath(r.URL.Path); ok {
		return protocol, true
	}
	if r.URL.Path == modelsPath {
		return protocolForModelsRequest(r), true
	}
	return "", false
}

// effectiveEndpoint 是一条端点及其本次装配生效的模型集合。
//
// 生效集合 = 显式声明的模型（按声明顺序在前）+ 从上游清单保留的模型（按上游返回顺序在后）。
// 它是「配置说的」与「上游现在有的」合并后的结果，选路表与对外目录都由它派生。
// 发现失败的端点落回只有显式声明的那一份。
type effectiveEndpoint struct {
	provider *config.Provider
	endpoint *config.Endpoint
	models   []config.Model
}

// resolveEndpointModels 合并显式声明与发现结果。
//
// 同名时显式声明胜出：它无条件生效（上游清单漏项时仍能用，也支撑发现失败时的降级），
// 而发现项只在清单命中时才有，它的对外名由 expose 决定。
// 这一条不算「同一端点内重复声明同一个对外名」，那条校验只对显式声明之间生效。
// 返回新切片，不修改端点上声明的模型集合。
func resolveEndpointModels(endpoint *config.Endpoint, discovered []catalog.Model) []config.Model {
	models := make([]config.Model, 0, len(endpoint.Models)+len(discovered))
	models = append(models, endpoint.Models...)

	declared := make(map[string]bool, len(endpoint.Models))
	for _, model := range endpoint.Models {
		declared[model.Name] = true
	}
	for _, model := range discovered {
		name := model.Name
		if name == "" {
			// 兼底：对外名缺省即上游 id。catalog 一定会填它，这里防的是
			// 调用方手搓 Model 时漏填，那会让目录里多出一个空名字。
			name = model.ID
		}
		if declared[name] {
			continue
		}
		declared[name] = true
		// 对外名取自 expose 改写后的结果，上游名始终是清单里的上游 id。
		models = append(models, config.Model{Name: name, Upstream: model.ID})
	}
	return models
}

// routesByModel 由生效端点集合构造「对外模型名 → 候选端点」的选路表。
//
// 一条候选对应一个「provider × endpoint × model」组合：同一条端点下的多个 model
// 各成一条候选，它们共用地址、协议、超时与凭据，只有上游模型名不同；同一个对外名
// 出现在多条端点下时，它们按声明顺序构成这个名字的回退链路。发现模型与显式模型
// 在同一条端点上排出同一种候选，因此两类来源在转发路径上没有区别。
//
// 账号池不在这里展开：它只有一个渠道内的账号序号这一个变量，而展开顺序要看本次请求
// 从哪个账号起算。展开因此留给请求期的 resolver（见 accountPool.order）。
func routesByModel(endpoints []effectiveEndpoint) map[string][]endpointRoute {
	table := make(map[string][]endpointRoute)
	for _, item := range endpoints {
		provider := item.provider.Name
		for _, model := range item.models {
			table[model.Name] = append(table[model.Name], endpointRoute{
				provider: provider,
				Route: domain.Route{
					UpstreamID:    upstreamID(provider, item.endpoint),
					Protocol:      item.endpoint.Protocol,
					UpstreamModel: model.Upstream,
					BaseURL:       item.endpoint.URL,
					Timeout:       item.endpoint.Timeout,
					CredentialRef: provider,
				},
			})
		}
	}
	return table
}

// modelEntries 由生效端点集合构造对外目录，按首次出现的顺序去重。
//
// 顺序取首次出现而不是字典序：这个列表会被 nova models 与数据面 /v1/models 展示，
// 让它与配置里读到的顺序一致，对着配置排查时不必来回换算位置。
func modelEntries(endpoints []effectiveEndpoint, discovered map[*config.Endpoint][]catalog.Model) []ModelEntry {
	var entries []ModelEntry
	seen := make(map[string]bool)

	// 发现项的展示名与创建时间按上游 id 索引：同一条端点内同名时显式声明优先，
	// 那条不应继承发现项的展示名。
	meta := make(map[string]catalog.Model)
	for _, models := range discovered {
		for _, model := range models {
			if _, ok := meta[model.ID]; !ok {
				meta[model.ID] = model
			}
		}
	}

	for _, item := range endpoints {
		declared := make(map[string]bool, len(item.endpoint.Models))
		for _, model := range item.endpoint.Models {
			declared[model.Name] = true
		}
		for _, model := range item.models {
			if seen[model.Name] {
				continue
			}
			seen[model.Name] = true
			entry := ModelEntry{Name: model.Name}
			if declared[model.Name] {
				entry.Source = ModelSourceStatic
			} else {
				entry.Source = ModelSourceDiscovered
				// 上游给的展示名与时间按上游 id 查：对外名可能已经被 expose 改写，
				// 拿它去查会查不到。
				if found, ok := meta[model.Upstream]; ok {
					entry.DisplayName = found.DisplayName
					entry.CreatedAt = found.CreatedAt
				}
			}
			entries = append(entries, entry)
		}
	}
	return entries
}

// upstreamID 是端点在上游尝试记录里的标识：provider 名 + 空格 + 主机名。
//
// 不写完整地址是有意的：scheme、版本根与端点路径对「分辨是哪条渠道」没有帮助，
// 却占了日志行里很大一截。主机名是能一眼分辨「打到哪台机器」的那部分，
// 而同一 provider 下多条端点打同一台机器的情形（同一家的两个协议端点），
// 由同一条记录里的 upstream_protocol 区分。
//
// 它不是可逆编码，也不是单射：一个本身就形如「provider 主机名」的主机名是无法防的，
// 但它只供观测使用，消费者不得解析它，也不得由它反推 provider 与地址。
func upstreamID(provider string, endpoint *config.Endpoint) string {
	return provider + " " + endpointHost(endpoint.URL)
}

// endpointHost 取地址里的主机名与端口。
//
// 取不到时退回抹掉 userinfo 的原文：配置层已经校验过地址形态，走到这里说明遇上了没预料到的
// 写法。此时宁可显示得长一点，也不要回一个空串——那会让「这条尝试打到了哪里」彻底消失。
func endpointHost(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return redactAddress(raw)
	}
	return parsed.Host
}

// redactAddress 抹掉地址里的 userinfo 口令，供日志使用。
//
// 先试 url.Redacted：对合法 URL 它按 RFC 3986 的 userinfo 文法处理，结果是标准的。
// 但它盖不住一种写法：地址没有主机名时（比如漏了 scheme 的 user:secret@host），
// url.Parse 会把整串当成 opaque 部分，此时 parsed.User 是 nil，Redacted 原样返回带口令的串。
// 配置层会拒掉这类地址，但这个函数是日志的最后一道出口——那时候更不能把口令漏出去，
// 所以再按 userinfo 文法切一层。
func redactAddress(raw string) string {
	if parsed, err := url.Parse(raw); err == nil && parsed.User != nil {
		return parsed.Redacted()
	}
	at := strings.LastIndex(raw, "@")
	if at < 0 {
		return raw
	}
	colon := strings.Index(raw[:at], ":")
	if colon < 0 {
		// 只有用户名没有口令：用户名本身不是秘密，原样保留。
		return raw
	}
	return raw[:colon+1] + maskedCredential + raw[at:]
}

// maskedCredential 是口令被抹掉后留下的替换串，与 upstream 那侧的写法一致。
const maskedCredential = "xxxxx"

// credentialsByRef 把配置里的渠道整理成「引用名 → 账号表」表。
//
// 引用名取 provider 名：它同时是 domain.Route.CredentialRef 的取值，两张表因此天然
// 对齐，不需要另建一套标识。每个渠道的账号切片按声明顺序保存，
// 因此「该渠道的默认账号」有确定答案（第一项），不依赖 map 的遍历顺序。
// 凭据只带密钥，注入形态由本次路由的协议决定（见 internal/credential），
// 因此这里不需要也不该记协议。
func credentialsByRef(providers []config.Provider) map[string][]credential.Account {
	table := make(map[string][]credential.Account, len(providers))
	for i := range providers {
		provider := &providers[i]
		accounts := make([]credential.Account, 0, len(provider.Accounts))
		for _, account := range provider.Accounts {
			accounts = append(accounts, credential.Account{Ref: account.Ref(), APIKey: account.APIKey})
		}
		table[provider.Name] = accounts
	}
	return table
}

// modelRouteResolver 是按对外模型名选路的实现。
type modelRouteResolver struct {
	routes map[string][]endpointRoute
	// accounts 按渠道存账号池；models 按对外名存候选分摊段。
	// 两层各有一颗轮转计数器：外层决定先试哪个渠道，内层决定先试哪份凭据。
	accounts map[string]*accountPool
	models   map[string]*modelCandidatePool
}

// endpointRoute 是一条端点级候选：一个对外模型名落到某条端点上的结果。
//
// 它嵌 domain.Route 而不另建一套字段，是为了让选路表在未展开账号时
// 就已经是一条可直接交给流水线的路由；账号只差一个取值。
// provider 单独存一份，供请求期按渠道取账号池：
// UpstreamID 里虽然也有渠道名，但那是一个给人读的字符串，不得由它反推渠道。
type endpointRoute struct {
	domain.Route
	provider string
}

// accountPool 是一个渠道的账号池及其轮转状态。
//
// 只有声明了多个账号的渠道才有池：单账号不需要一个稳定的账号标识，
// AccountRef 因此留空串，日志与凭据表与多账号之前的行为逐字一致。
type accountPool struct {
	// refs 是账号引用，weights 是与它一一对应的分摊权重，total 是权重之和。
	refs    []string
	weights []int
	total   int
	// balanced 报告这个池按权重轮询分摊；为假时按声明顺序调度。
	balanced bool
	// counter 是轮转计数，自增一次就把起点前移一个位置。
	// 它挂在渠道而不是模型上：同一渠道下不同模型共享一条轮转序列，
	// 否则并发请求会各查各的计数器而全压在同一个账号上。
	counter atomic.Uint64
}

// order 返回本次请求使用的账号顺序。
//
// 不分摊时就是声明顺序，每个请求完全一样。分摊时把起点按权重前移：
// 先按累计权重找出这次轮到谁，再把它后面的账号按声明顺序接上。
// 链尾保留全部账号，因此分摊只改起点，不改「失败换下一个」的回退语义。
func (p *accountPool) order() []string {
	if !p.balanced {
		return append([]string(nil), p.refs...)
	}

	// Add 返回自增后的值，减一才是本次的轮转位置。
	position := int((p.counter.Add(1) - 1) % uint64(p.total))
	first := 0
	acc := 0
	for i, weight := range p.weights {
		acc += weight
		if position < acc {
			first = i
			break
		}
	}

	out := make([]string, 0, len(p.refs))
	out = append(out, p.refs[first])
	for i := range p.refs {
		if i != first {
			out = append(out, p.refs[i])
		}
	}
	return out
}

// newModelRouteResolver 由生效端点集合与配置建选路表，并返回装配期的两条提醒。
//
// 提醒只能在装配期算：一条规则能不能命中对外名、一个候选渠道有没有作用对象，
// 都取决于此时实际存在的对外名集合（含从清单发现来的那些）。
func newModelRouteResolver(endpoints []effectiveEndpoint, cfg *config.Config) (*modelRouteResolver, []config.Warning) {
	routes, models, warnings := applyModelRoutes(routesByModel(endpoints), cfg)
	return &modelRouteResolver{
		routes:   routes,
		accounts: accountPools(cfg.Providers),
		models:   models,
	}, warnings
}

// modelCandidatePool 是一个对外模型名下参与分摊的候选段及其轮转状态。
//
// 段是「规则里列出的一个渠道」在候选链上的连续区间：同一渠道的多条端点候选
// 不被拆开，先试完这条渠道再换下一条。未列出的候选不在段里，接在链尾当回退。
type modelCandidatePool struct {
	segments []candidateSegment
	// tail 是参与分摊的候选在链上的长度：[0, tail) 是分摊段，之后是回退段。
	tail    int
	total   int
	counter atomic.Uint64
}

// candidateSegment 是链上的一段候选及其权重。
type candidateSegment struct {
	start, end int
	weight     int
}

// order 返回本次请求使用的候选顺序：分摊段按权重轮转起点，回退段原样接在后面。
//
// 只有一段时不重排：起点永远是它，重排只是白花一次拷贝。
func (p *modelCandidatePool) order(candidates []endpointRoute) []endpointRoute {
	if len(p.segments) <= 1 || p.total <= 0 {
		return candidates
	}
	position := int((p.counter.Add(1) - 1) % uint64(p.total))
	first := 0
	acc := 0
	for i, segment := range p.segments {
		acc += segment.weight
		if position < acc {
			first = i
			break
		}
	}

	out := make([]endpointRoute, 0, len(candidates))
	for _, segment := range p.segments[first:] {
		out = append(out, candidates[segment.start:segment.end]...)
	}
	for _, segment := range p.segments[:first] {
		out = append(out, candidates[segment.start:segment.end]...)
	}
	return append(out, candidates[p.tail:]...)
}

// applyModelRoutes 按规则重排每个对外名下的候选，并为分摊的模型建轮转状态。
//
// 候选分三段：主用候选（`provider` 行，按书写顺序，写 balance 时按权重轮转）→
// 显式回退候选（`fallback` 行，按书写顺序，不参与轮转）→ 规则没提到的渠道
// （按声明顺序，最后兜底）。后两段永远待在前一段之后，因此「主用全部失败才切换」
// 是结构上的事实，而不是靠权重或运气。
//
// 返回的提醒对应两件事：一条规则没有命中任何对外名；一条规则里的某个渠道
// 没有提供任何匹配该模式的模型。两者都只能在装配期回答，因为对外名集合包含
// 从上游清单发现来的那些。
func applyModelRoutes(
	routes map[string][]endpointRoute,
	cfg *config.Config,
) (map[string][]endpointRoute, map[string]*modelCandidatePool, []config.Warning) {
	out := make(map[string][]endpointRoute, len(routes))
	models := make(map[string]*modelCandidatePool)

	hit := make([]bool, len(cfg.ModelRoutes))
	seenCandidate := make([]map[string]bool, len(cfg.ModelRoutes))
	for i := range seenCandidate {
		seenCandidate[i] = map[string]bool{}
	}

	for name, chain := range routes {
		index := cfg.ModelRouteIndexFor(name)
		if index < 0 {
			out[name] = chain
			continue
		}
		hit[index] = true
		rule := &cfg.ModelRoutes[index]

		// 按渠道分组：同一渠道的多条端点候选保持相对声明顺序，整组移动。
		groups, at := groupByProvider(chain)
		used := make([]bool, len(groups))

		var preferred []endpointRoute
		var fallbacks []endpointRoute
		var segments []candidateSegment
		total := 0
		for _, candidate := range rule.Candidates {
			group, ok := at[candidate.Provider]
			if !ok {
				continue
			}
			seenCandidate[index][candidate.Provider] = true
			used[group] = true
			if candidate.Fallback {
				fallbacks = append(fallbacks, groups[group]...)
				continue
			}
			start := len(preferred)
			preferred = append(preferred, groups[group]...)
			if rule.Balanced {
				segments = append(segments, candidateSegment{
					start: start, end: len(preferred), weight: candidate.Weight,
				})
				total += candidate.Weight
			}
		}

		// 三段依次拼起来：主用候选 → fallback 行声明的回退候选 → 规则没提到的渠道。
		// 后两段都不参与轮转，它们只在前面全部失败之后才被用到。
		tail := len(preferred)
		reordered := append(preferred, fallbacks...)
		for i, group := range groups {
			if !used[i] {
				reordered = append(reordered, group...)
			}
		}
		out[name] = reordered
		// 有规则就建池，即使没写 balance（segments 为空）：它同时是一个标记，
		// 告诉请求期这个名字的候选顺序已经由规则定下，不再做同协议优先的隐式划分。
		models[name] = &modelCandidatePool{segments: segments, tail: tail, total: total}
	}
	return out, models, routeWarnings(cfg.ModelRoutes, hit, seenCandidate)
}

// groupByProvider 把一条候选链按渠道切成若干组，并返回渠道名到组下标。
//
// 组内保持原顺序，组的顺序按首次出现：同一渠道的多条端点候选因此不会被拆开。
func groupByProvider(chain []endpointRoute) ([][]endpointRoute, map[string]int) {
	at := make(map[string]int, len(chain))
	var groups [][]endpointRoute
	for _, candidate := range chain {
		index, ok := at[candidate.provider]
		if !ok {
			index = len(groups)
			at[candidate.provider] = index
			groups = append(groups, nil)
		}
		groups[index] = append(groups[index], candidate)
	}
	return groups, at
}

// routeWarnings 把两条「写了但没起作用」的事实整理成带位置的提醒。
//
// 一条规则没命中任何对外名时不再逐条报它的渠道：那种情况下每个渠道都没有作用对象，
// 报一次模式对不上就够，逐条报只会把真正的信号淹掉。
func routeWarnings(
	rules []config.ModelRoute,
	hit []bool,
	seenCandidate []map[string]bool,
) []config.Warning {
	var warnings []config.Warning
	for i := range rules {
		rule := &rules[i]
		if !hit[i] {
			warnings = append(warnings, config.Warning{
				File: rule.File,
				Line: rule.Line,
				Msg: fmt.Sprintf("route %s 没有命中任何对外名；"+
					"检查模式是否与 provider 里声明的模型名一致（匹配大小写敏感）", rule.Pattern),
			})
			continue
		}
		for _, candidate := range rule.Candidates {
			if seenCandidate[i][candidate.Provider] {
				continue
			}
			warnings = append(warnings, config.Warning{
				File: candidate.File,
				Line: candidate.Line,
				Msg: fmt.Sprintf("route %s 里的渠道 %q 没有提供任何匹配该模式的模型",
					rule.Pattern, candidate.Provider),
			})
		}
	}
	return warnings
}

// accountPools 按渠道建账号池，只保留声明了多个账号的渠道。
func accountPools(providers []config.Provider) map[string]*accountPool {
	pools := make(map[string]*accountPool, len(providers))
	for i := range providers {
		provider := &providers[i]
		if !provider.HasAccountPool() {
			continue
		}
		pool := &accountPool{
			refs:     make([]string, 0, len(provider.Accounts)),
			weights:  make([]int, 0, len(provider.Accounts)),
			balanced: provider.Balanced,
		}
		for _, account := range provider.Accounts {
			pool.refs = append(pool.refs, account.Ref())
			pool.weights = append(pool.weights, account.Weight)
			pool.total += account.Weight
		}
		pools[provider.Name] = pool
	}
	return pools
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
func (r *modelRouteResolver) Candidates(_ context.Context, req *domain.Request) ([]domain.Route, error) {
	if req == nil {
		return nil, nil
	}
	candidates := r.routes[req.Model]
	if len(candidates) == 0 {
		return nil, nil
	}
	// 候选层先轮转：决定这次先试哪个渠道；账号展开再跟在它后面。
	// 命中规则时不再做同协议优先：规则是显式写下的顺序，跨协议的候选同样是被
	// 明确列出的等价来源，把它推到链尾会让分摊失效。没写规则时才用这条隐式优化。
	ruled := false
	if pool := r.models[req.Model]; pool != nil {
		candidates = pool.order(candidates)
		ruled = true
	}

	// orders 缓存本次请求每个渠道的账号顺序：同一渠道的多条端点候选共用一个起点，
	// 否则一个请求会在 E1 上从 #2 开始、在 E2 上又从 #1 开始。
	orders := make(map[string][]string, len(r.accounts))
	expanded := make([]domain.Route, 0, len(candidates))
	for _, candidate := range candidates {
		order, cached := orders[candidate.provider]
		if !cached {
			if pool := r.accounts[candidate.provider]; pool != nil {
				order = pool.order()
			}
			orders[candidate.provider] = order
		}
		if len(order) == 0 {
			expanded = append(expanded, candidate.Route)
			continue
		}
		for _, ref := range order {
			route := candidate.Route
			route.AccountRef = ref
			expanded = append(expanded, route)
		}
	}

	// 返回值必须是新切片：候选表会被并发读，就地排序会让一个请求的选路次序
	// 影响另一个请求正在用的那份数据。
	if !ruled {
		expanded = domain.PreferRoutesForProtocol(req.Protocol, expanded)
	}
	return expanded, nil
}

// maxCandidates 返回单个模型展开账号后最长的候选链长度。
//
// 上限必须覆盖整条链：流水线按「每条候选一次尝试」推进，上限取一个与候选数无关的
// 小常量时，排在它之后的候选永远轮不到，配置里写下的回退链路形同虚设。
//
// 这不会让「多配一个账号」无端多出重试：候选循环的推进条件是「本次失败且可重试」，
// 每条候选仍最多尝试一次，上限只是把循环的边界放到链尾。
func (r *modelRouteResolver) maxCandidates() int {
	longest := 0
	for _, candidates := range r.routes {
		total := 0
		for _, candidate := range candidates {
			total++
			if pool := r.accounts[candidate.provider]; pool != nil {
				total += len(pool.refs) - 1
			}
		}
		if total > longest {
			longest = total
		}
	}
	if longest < 1 {
		return 1
	}
	return longest
}

// forwarderOptions 组装流水线的依赖与边界。
func forwarderOptions(
	resolver *modelRouteResolver,
	adapters pipeline.AdapterLookup,
	caller domain.UpstreamCaller,
	observer domain.Observer,
) pipeline.Options {
	return pipeline.Options{
		Adapters: adapters,
		Upstream: caller,
		Routes:   resolver,
		// Observer 记每次上游尝试：客户端只拿到聚合后的错误码，
		// 上游的状态码与响应片段只在尝试记录里可见，缺了它排障只能靠猜。
		Observer: observer,
		// Breaker 留零值：本版不做熔断，候选按声明顺序逐个尝试。
		// 上游那份实现（internal/router）在生产装配里本来就没生效过，没有搬过来。
		MaxAttempts: resolver.maxCandidates(),
	}
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
