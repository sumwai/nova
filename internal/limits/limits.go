// Package limits 合并额度声明与观测，求值账号可用性，并做本地记账与预留。
//
// 本包只依赖 internal/profile 与标准库：额度文档的形状由 profile 校验，本包不解析
// YAML、不认配置、不碰网关。它回答两个问题——「此刻该账号/模型能不能用」与
// 「这次请求的预估成本能不能先占住」，并保证后者在同一张表上并发调用不超卖。
//
// 合并规则按设计文档 §5.4 逐条落地：按键合并、声明给常量、观测给状态、作用域相与、
// 同作用域取更保守值、恶化立即生效而改善需一次确认、观测带新鲜度上限、单位不同不合并。
package limits

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/sumwai/nova/internal/profile"
)

// Scope 是一条额度条目的作用域。
//
// Account 与 Pool 定位账号，Model 为空表示账号级（跨模型共享）；非空表示模型级
// （只影响所列模型）。账号级与模型级同时存在时相与生效：任一触顶即不可用。
type Scope struct {
	Account string
	Pool    string
	Model   string
}

// Key 是合并的键，对应设计文档 §5.4 的 (账号, 池, 模型, metric, window, kind)。
//
// 键里带 metric 与 window 意味着单位不同天然是不同条目，不存在把 tokens 与 requests
// 混算的路径；键里带 model 意味着模型级与账号级是两组独立条目，求值时相与。
type Key struct {
	Scope
	Metric string
	Window string
	Kind   string
}

// Usage 是一次请求在各 metric 上的实际用量。
//
// 本包不 import internal/domain，避免额度层被用量类型的演进牵动；网关侧按 metric
// 把 domain.Usage 折算后填入本类型。未给出的 metric 保持零值，不会被当成用量。
type Usage struct {
	Tokens   float64
	Requests float64
	USD      float64
	Credits  float64
}

// amount 返回某个 metric 对应的用量；未识别的 metric 返回 0。
func (u Usage) amount(metric string) float64 {
	switch metric {
	case "tokens":
		return u.Tokens
	case "requests":
		return u.Requests
	case "usd":
		return u.USD
	case "credits":
		return u.Credits
	default:
		return 0
	}
}

// Options 是构造 Table 的外部依赖。
type Options struct {
	// StateDir 是状态目录，快照写在 <StateDir>/limits.json；空表示不落盘。
	StateDir string

	// Account 是这些额度文档所属的账号。额度文档本身不带账号名，
	// 账号由调用方在装配时给出；Scope.Account 为空时回落到这里。
	Account string

	// Clock 取当前时刻。注入让窗口边界、TTL 与退避都能在测试里定死。
	// 零值用 time.Now；用 time.Now 取得的时刻自带单调读数，本地推算因此走单调时钟。
	Clock func() time.Time

	// TTL 是探测间隔，新鲜度上限取 TTLFactor 倍它；零值用 60s。
	TTL time.Duration

	// TTLFactor 是新鲜度上限的系数，零值用 3。
	TTLFactor float64

	// ConservativeFactor 是估算态下对剩余量施加的保守系数；零值用 0.5。
	ConservativeFactor float64

	// BlockCap 是 quota 类条目在 resets_at 缺席时的不可用上限；零值用 1h。
	BlockCap time.Duration

	// RateBackoff 是 rate 类条目触顶且 resets_at 缺席时的固定短退避；零值用 45s。
	RateBackoff time.Duration

	// FlushInterval 是快照写盘的节流间隔；零值用 5s，负数表示每次变更都写。
	FlushInterval time.Duration

	// GapThreshold 是快照与本机时刻的最大可接受差；超过即认为记账有断档；零值用 1h。
	GapThreshold time.Duration
}

