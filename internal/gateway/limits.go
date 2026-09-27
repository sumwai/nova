package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/sumwai/nova/internal/config"
	"github.com/sumwai/nova/internal/domain"
	"github.com/sumwai/nova/internal/limits"
	"github.com/sumwai/nova/internal/pipeline"
	"github.com/sumwai/nova/internal/price"
)

// defaultLimitFailureThreshold 是设计第六节保守自适应规则的缺省 N：同一账号连续 N 次
// 同码失败且期间无成功才升级为窗口耗尽。它可经 AssembleOptions 覆盖，不是写死的常量。
const defaultLimitFailureThreshold = 5

// defaultLimitFailureWindow 是自适应规则里「连续」的时间上界：同一账号的失败计数
// 只在这个窗口内累加。
//
// 没有这个上界时，几小时内零星出现的同码失败会攒够 N 次并升级为窗口耗尽，而它们
// 并不是「连续」失败。窗口只约束累加，不限制升级后标记的存活时长（那由 until 与
// 保守上限决定）。
const defaultLimitFailureWindow = 10 * time.Minute

// limitRuntime 把逐账号的额度表接到转发路径上。
//
// 每个（渠道，账号）一张表：额度作用域是账号级事实，渠道内多个账号各有各的剩余量。
// 表只在账号声明了额度时创建；没有声明的账号不建表，一律视为可用——「没有限制」不等于
// 「限制为零」。
type limitRuntime struct {
	tables    map[string]map[string]*limits.Table // 渠道名 → 账号引用 → 表
	threshold int
	log       *logger
	// prices 是本次装配的价格表，用于把 token 用量折算成 usd；nil 表示不折算。
	prices *price.Table

	mu       sync.Mutex
	failures map[string]failureState // 账号标识 → 连续失败状态
	// now 取当前时刻，供失败计数窗口与不可用标记的 until 使用。
	// 注入让「连续」的时间上界能在测试里定死，而不依赖真实等待。
	now func() time.Time
	// warned 记录已输出过的观测拒绝提醒，键是账号与缺失键的组合。
	// 同一份档案会对每个请求产出同一组被拒绝的键；逐请求重复输出只会刷屏。
	warned map[string]bool
}

// failureState 记录一个账号上连续同码失败的码、次数与最近一次失败时刻。
type failureState struct {
	code      string
	count     int
	lastError time.Time
}

// newLimitRuntime 按配置里的渠道与账号建额度表，并把档案写入的声明合并进去。
//
// stateDir 为空时不落盘（表只活在进程里）。每张表写在自己的子目录下：
// limits.Table 的快照文件名固定为 limits.json，多张表共用一个目录会互相覆盖。
func newLimitRuntime(providers []config.Provider, stateDir string, threshold int, log *logger, prices *price.Table) (*limitRuntime, error) {
	if threshold <= 0 {
		threshold = defaultLimitFailureThreshold
	}
	runtime := &limitRuntime{
		tables:    make(map[string]map[string]*limits.Table),
		threshold: threshold,
		log:       log,
		prices:    prices,
		failures:  make(map[string]failureState),
		now:       time.Now,
		warned:    make(map[string]bool),
	}
	for i := range providers {
		provider := &providers[i]
		hasPool := provider.HasAccountPool()
		for j := range provider.Accounts {
			account := &provider.Accounts[j]
			if len(account.Limits) == 0 {
				continue
			}
			ref := ""
			if hasPool {
				ref = account.Ref()
			}
			scope := scopeName(provider.Name, ref)
			table := limits.New(limits.Options{
				StateDir: tableStateDir(stateDir, scope),
				Account:  scope,
			})
			if err := table.MergeDeclared(limits.DeclaredDoc(account.Limits)); err != nil {
				return nil, fmt.Errorf("渠道 %s 的账号 %s 声明额度失败：%w", provider.Name, scope, err)
			}
			runtime.put(provider.Name, ref, table)
		}
	}
	return runtime, nil
}

