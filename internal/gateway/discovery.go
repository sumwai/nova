package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sumwai/nova/internal/catalog"
	"github.com/sumwai/nova/internal/config"
	"github.com/sumwai/nova/internal/credential"
	"github.com/sumwai/nova/internal/domain"
)

// discoveryErrorDetailLimit 是写进发现失败文案的上游响应体最大字节数。
//
// 与上游调用那一侧取同一个量级：两处记录的是同一件事（上游回了什么），
// 口径不同只会让日志看起来像两种事实。
const discoveryErrorDetailLimit = 256

// DiscoveryReport 是一条端点的发现结果。
//
// 它同时供给三处：启动与 reload 的日志、`nova models` 的输出、启动横幅的目录汇总。
// 失败情形靠 Err 与 Degraded 两个字段区分：Err 是这次发现为什么没成，
// Degraded 是装配层据此作出的处置（退回该端点上显式声明的模型）。
type DiscoveryReport struct {
	Provider string
	Listing  string
	Protocol domain.Protocol
	// Shape 是判出的清单风格（openai / anthropic / unknown）；发现失败时为空。
	Shape catalog.Shape
	// Found 是清单里去重后的条目数，Kept 是过滤与改名之后保留的模型，
	// Filtered 是被过滤规则排除的条目数。
	Found    int
	Kept     []config.Model
	Filtered int
	// Declared 是这条端点显式声明的模型（含上游名映射），按声明顺序。
	// 它不属于清单视角的保留与排除，但同属这条端点实际提供的对外名，
	// 因此报告里单独列一段：只列清单那两组时，显式声明的名字（尤其是别名）会完全看不到。
	Declared []config.Model
	// UnusedAliases 是没有命中任何清单项的改名规则，供装配层记一条提醒。
	UnusedAliases []config.Alias
	// HasMore 报告清单自述还有下一页；本版不翻页。
	HasMore bool
	// Excluded 是被过滤规则排除的模型 id，`nova models` 用它列出被拦下的模型。
	Excluded []string
	// Degraded 报告这次发现失败，端点已退回只有显式声明的模型。
	Degraded bool
	// Err 是这次发现的失败原因；成功时为 nil。
	Err error
	// Duration 是这条端点发现耗时。
	Duration time.Duration

	// endpoint 指向这条报告对应的端点，供装配期判定「有没有显式模型可退回」。
	// 不导出：它只在装配期有意义，装配完成后调用方只看上面那些事实。
	endpoint *config.Endpoint
}

// discoveryTarget 是一条待发现的端点：配置里的端点，加上拆好的纯数据描述。
type discoveryTarget struct {
	provider string
	endpoint *config.Endpoint
	spec     catalog.Endpoint
}

// discoveryOutcome 是一条端点的发现结果，与 discoveryTarget 按下标对应。
type discoveryOutcome struct {
	result   catalog.Result
	err      error
	duration time.Duration
}

// discoveryTargets 列出配置里声明了发现的端点，按声明顺序。
func discoveryTargets(cfg *config.Config) []discoveryTarget {
	var targets []discoveryTarget
	for i := range cfg.Providers {
		provider := &cfg.Providers[i]
		for j := range provider.Endpoints {
			endpoint := &provider.Endpoints[j]
			if endpoint.Discover == nil {
				continue
			}
			targets = append(targets, discoveryTarget{
				provider: provider.Name,
				endpoint: endpoint,
				spec: catalog.Endpoint{
					Provider:      provider.Name,
					ListingURL:    endpoint.Discover.URL,
					Protocol:      endpoint.Protocol,
					CredentialRef: provider.Name,
					Timeout:       endpoint.Timeout,
					Allow:         endpoint.Discover.Allow,
					Deny:          endpoint.Discover.Deny,
					Expose:        exposeAliases(endpoint.Discover.Expose),
				},
			})
		}
	}
	return targets
}