// Table 是一个账号的额度表。
//
// 所有对外方法都在同一把锁内完成读与写：账号选择与额度预留因此是不可分割的一步，
// 并发调用不会出现「各自看到还有余量、各自占住」的超卖。
type Table struct {
	mu      sync.Mutex
	opts    Options
	now     func() time.Time
	entries map[Key]*entry
	blocks  map[blockKey]block

	// degraded 表示本表的状态不完整（快照载入失败、记账有断档、写盘失败）。
	// 它让 Available 给出估算态，而不是一个看似准确的剩余值。
	degraded       bool
	degradedReason string

	// pending 是快照里载入、尚未与声明对齐的状态；键在声明出现时被应用。
	pending map[Key]snapshotEntry

	dirty     bool
	lastFlush time.Time
	// writeMu 串行化磁盘写入。快照在持有它时从当前状态重建，因此最后一个写者
	// 写入的一定是最新状态，不会出现旧快照覆盖新快照。锁序只有 writeMu → mu 一种。
	writeMu sync.Mutex
	// writers 统计在途的后台写盘，Close 等它全部结束后再写最终快照。
	writers sync.WaitGroup
	// flushing 表示已有一个后台写盘在途，用于避免请求路径按每次变更堆叠 goroutine。
	flushing bool
	// closed 在 Close 之后置位，之后的变更不再落盘。换入新装配后旧表仍可能被在途请求
	// 持有（settle 时调 Consume），不挡住它会把旧状态写回同一份快照。
	closed bool
}

// New 构造一张额度表，并在 StateDir 非空时载入上一进程的快照。
//
// 载入失败不让构造失败：一份读不出来的快照只说明状态不可信，不代表账号不可用。
// 表会被标记为估算态，Available 的 Reason 里体现，偏保守而不静默乐观。
func New(opts Options) *Table {
	opts = withDefaults(opts)
	table := &Table{
		opts:    opts,
		now:     opts.Clock,
		entries: map[Key]*entry{},
		blocks:  map[blockKey]block{},
		pending: map[Key]snapshotEntry{},
	}
	table.load()
	return table
}

// withDefaults 补齐选项缺省值。缺省偏保守：估算系数取半，退避 45s，不可用上限 1h。
func withDefaults(opts Options) Options {
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	if opts.TTL <= 0 {
		opts.TTL = 60 * time.Second
	}
	if opts.TTLFactor <= 0 {
		opts.TTLFactor = 3
	}
	if opts.ConservativeFactor <= 0 {
		opts.ConservativeFactor = 0.5
	}
	if opts.BlockCap <= 0 {
		opts.BlockCap = time.Hour
	}
	if opts.RateBackoff <= 0 {
		opts.RateBackoff = 45 * time.Second
	}
	if opts.FlushInterval == 0 {
		opts.FlushInterval = 5 * time.Second
	}
	if opts.GapThreshold <= 0 {
		opts.GapThreshold = time.Hour
	}
	return opts
}

// normalizeScope 在 Scope.Account 为空时填入装配时给出的账号。
func (t *Table) normalizeScope(scope Scope) Scope {
	if scope.Account == "" {
		scope.Account = t.opts.Account
	}
	return scope
}

// effectiveModel 取本次请求实际匹配的模型：入参优先，其次作用域自带的模型。
func effectiveModel(scope Scope, model string) string {
	if model != "" {
		return model
	}
	return scope.Model
}