// put 登记一张额度表。
func (rt *limitRuntime) put(provider, ref string, table *limits.Table) {
	byRef, ok := rt.tables[provider]
	if !ok {
		byRef = make(map[string]*limits.Table)
		rt.tables[provider] = byRef
	}
	byRef[ref] = table
}

// table 返回某账号的额度表；没有声明额度的账号返回 nil。
func (rt *limitRuntime) table(provider, ref string) *limits.Table {
	byRef := rt.tables[provider]
	if byRef == nil {
		return nil
	}
	return byRef[ref]
}

// scopeName 是额度作用域里的账号标识：渠道名 + 账号引用，单账号渠道不带引用。
//
// 它与 domain.Route 的 Provider/AccountRef 一一对应，因此运行期查表时不必再拼一次。
func scopeName(provider, ref string) string {
	if ref == "" {
		return provider
	}
	return provider + " " + ref
}

// tableStateDir 返回某账号额度表的落盘子目录。
//
// 作用域名里有渠道名与账号名，两者都是配置里书写的字符串，可能含 `/` 或 `..`。
// 直接拼进路径会把快照写到状态目录之外，因此先按文件名字符集过滤，再在名字确实
// 被改动时补一段由原名派生的短哈希：截断与替换都可能让两个不同的作用域撞进同一个
// 目录，而额度快照按目录区分账号，撞在一起等于把一个账号的用量算到另一个头上。
// 这与 internal/profile 对订阅源快照命名空间的处理是同一个理由。
func tableStateDir(stateDir, scope string) string {
	if stateDir == "" {
		return ""
	}
	return filepath.Join(stateDir, "limits", safeStateComponent(scope))
}

// safeStateComponent 把一个可能含路径分隔符的名字变成一个安全的目录名。
//
// 只保留字母、数字、点、下划线与连字符；其余字符一律换成 `-`。名字未被改动时
// 直接用原名（可读），被改动时补上原名的短哈希（不撞）。
func safeStateComponent(name string) string {
	sanitized := make([]rune, 0, len(name))
	changed := false
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			sanitized = append(sanitized, r)
		case r == '.' || r == '_' || r == '-':
			sanitized = append(sanitized, r)
		default:
			sanitized = append(sanitized, '-')
			changed = true
		}
	}
	if !changed && name != "" && name != "." && name != ".." {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	return string(sanitized) + "-" + hex.EncodeToString(sum[:8])
}

// scopeOf 返回本次路由对应的额度作用域。
func (rt *limitRuntime) scopeOf(route domain.Route) limits.Scope {
	return limits.Scope{Account: scopeName(route.Provider, route.AccountRef)}
}

// availability 是一次额度判定的结果：可用性与判定原因。
//
// 原因不是可丢弃的副产品：快照读失败、记账断档、absolute 触顶需显式清除这些事实
// 只在这里给出，消费方据此记日志或写进对外错误。
type availability struct {
	ok     bool
	reason limits.Reason
}

// availability 报告某账号此刻是否可用，并给出判定原因；没有额度表时恒为可用。
func (rt *limitRuntime) availability(provider, ref, model string) availability {
	table := rt.table(provider, ref)
	if table == nil {
		return availability{ok: true, reason: limits.Reason{Verdict: limits.VerdictOK}}
	}
	ok, reason := table.Available(limits.Scope{Account: scopeName(provider, ref)}, model, rt.now())
	return availability{ok: ok, reason: reason}
}

// available 报告某账号此刻是否可用；没有额度表时恒为可用。
func (rt *limitRuntime) available(provider, ref, model string) bool {
	return rt.availability(provider, ref, model).ok
}

