// Package catalog 从上游清单接口取得一条端点可用的模型集合。
//
// 职责边界：
//   - 只做「取清单、判形状、抽字段、按规则过滤」这四件事；
//   - 不认识 internal/config，也不做选路：端点描述由装配层拆成纯数据传进来，
//     过滤后的模型集合由装配层与显式声明的模型合并、再构造选路表；
//   - 网络访问经 Fetcher 注入：凭据与协议请求头的构造属装配层的事实，本包不碰。
package catalog

import (
	"context"
	"fmt"
	"time"

	"github.com/sumwai/nova/internal/domain"
)

// MaxListingBytes 是清单响应体的字节上限。
//
// 越限按发现失败处理，而不是截断后继续解析：被截断的 JSON 会以解析错误收场，
// 报出来的原因与真实原因（响应体异常大）无关，排查会从错误的方向开始。
const MaxListingBytes = 4 << 20

// defaultTimeout 是取清单的超时上限。
//
// 发现发生在装配路径上，串行等待多个端点会把启动时间拖长；端点自身的 timeout 更短时
// 取端点那个值，因为端点超时表达的是「这条上游允许多慢」。
const defaultTimeout = 10 * time.Second

// Endpoint 是一条端点里与发现有关的事实。
//
// 它是纯数据，由装配层从配置拆出来：本包因此不依赖 internal/config，
// 也不必知道 provider 块的语法。
type Endpoint struct {
	// Provider 是端点所属渠道的名字，只用于错误消息与日志定位。
	Provider string

	// ListingURL 是清单接口的完整地址。
	ListingURL string

	// Protocol 是这条端点的线协议，决定清单请求要不要带协议内置头。
	Protocol domain.Protocol

	// CredentialRef 是凭据引用（即 provider 名），由 Fetcher 用来取密钥。
	CredentialRef string

	// Timeout 是端点自身的超时；发现超时取它与 defaultTimeout 中较小的那个。
	Timeout time.Duration

	// Allow 与 Deny 是过滤模式。两者都为空表示清单里的模型全部保留。
	Allow []string
	Deny  []string

	// Expose 是改名规则，按声明顺序；第一条命中即生效。
	// 空表示对外名即上游 id。
	Expose []Alias
}

// where 把一条端点描述成人读的定位文本，用于错误消息。
func (e Endpoint) where() string {
	return fmt.Sprintf("provider %s 的清单 %s", e.Provider, e.ListingURL)
}

// Shape 是上游清单响应的字段风格。
//
// 它只影响统计口径与分页字段怎么读：字段提取一律宽容（一个条目里能读到的都读），
// 因此形状判错不会丢字段，最多让日志里的风格标注与实际不符。
type Shape string

const (
	ShapeOpenAI    Shape = "openai"
	ShapeAnthropic Shape = "anthropic"
	ShapeUnknown   Shape = "unknown"
)

// Model 是清单里的一条模型，字段已按两种上游形状归一。
type Model struct {
	// ID 是上游模型 id：既是过滤的匹配对象，也是经 expose 改写后发往上游的名字。
	ID string

	// Name 是暴露给客户端的对外名。没有被任何 expose 规则命中时等于 ID。
	Name string

	// DisplayName 是上游给的展示名；OpenAI 形状的清单没有这个字段。
	DisplayName string

	// OwnedBy 是上游给的归属；Anthropic 形状的清单没有这个字段。
	OwnedBy string

	// CreatedAt 是上游给的创建时间；零值表示上游没给。
	CreatedAt time.Time
}

// Result 是一条端点的发现结果。
type Result struct {
	// Models 是通过过滤的模型，按清单顺序。
	Models []Model

	// Shape 是判出的清单风格。
	Shape Shape

	// Found 是清单里去重后的条目数，即过滤之前的候选集合大小。
	Found int

	// Filtered 是被 allow / deny 排除的条目数。
	Filtered int

	// HasMore 报告清单自述还有下一页。本版不翻页，只把它记成一条提醒。
	HasMore bool

	// Excluded 是被过滤规则排除的模型 id（上游 id），按清单顺序。
	// `nova models` 用它列出被拦下的模型；日志只记计数。
	Excluded []string

	// UnusedAliases 是没有命中任何清单项的改名规则，供装配层记一条提醒。
	// 它多半意味着模式写错了，或者规则针对的项被 allow / deny 先拦掉了。
	UnusedAliases []Alias
}