// MergeDeclared 合并一份声明文档：只写常量（上限、窗口、metric、作用域）。
//
// 声明是常量来源，观测是状态来源。因此这里不覆盖已有观测得到的状态，只补常量；
// 声明自带的 limit/used/remaining 在没有观测时作为初始基准，并标记为假定值，
// 直到一次真实观测把它替换掉——静态声明因此可以自愈。
func (t *Table) MergeDeclared(doc *profile.LimitsDoc) error {
	if doc == nil {
		return fmt.Errorf("额度文档为空")
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.now()
	for _, limit := range doc.Account {
		if err := t.declare(Scope{Account: t.opts.Account, Pool: limit.Pool}, limit, doc, now); err != nil {
			return err
		}
	}
	modelNames := make([]string, 0, len(doc.Models))
	for name := range doc.Models {
		modelNames = append(modelNames, name)
	}
	sort.Strings(modelNames)
	for _, name := range modelNames {
		for _, limit := range doc.Models[name] {
			scope := Scope{Account: t.opts.Account, Pool: limit.Pool, Model: name}
			if err := t.declare(scope, limit, doc, now); err != nil {
				return err
			}
		}
	}

	// 声明到位后，把快照里对应的状态接到条目上；没有对应声明的状态被丢弃，
	// 因为缺少窗口与上限的剩余量无法参与求值，留着只会给出一个看似准确的数值。
	t.applyPending()
	t.dirty = true
	t.maybeFlush(now)
	return nil
}

// declare 合并单条声明。
func (t *Table) declare(scope Scope, limit profile.Limit, doc *profile.LimitsDoc, now time.Time) error {
	spec, err := parseWindow(limit.Window, limit.TZ, limit.Anchor)
	if err != nil {
		return fmt.Errorf("窗口 %q 无法解析：%w", limit.Window, err)
	}
	key := Key{
		Scope:  scope,
		Metric: limit.Metric,
		Window: limit.Window,
		Kind:   limit.Kind,
	}
	item, ok := t.entries[key]
	if !ok {
		item = &entry{key: key}
		t.entries[key] = item
	}

	item.spec = spec
	item.unbounded = limit.Unbounded
	item.limit = limit.Limit
	item.declRemaining = limit.Remaining
	item.declUsed = limit.Used
	if doc.ExpiresAt != "" {
		if parsed, err := time.Parse(time.RFC3339, doc.ExpiresAt); err == nil {
			item.expiresAt = parsed
		}
	}

	if item.windowStart.IsZero() && spec.kind != windowAbsolute {
		item.windowStart = spec.start(now)
	}
	// 已有观测状态时不动它：观测是可覆盖声明的实测值。
	if !item.hasObserved && item.baseline == nil {
		item.baseline = item.declaredBaseline()
		item.assumed = item.baseline != nil
	}
	// 上游成功会清掉学习到的不可用标记（静态声明自愈）；这里在声明里不做，
	// 因为声明不是实测，不能用来证明账号恢复了。
	return nil
}

// MergeObserved 合并一份观测文档：只写状态（remaining/used/resets_at）。
//
// 观测不得新建条目、不得删除条目、不得修改上限/窗口/metric/作用域。唯一的例外是
// 错误学习，且必须落在已存在的条目上。因此本方法对没有对应声明的键只报错、不建条目；
// 已声明的键照常合并，缺声明的键在返回的错误里逐条列出。
func (t *Table) MergeObserved(doc *profile.LimitsDoc) error {
	if doc == nil {
		return fmt.Errorf("额度文档为空")
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.now()
	var missing []Key
	observedAt, hasObservedAt := parseObservedAt(doc.ObservedAt)
	stale := !hasObservedAt

	merge := func(scope Scope, limit profile.Limit) {
		key := Key{Scope: scope, Metric: limit.Metric, Window: limit.Window, Kind: limit.Kind}
		item, ok := t.entries[key]
		if !ok {
			missing = append(missing, key)
			return
		}
		t.observe(item, limit, observedAt, stale, now)
	}

	for _, limit := range doc.Account {
		merge(Scope{Account: t.opts.Account, Pool: limit.Pool}, limit)
	}
	modelNames := make([]string, 0, len(doc.Models))
	for name := range doc.Models {
		modelNames = append(modelNames, name)
	}
	sort.Strings(modelNames)
	for _, name := range modelNames {
		for _, limit := range doc.Models[name] {
			merge(Scope{Account: t.opts.Account, Pool: limit.Pool, Model: name}, limit)
		}
	}

	t.dirty = true
	t.maybeFlush(now)
	if len(missing) > 0 {
		return fmt.Errorf("观测不得新建条目，以下键没有对应声明：%s", joinKeys(missing))
	}
	return nil
}

// observe 把一条观测并入条目。
//
// 新鲜度按 min(TTLFactor × TTL, 窗口长度的 10%) 判定；absolute 更严格，取 TTLFactor × TTL。
// 超限的观测不更新基准，只把条目标记为降级（本地累计 + 保守系数），并在 Reason 里体现。
func (t *Table) observe(item *entry, limit profile.Limit, observedAt time.Time, stale bool, now time.Time) {
	if !stale {
		window := item.freshnessLimit(t.opts)
		if now.Sub(observedAt) > window || observedAt.Sub(now) > window {
			stale = true
		}
	}
	if stale {
		item.stale = true
		return
	}

	// 上游权威观测到达即清除学习到的不可用标记：这是静态声明与错误学习的自愈入口。
	// 只有剩余量大于零才算「上游成功」，remaining=0 的观测不该清掉标记。

	if item.unbounded {
		t.clearBlockLocked(item.key.Scope, item.key.Model)
		item.assumed = false
		item.stale = false
		item.hasObserved = true
		item.observedAt = observedAt
		return
	}

	value, ok := observedRemaining(limit)
	if !ok {
		// schema 已保证观测能确定 remaining；走到这里说明类型被绕过，保守地不采信。
		item.stale = true
		return
	}
	if value > 0 {
		t.clearBlockLocked(item.key.Scope, item.key.Model)
	}

	item.applyObservation(value)

	if limit.ResetsAt != "" {
		if parsed, err := time.Parse(time.RFC3339, limit.ResetsAt); err == nil {
			item.resetsAt = parsed
			if item.spec.kind == windowDuration && !parsed.IsZero() {
				item.windowStart = parsed.Add(-item.spec.length)
			}
		}
	}
	switch item.spec.kind {
	case windowNatural:
		item.windowStart = item.spec.start(now)
	case windowDuration:
		if item.windowStart.IsZero() {
			item.windowStart = now
		}
	}

	item.observedAt = observedAt
	item.hasObserved = true
	item.assumed = false
	item.stale = false
}

// observedRemaining 从一条观测里算出剩余量；观测必须能确定 remaining。
func observedRemaining(limit profile.Limit) (float64, bool) {
	switch {
	case limit.Remaining != nil:
		return *limit.Remaining, true
	case limit.Limit != nil && limit.Used != nil:
		return *limit.Limit - *limit.Used, true
	default:
		return 0, false
	}
}

// LearnExhausted 记录「该账号在该模型上不可用，至 until」。
//
// 任何不可用标记必须带 until，禁止无期限禁用；until 不是未来时刻或没有任何已声明的
// 条目时不产生标记。错误学习是观测不得新建条目的唯一例外，但仍必须落在已存在的
// 条目上：一个从未声明的账号/模型组合不是这里该创造的。
func (t *Table) LearnExhausted(scope Scope, model string, until time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.now()
	if !until.After(now) {
		return
	}
	scope = t.normalizeScope(scope)
	model = effectiveModel(scope, model)
	if !t.hasEntriesLocked(scope, model) {
		return
	}
	key := blockKey{Account: scope.Account, Pool: scope.Pool, Model: model}
	existing, ok := t.blocks[key]
	if ok && !until.After(existing.until) {
		return
	}
	t.blocks[key] = block{until: until, verdict: VerdictQuotaExhausted}
	t.dirty = true
	t.maybeFlush(now)
}

// Clear 显式清除某个作用域上学习到的不可用标记。
//
// absolute 类条目触顶没有 until，只能靠显式清除或一次真实观测覆盖；这个入口给脚本与
// 人工用。它不清除 remaining <= 0 本身：那是上游事实，只能由新的观测改写。
func (t *Table) Clear(scope Scope, model string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	scope = t.normalizeScope(scope)
	model = effectiveModel(scope, model)
	t.clearBlockLocked(scope, model)
	t.dirty = true
	t.maybeFlush(t.now())
}

// Consume 记一次成功请求的用量，进入本地累计。
//
// 累计只在本地生效：它不修改声明，也不取代观测，只用于把「距上次权威观测以来的用量」
// 从剩余量里扣掉。窗口切换后本地累计清零。
func (t *Table) Consume(scope Scope, model string, usage Usage, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()

	scope = t.normalizeScope(scope)
	model = effectiveModel(scope, model)
	for _, item := range t.matchLocked(scope, model) {
		item.rollover(now)
		item.localUsed += usage.amount(item.key.Metric)
	}
	t.dirty = true
	t.maybeFlush(now)
}

// Reserve 原子地判定可用并预留估量。
//
// 判定与预留必须在同一把锁内完成：先 Available 再单独占位会在两次调用之间被并发
// 请求钻空子，造成超卖。预留成功后返回的 release 必须是幂等的，成功提交与失败回滚
// 都调用它一次即可。
//
// est 按 metric 分量给出：每条条目只扣减自己 metric 上的分量。一个分量为 0 表示
// 该分量没有已知成本上界，此时仍要求剩余严格大于已预留量（挡住 remaining==0），
// 但挡不住「剩余仍为正、并发请求一起挤进来」的弱超卖。分量与条目的单位对应由
// Usage.amount 单一实现：requests 与 tokens 不互相折算。
func (t *Table) Reserve(scope Scope, model string, est Usage, now time.Time) (func(), bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	scope = t.normalizeScope(scope)
	model = effectiveModel(scope, model)
	matched := t.matchLocked(scope, model)

	// 先整体判定，再统一预留：任一条目容不下就整笔拒绝，不留下半张预留。
	for _, item := range matched {
		item.rollover(now)
		if !item.expiresAt.IsZero() && !now.Before(item.expiresAt) {
			return func() {}, false
		}
		if _, blocked := t.blockedLocked(scope, model, now); blocked {
			return func() {}, false
		}
		remaining, known, conservative := item.remaining()
		if !known {
			continue
		}
		if conservative || t.degraded {
			remaining = t.conservative(remaining)
		}
		need := est.amount(item.key.Metric)
		// 判据与 Available 对齐：remaining-reserved <= 0 即视为不可用。
		// need>0 时该式已被下一条覆盖；need==0 时靠它挡住 remaining==0，
		// 而不只是挡住负剩余。need>0 时要求剩余严格大于估量，留下安全边界。
		if remaining-item.reserved <= 0 || remaining-item.reserved-need < 0 {
			return func() {}, false
		}
	}
	for _, item := range matched {
		item.reserved += est.amount(item.key.Metric)
	}
	released := false
	release := func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		if released {
			return
		}
		released = true
		for _, item := range matched {
			item.reserved -= est.amount(item.key.Metric)
			if item.reserved < 0 {
				item.reserved = 0
			}
		}
		t.dirty = true
		t.maybeFlush(t.now())
	}
	return release, true
}

// Available 判定账号/模型此刻是否可用，并给出可区分的原因。
//
// 求值规则：kind: quota 任一项触顶即不可用；kind: rate 任一项触顶只表示「此刻跳过」，
// 返回 VerdictRateLimited，不标记账号死亡；expires_at 已过即不可用。账号级与模型级
// 条目相与生效：任一触顶即不可用。
func (t *Table) Available(scope Scope, model string, now time.Time) (bool, Reason) {
	t.mu.Lock()
	defer t.mu.Unlock()

	scope = t.normalizeScope(scope)
	model = effectiveModel(scope, model)
	matched := t.matchLocked(scope, model)

	reason := Reason{Verdict: VerdictOK, Estimated: t.degraded, Model: model}
	if len(matched) == 0 {
		return true, reason
	}

	if block, ok := t.blockedLocked(scope, model, now); ok {
		return false, Reason{
			Verdict:       block.verdict,
			Until:         block.until,
			Model:         model,
			Estimated:     reason.Estimated,
			RequiresClear: block.requiresClear,
			Detail:        "上游错误学习得到的不可用标记",
		}
	}

	var rate *Reason
	for _, item := range matched {
		item.rollover(now)
		if !item.expiresAt.IsZero() && !now.Before(item.expiresAt) {
			return false, Reason{
				Verdict:   VerdictExpired,
				Until:     item.expiresAt,
				Metric:    item.key.Metric,
				Window:    item.key.Window,
				Kind:      item.key.Kind,
				Model:     model,
				Estimated: reason.Estimated,
				Detail:    "额度文档已到期",
			}
		}
		remaining, known, conservative := item.remaining()
		if !known {
			continue
		}
		if conservative || t.degraded {
			remaining = t.conservative(remaining)
			reason.Estimated = true
		}
		if item.assumed {
			reason.Estimated = true
		}
		if remaining-item.reserved > 0 {
			continue
		}
		candidate := t.exhaustion(item, now)
		candidate.Model = model
		candidate.Estimated = candidate.Estimated || reason.Estimated
		if item.key.Kind == "quota" {
			return false, candidate
		}
		if rate == nil {
			rate = &candidate
		}
	}
	if rate != nil {
		return false, *rate
	}
	return true, reason
}

// exhaustion 给出条目触顶时的不可用标记，任何标记都带 until（absolute 除外）。
func (t *Table) exhaustion(item *entry, now time.Time) Reason {
	isRate := item.key.Kind == "rate"
	reason := Reason{
		Metric:    item.key.Metric,
		Window:    item.key.Window,
		Kind:      item.key.Kind,
		Until:     item.resetsAt,
		Estimated: item.stale,
	}
	if isRate {
		reason.Verdict = VerdictRateLimited
	} else {
		reason.Verdict = VerdictQuotaExhausted
	}
	if item.resetsAt.After(now) {
		return reason
	}
	if isRate {
		// rate 触顶只作用于本请求：固定短退避，不写账号状态。
		reason.Until = now.Add(t.opts.RateBackoff)
		reason.Detail = "秒级限流，短退避后重试；未写账号状态"
		return reason
	}
	if item.spec.kind == windowAbsolute {
		// absolute 没有窗口可等：安排一个未来时刻自动复活是错的。
		reason.Until = time.Time{}
		reason.RequiresClear = true
		reason.Detail = "absolute 额度触顶，需显式清除或一次真实观测覆盖"
		return reason
	}
	remaining := item.spec.end(item.windowStart).Sub(now)
	if remaining <= 0 || remaining > t.opts.BlockCap {
		remaining = t.opts.BlockCap
	}
	reason.Until = now.Add(remaining)
	reason.Detail = "窗口额度触顶，至窗口恢复或保守上限"
	return reason
}

// conservative 对剩余量施加保守系数。估算态下宁可低估：低估只会少发请求，
// 高估会让已耗尽的账号继续被选中。
func (t *Table) conservative(remaining float64) float64 {
	if math.IsInf(remaining, 1) {
		return remaining
	}
	return remaining * t.opts.ConservativeFactor
}

// matchLocked 返回与作用域匹配的条目：账号相同，池在指定时相同，模型级条目只算本次
// 请求的模型，账号级条目（模型为空）恒参与。
func (t *Table) matchLocked(scope Scope, model string) []*entry {
	var out []*entry
	for _, item := range t.entries {
		if item.key.Account != scope.Account {
			continue
		}
		if scope.Pool != "" && item.key.Pool != scope.Pool {
			continue
		}
		if item.key.Model != "" && item.key.Model != model {
			continue
		}
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key.less(out[j].key) })
	return out
}