// degradedWarnings 列出全部处于估算态的额度表，供装配期记一条运行期提醒。
//
// 估算态意味着剩余量依赖本地累计或快照不可信，而这一点不会自己浮到日志里：
// Unavailable 的原因只在请求期可读，装配完成后没有第二个出口。
func (rt *limitRuntime) degradedWarnings() []string {
	var out []string
	for provider, byRef := range rt.tables {
		for ref, table := range byRef {
			if degraded, reason := table.Degraded(); degraded {
				out = append(out, fmt.Sprintf("渠道 %s 的账号 %s：%s", provider, scopeName(provider, ref), reason))
			}
		}
	}
	sort.Strings(out)
	return out
}

// warnOnce 输出一条每个唯一键只出现一次的额度提醒。
func (rt *limitRuntime) warnOnce(key, message string, err error) {
	if rt.log == nil {
		return
	}
	rt.mu.Lock()
	if rt.warned[key] {
		rt.mu.Unlock()
		return
	}
	rt.warned[key] = true
	rt.mu.Unlock()
	rt.log.failure(message, err)
}

// Reserve 实现 pipeline.LimitRuntime：原子判定并预留一次尝试。
//
// 没有额度表的账号直接放行（返回 nil settle）：不得因为缺少表而剔除。
func (rt *limitRuntime) Reserve(route domain.Route, model string, est pipeline.CostEstimate) (func(domain.Usage, error), bool) {
	table := rt.table(route.Provider, route.AccountRef)
	if table == nil {
		return nil, true
	}
	scope := rt.scopeOf(route)
	release, ok := table.Reserve(scope, model, rt.estimate(route, est), rt.now())
	if !ok {
		return nil, false
	}
	return func(actual domain.Usage, err error) {
		if err == nil {
			table.Consume(scope, model, rt.usageForLimits(route, actual), rt.now())
		}
		release()
	}, true
}

// Observe 实现 pipeline.LimitRuntime：合并响应头观测，成功时降级并清除标记。
func (rt *limitRuntime) Observe(route domain.Route, model string, header http.Header, succeeded bool) {
	if succeeded {
		rt.resetFailures(scopeName(route.Provider, route.AccountRef))
	}
	table := rt.table(route.Provider, route.AccountRef)
	if table == nil {
		return
	}
	scope := rt.scopeOf(route)
	if succeeded {
		// 上游成功返回即静态声明判为耗尽的证据被推翻：清除学习到的不可用标记。
		table.Clear(scope, model)
	}
	if header == nil {
		return
	}
	doc, err := limits.ParseHeaders(header, rt.now())
	if doc == nil {
		return
	}
	// 解析告警不阻断：少数头部写坏时仍合并能用的那部分观测，但要在日志里留一句。
	var warn *limits.Warn
	if errors.As(err, &warn) {
		rt.warnOnce("headers/"+scope.Account, "额度响应头有值无法解析", warn)
	}
	// 观测不得新建条目：响应头里出现了声明中没有的 metric/window 时，MergeObserved
	// 会拒绝该键并保留其余。被拒绝的键必须在日志里可见，否则「响应头观测已生效」
	// 就是一句空话——声明里没有对应键时，运行期不会有任何迹象。
	if err := table.MergeObserved(doc); err != nil {
		rt.warnOnce("merge/"+scope.Account+"/"+err.Error(), "额度响应头观测未合并", err)
	}
}

// LearnExhausted 实现 pipeline.LimitRuntime：按保守自适应规则写入不可用标记。
//
// 只有同一账号连续 threshold 次同码失败且期间无成功才升级；未达阈值时不写账号状态，
// 本次请求按普通可重试失败处理。
func (rt *limitRuntime) LearnExhausted(route domain.Route, model string, err error) {
	table := rt.table(route.Provider, route.AccountRef)
	if table == nil {
		// 没有声明额度的账号不建表，也不累计失败：没有可写的条目，计数没有消费者。
		return
	}
	account := scopeName(route.Provider, route.AccountRef)
	if !rt.recordFailure(account, err) {
		return
	}
	// until 取「保守上限」与「上游 Retry-After」的较小值：直接采信上游头会让一条
	// 写坏的（或恶意的）Retry-After 把渠道停用任意长，而任何标记都不得无期限。
	until := rt.now().Add(table.DefaultBlockCap())
	if delay, ok := retryAfter(err); ok && delay > 0 {
		if hinted := rt.now().Add(delay); hinted.Before(until) {
			until = hinted
		}
	}
	table.LearnExhausted(rt.scopeOf(route), model, until)
}