// Fetcher 取回一份清单响应体。
//
// 网络访问、凭据注入、超时与响应体上限都由实现负责；本包只消费字节。
// 返回的字节超过 MaxListingBytes 时本包同样会拒绝，让注入的实现漏了上限时不会静默通过。
type Fetcher interface {
	Listing(ctx context.Context, ep Endpoint) ([]byte, error)
}

// Discover 取一条端点的清单，判形状、去重、按规则过滤，返回保留的模型集合。
//
// 任何一步失败都返回错误：不做部分解析，也不在失败时返回半个集合。
// 「有静态模型就降级、没有就装配失败」的处置属装配层的事实，本包只回答这一次发现成不成功。
func Discover(ctx context.Context, ep Endpoint, fetcher Fetcher) (Result, error) {
	switch {
	case ep.ListingURL == "":
		return Result{}, fmt.Errorf("%s 没有清单地址", ep.where())
	case fetcher == nil:
		return Result{}, fmt.Errorf("%s 缺少取清单的实现", ep.where())
	}

	timeout := requestTimeout(ep.Timeout)
	listingCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	body, err := fetcher.Listing(listingCtx, ep)
	if err != nil {
		return Result{}, fmt.Errorf("%s 请求失败：%w", ep.where(), err)
	}
	if len(body) > MaxListingBytes {
		return Result{}, fmt.Errorf("%s 的响应体超过 %d 字节", ep.where(), MaxListingBytes)
	}

	entries, shape, hasMore, err := parseListing(body)
	if err != nil {
		return Result{}, fmt.Errorf("%s 的响应无法解析：%w", ep.where(), err)
	}

	result := Result{Shape: shape, HasMore: hasMore}
	seenID := make(map[string]bool, len(entries))
	// seenName 记录对外名到上游 id 的归属，用来拦住「两条清单项改名后撞同一个对外名」：
	// 那种情况下谁该先生效在配置里没有表达，静默取其一会让另一条看起来生效了。
	seenName := make(map[string]string, len(entries))
	usedRules := make([]bool, len(ep.Expose))
	result.Models = make([]Model, 0, len(entries))
	for _, model := range entries {
		// 同一份清单里的重复 id 保留首个：重复本身不携带新事实，
		// 而保留第二个会让「哪个生效」取决于上游的返回顺序。去重按上游 id，不按对外名。
		if seenID[model.ID] {
			continue
		}
		seenID[model.ID] = true
		result.Found++
		if !keep(ep, model.ID) {
			result.Filtered++
			result.Excluded = append(result.Excluded, model.ID)
			continue
		}

		// 过滤先于改名：allow / deny 描述的是「上游有哪些东西」，
		// 暴露叫什么名字是另一件事。
		name, rule := applyAliases(ep.Expose, model.ID)
		if rule >= 0 {
			usedRules[rule] = true
		}
		if other, exists := seenName[name]; exists {
			return Result{}, fmt.Errorf("%s 里 %q 与 %q 改名后都是 %q；同一个对外名只能对应一条候选",
				ep.where(), other, model.ID, name)
		}
		seenName[name] = model.ID
		model.Name = name
		result.Models = append(result.Models, model)
	}
	for i, alias := range ep.Expose {
		if !usedRules[i] {
			result.UnusedAliases = append(result.UnusedAliases, alias)
		}
	}
	return result, nil
}

// requestTimeout 取本次发现的超时：不超过 defaultTimeout，不超过端点自身的 timeout。
func requestTimeout(endpointTimeout time.Duration) time.Duration {
	if endpointTimeout <= 0 || endpointTimeout > defaultTimeout {
		return defaultTimeout
	}
	return endpointTimeout
}

// keep 判定一个模型 id 是否通过过滤规则。
//
// allow 为空视为全通过，其余只保留命中任一 allow 模式的项；随后排除命中任一 deny 模式的项，
// 即 deny 优先于 allow。匹配不区分大小写，但保留与暴露的 id 一律是上游原样。
func keep(ep Endpoint, id string) bool {
	if len(ep.Allow) > 0 && !matchesAny(ep.Allow, id) {
		return false
	}
	return !matchesAny(ep.Deny, id)
}

// matchesAny 报告 id 是否命中集合里的任一模式。
func matchesAny(patterns []string, id string) bool {
	for _, pattern := range patterns {
		if Match(pattern, id) {
			return true
		}
	}
	return false
}