// runDiscovery 并发取全部端点的清单，返回成功端点的模型集合与逐端点的报告。
//
// 并发是因为各端点的发现互相独立，而串行等待会把启动时间按端点数线性拉长；
// 结果按端点的声明顺序返回，日志与错误因此与配置里读到的顺序一致。
//
// 失败在这里不改变装配结果：有没有可退回的模型只有装配层知道（要看端点上写了几个 model），
// 处置与日志都由它决定。
func runDiscovery(ctx context.Context, cfg *config.Config, fetcher catalog.Fetcher) (map[*config.Endpoint][]catalog.Model, []DiscoveryReport) {
	targets := discoveryTargets(cfg)
	outcomes := make([]discoveryOutcome, len(targets))

	// 每个 goroutine 只写自己那一格：切片不同元素的并发写不需要锁，
	// 共享一个 map 则需要（Go 的 map 并发写即使键不同也会被运行时发现）。
	var wg sync.WaitGroup
	for i := range targets {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			started := time.Now()
			result, err := catalog.Discover(ctx, targets[index].spec, fetcher)
			outcomes[index] = discoveryOutcome{
				result:   result,
				err:      err,
				duration: time.Since(started),
			}
		}(i)
	}
	wg.Wait()

	discovered := make(map[*config.Endpoint][]catalog.Model, len(targets))
	reports := make([]DiscoveryReport, 0, len(targets))
	for i, target := range targets {
		outcome := outcomes[i]
		report := DiscoveryReport{
			Provider:      target.provider,
			Listing:       target.spec.ListingURL,
			Protocol:      target.spec.Protocol,
			Shape:         outcome.result.Shape,
			Found:         outcome.result.Found,
			Kept:          keptModels(outcome.result.Models),
			Filtered:      outcome.result.Filtered,
			Declared:      target.endpoint.Models,
			UnusedAliases: unusedAliases(outcome.result.UnusedAliases),
			HasMore:       outcome.result.HasMore,
			Excluded:      outcome.result.Excluded,
			Err:           outcome.err,
			Duration:      outcome.duration,
			endpoint:      target.endpoint,
		}
		if outcome.err == nil {
			discovered[target.endpoint] = outcome.result.Models
		}
		reports = append(reports, report)
	}
	return discovered, reports
}

// listingFetcher 按调用上游的同一套规则取清单：凭据与协议内置头由同一条路径产出。
//
// 不自己拼请求头：Anthropic 的清单接口同样要求 anthropic-version，另写一份必然漏掉
// 这类协议内置事实，而漏掉的表现是一个 400 —— 从状态码看不出是少了个头。
type listingFetcher struct {
	http        *http.Client
	credentials *credential.Provider
	adapters    map[domain.Protocol]domain.Adapter
}

// Listing 实现 catalog.Fetcher。
func (f listingFetcher) Listing(ctx context.Context, ep catalog.Endpoint) ([]byte, error) {
	headers, err := f.credentials.UpstreamHeaders(ctx, domain.Route{
		UpstreamID:    ep.Provider,
		Protocol:      ep.Protocol,
		CredentialRef: ep.CredentialRef,
	})
	if err != nil {
		return nil, fmt.Errorf("解析请求头失败：%w", err)
	}
	if provider, ok := f.adapters[ep.Protocol].(domain.UpstreamHeaderProvider); ok {
		headers = provider.UpstreamHeaders(headers)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep.ListingURL, nil)
	if err != nil {
		return nil, fmt.Errorf("构造清单请求失败：%w", err)
	}
	req.Header = headers
	// 清单接口是 GET，没有请求体；Content-Type 因此不设，Accept 说清要 JSON。
	req.Header.Set("Accept", "application/json")

	resp, err := f.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求清单失败：%w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, discoveryErrorDetailLimit))
		return nil, fmt.Errorf("上游 HTTP 状态码 %d：%s",
			resp.StatusCode, oneLineText(strings.TrimSpace(string(snippet))))
	}

	// 多读一个字节用于区分「恰好读满」与「超限」：只读上限个字节的话，超长响应会被
	// 静默截断，然后以「JSON 解析失败」的形式报出来，与真实原因（响应体异常大）无关。
	body, err := io.ReadAll(io.LimitReader(resp.Body, catalog.MaxListingBytes+1))
	if err != nil {
		return nil, fmt.Errorf("读取清单响应失败：%w", err)
	}
	return body, nil
}

// modelIDs 取出保留模型的 id，按清单顺序。
func modelIDs(models []catalog.Model) []string {
	ids := make([]string, len(models))
	for i, model := range models {
		ids[i] = model.ID
	}
	return ids
}