// recordFailure 累计一次同码失败，返回是否已达到升级阈值。
//
// 计数只在 defaultLimitFailureWindow 内累加：超过窗口的旧失败不再算「连续」。
// 达到阈值时计数被清掉——标记已经写下，下一次要重新攒够 threshold 次连续失败，
// 否则计数会无上界增长，且窗口一过就会用旧计数立即重写标记。
func (rt *limitRuntime) recordFailure(account string, err error) bool {
	code := ""
	if domainErr := domain.AsError(err); domainErr != nil {
		code = string(domainErr.Code)
	}
	now := rt.now()

	rt.mu.Lock()
	defer rt.mu.Unlock()
	state := rt.failures[account]
	if state.code != code || now.Sub(state.lastError) > defaultLimitFailureWindow {
		state = failureState{code: code, count: 1, lastError: now}
	} else {
		state.count++
		state.lastError = now
	}
	if state.count >= rt.threshold {
		delete(rt.failures, account)
		return true
	}
	rt.failures[account] = state
	return false
}

// resetFailures 清掉一个账号的连续失败状态。
func (rt *limitRuntime) resetFailures(account string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	delete(rt.failures, account)
}

// Close 落盘并关闭全部额度表。
func (rt *limitRuntime) Close() error {
	var errs []error
	for _, byRef := range rt.tables {
		for _, table := range byRef {
			if err := table.Close(); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// retryAfter 从错误里读取上游给出的退避提示；错误不带该能力时返回 false。
func retryAfter(err error) (time.Duration, bool) {
	var carrier interface {
		RetryAfter() (time.Duration, bool)
	}
	if errors.As(err, &carrier) {
		return carrier.RetryAfter()
	}
	return 0, false
}

// estimate 把流水线给出的成本上界折算成额度层的逐 metric 估量。
//
// requests 与 tokens 由流水线给出（requests 恒 1，tokens 取有效输出上限）；usd 由
// 价格层按 tokens 折算。价格未知（Assumed）时不折算：名义价只用于排序，不能当成
// 真实计费。credits 没有价格来源，保持 0。
func (rt *limitRuntime) estimate(route domain.Route, est pipeline.CostEstimate) limits.Usage {
	out := limits.Usage{
		Requests: est.Requests,
		Tokens:   est.Tokens,
		Credits:  est.Credits,
		USD:      est.USD,
	}
	if out.USD == 0 {
		out.USD = rt.convertUSD(route, price.Usage{Output: int(est.Tokens)})
	}
	return out
}

// usageForLimits 把一次成功尝试的用量折算成额度层的累计口径。
//
// 额度层不 import internal/domain，折算只在装配层做。tokens 取输入与输出之和，
// requests 一次成功尝试记 1；usd 由价格层按实际输入输出折算，价格未知时为 0。
// credits 没有价格来源，保持 0——声明了 credits 额度的账号靠上游观测而非本地累计。
func (rt *limitRuntime) usageForLimits(route domain.Route, usage domain.Usage) limits.Usage {
	return limits.Usage{
		Tokens:   float64(usage.Total()),
		Requests: 1,
		USD: rt.convertUSD(route, price.Usage{
			Input:  usage.InputTokens,
			Output: usage.OutputTokens,
		}),
	}
}

// convertUSD 按本次路由的价格把一次用量折算成美元；价格未知时返回 0。
func (rt *limitRuntime) convertUSD(route domain.Route, usage price.Usage) float64 {
	if rt.prices == nil || route.PriceKey == "" {
		return 0
	}
	entry := rt.prices.Lookup(route.PriceKey)
	if entry.Assumed {
		return 0
	}
	return price.Estimate(entry, usage)
}