// hasEntriesLocked 报告该作用域上是否存在已声明的条目。
func (t *Table) hasEntriesLocked(scope Scope, model string) bool {
	return len(t.matchLocked(scope, model)) > 0
}

// blockedLocked 返回当前生效的不可用标记；learned blocks 与条目上的 resets_at 分开存，
// 因此 Available 与 Reserve 用同一判据。
func (t *Table) blockedLocked(scope Scope, model string, now time.Time) (block, bool) {
	key := blockKey{Account: scope.Account, Pool: scope.Pool, Model: model}
	item, ok := t.blocks[key]
	if !ok {
		return block{}, false
	}
	if !now.Before(item.until) {
		delete(t.blocks, key)
		return block{}, false
	}
	return item, true
}

// clearBlockLocked 清除某个作用域上学习到的不可用标记。
func (t *Table) clearBlockLocked(scope Scope, model string) {
	// 账号级标记（模型为空）与本次模型级标记都要清：一次成功返回既证明该模型恢复，
	// 也证明账号整体恢复。
	for _, key := range []blockKey{
		{Account: scope.Account, Pool: scope.Pool, Model: model},
		{Account: scope.Account, Pool: scope.Pool},
	} {
		delete(t.blocks, key)
	}
}

// applyPending 把快照载入的状态接到已声明的条目上，并丢弃没有声明的状态。
func (t *Table) applyPending() {
	for key, state := range t.pending {
		item, ok := t.entries[key]
		if !ok {
			continue
		}
		state.apply(item)
		delete(t.pending, key)
	}
}

// joinKeys 把缺声明的键排成人读列表。
func joinKeys(keys []Key) string {
	parts := make([]string, len(keys))
	for i, key := range keys {
		parts[i] = key.String()
	}
	return fmt.Sprintf("%v", parts)
}

// String 返回键的可读写法，用于报错。
func (k Key) String() string {
	scope := k.Account
	if k.Pool != "" {
		scope += "/" + k.Pool
	}
	if k.Model != "" {
		scope += "/" + k.Model
	}
	return fmt.Sprintf("%s[%s %s %s]", scope, k.Kind, k.Metric, k.Window)
}

// less 给键排序，让求值与报错的顺序稳定。
func (k Key) less(other Key) bool {
	if k.Account != other.Account {
		return k.Account < other.Account
	}
	if k.Pool != other.Pool {
		return k.Pool < other.Pool
	}
	if k.Model != other.Model {
		return k.Model < other.Model
	}
	if k.Kind != other.Kind {
		return k.Kind < other.Kind
	}
	if k.Metric != other.Metric {
		return k.Metric < other.Metric
	}
	return k.Window < other.Window
}