// keptModels 把清单里保留的模型转成「对外名 + 上游名」的成对形式。
//
// 转发与报告都用这一对：对外名是客户端请求的名字，上游名是发往上游的名字，
// 两者在 expose 改名之后不再相等。
func keptModels(models []catalog.Model) []config.Model {
	kept := make([]config.Model, len(models))
	for i, model := range models {
		kept[i] = config.Model{Name: model.Name, Upstream: model.ID}
	}
	return kept
}

// exposeAliases 把配置里的改名规则转成 catalog 的纯数据形式。
func exposeAliases(aliases []config.Alias) []catalog.Alias {
	converted := make([]catalog.Alias, len(aliases))
	for i, alias := range aliases {
		converted[i] = catalog.Alias{
			From: alias.From,
			To:   alias.To,
			File: alias.File,
			Line: alias.Line,
		}
	}
	return converted
}

// unusedAliases 把 catalog 报回的未命中规则转回配置层类型，供装配层用它带的位置记提醒。
func unusedAliases(aliases []catalog.Alias) []config.Alias {
	converted := make([]config.Alias, len(aliases))
	for i, alias := range aliases {
		converted[i] = config.Alias{
			From: alias.From,
			To:   alias.To,
			File: alias.File,
			Line: alias.Line,
		}
	}
	return converted
}

// catalogStats 是本次装配的目录事实，供启动横幅与 `nova models` 汇总。
//
// 它只包含计数，不含模型名单：名单可能几百条，把它塞进横幅会把这屏刷掉。
type catalogStats struct {
	// DiscoveryEndpoints 是声明了发现的端点数。
	DiscoveryEndpoints int
	// Discovered 是各清单里发现并去掉重复后的条目总数。
	Discovered int
	// Kept 是通过过滤保留的发现模型数。
	Kept int
	// Filtered 是被过滤规则排除的条目数。
	Filtered int
	// Degraded 是发现失败后退回显式模型的端点数。
	Degraded int
	// Models 是目录里的对外名总数（显式声明 + 发现，按端点合并去重后）。
	Models int
}

// catalogStatsFrom 汇总本次装配的目录事实。
func catalogStatsFrom(reports []DiscoveryReport, models []ModelEntry) catalogStats {
	stats := catalogStats{DiscoveryEndpoints: len(reports), Models: len(models)}
	for _, report := range reports {
		if report.Err != nil {
			if report.Degraded {
				stats.Degraded++
			}
			continue
		}
		stats.Discovered += report.Found
		stats.Kept += len(report.Kept)
		stats.Filtered += report.Filtered
	}
	return stats
}

// fatalDiscovery 把「发现失败且没有显式模型可退回」的端点汇总成装配失败原因。
//
// 两条处置规则的分界就在这里：有显式模型的端点已在上一步标记降级，装配继续；
// 没有显式模型的端点在发现失败后没有任何可路由的模型，等价于一条配置错误，
// 因此让它以明确失败收场，而不是起一个「看起来在跑、实际上没有模型」的实例。
func fatalDiscovery(reports []DiscoveryReport) error {
	var failures []string
	for _, report := range reports {
		if report.Err == nil || len(report.endpoint.Models) > 0 {
			continue
		}
		failures = append(failures, fmt.Sprintf("provider %s 的清单 %s：%v",
			report.Provider, report.Listing, report.Err))
	}
	if len(failures) == 0 {
		return nil
	}
	return fmt.Errorf("模型发现失败，且这些端点没有显式声明的模型可退回：\n  %s",
		strings.Join(failures, "\n  "))
}

// assembleEndpoints 把配置里的全部端点合成本次装配的生效端点集合。
//
// 顺序即配置顺序：同名模型之间的回退顺序来自它。没有声明发现的端点也在这里，
// 它的生效集合就是显式声明的那些模型——「目录」因此描述的是全部对外名，
// 而不是只有发现来的那一部分。
func assembleEndpoints(
	cfg *config.Config,
	discovered map[*config.Endpoint][]catalog.Model,
) []effectiveEndpoint {
	var endpoints []effectiveEndpoint
	for i := range cfg.Providers {
		provider := &cfg.Providers[i]
		for j := range provider.Endpoints {
			endpoint := &provider.Endpoints[j]
			endpoints = append(endpoints, effectiveEndpoint{
				provider: provider,
				endpoint: endpoint,
				models:   resolveEndpointModels(endpoint, discovered[endpoint]),
			})
		}
	}
	return endpoints
}
