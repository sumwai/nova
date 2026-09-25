package stats

import (
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/sumwai/nova/internal/domain"
)

// maxBreakdownBuckets 是每个分组轴返回的桶数上限。
//
// 超限时按请求数降序取前若干个，其余归入截断计数。不设上限的分组会在模型发现
// 返回几百条时把输出撑成一份难以阅读的清单；截断哪些轴由 TruncatedAxes 报出，
// 情形因此可见而不是静默丢失。
const maxBreakdownBuckets = 50

// latencyBounds 是耗时直方图的桶上界（毫秒），最后一档之上去溢出桶。
//
// 上界按「网关耗时关心什么」取：个位数毫秒是本地直连，百毫秒量级是正常生成，
// 秒级以上开始值得注意，一分钟以上基本等同于故障。
var latencyBounds = [...]int64{
	1, 2, 5, 10, 25, 50, 100, 250, 500,
	1000, 2500, 5000, 10000, 30000, 60000,
}

// histogram 是定长桶耗时直方图。
//
// 用直方图而不是保留全部耗时样本：分位数因此是近似的（取所在桶的上界），
// 但内存与请求量解耦，窗口容量不会因为「多算几个分位」而额外膨胀。
type histogram struct {
	counts [len(latencyBounds) + 1]int64
	count  int64
	max    int64
}

func (h *histogram) add(milliseconds int64) {
	if milliseconds < 0 {
		milliseconds = 0
	}
	h.count++
	if milliseconds > h.max {
		h.max = milliseconds
	}
	for i, bound := range latencyBounds {
		if milliseconds <= bound {
			h.counts[i]++
			return
		}
	}
	h.counts[len(latencyBounds)]++
}

func (h histogram) view() LatencyView {
	return LatencyView{
		P50: h.quantile(0.50),
		P90: h.quantile(0.90),
		P95: h.quantile(0.95),
		P99: h.quantile(0.99),
		Max: h.max,
	}
}

// quantile 返回第 q 分位的近似值：该分位落在哪个桶，就取哪个桶的上界。
//
// 桶上界可能超过窗口内的最大值（所有值都是 40ms 时会落进 ≤50ms 的桶），
// 因此结果要钳到 max：分位数不可能大于最大值，报出一个比 max 大的 p50 只会
// 让人以为数字算错了。溢出桶没有上界，直接取 max。
func (h histogram) quantile(q float64) int64 {
	if h.count == 0 {
		return 0
	}
	target := int64(math.Ceil(q * float64(h.count)))
	if target < 1 {
		target = 1
	}
	var cumulative int64
	for i, count := range h.counts {
		cumulative += count
		if cumulative < target {
			continue
		}
		if i >= len(latencyBounds) || latencyBounds[i] > h.max {
			return h.max
		}
		return latencyBounds[i]
	}
	return h.max
}

// usageTotals 是用量的累加器：只做整数求和，不参与 domain.Usage 的来源判定。
//
// 聚合结果混有多种来源，Source 在这里没有意义；来源只在单条请求或单条尝试上有答案。
type usageTotals struct {
	input      int
	output     int
	cacheRead  int
	cacheWrite int
	reasoning  int
	tools      int
}

func (u *usageTotals) add(v domain.Usage) {
	u.input += v.InputTokens
	u.output += v.OutputTokens
	u.cacheRead += v.CacheReadTokens
	u.cacheWrite += v.CacheWriteTokens
	u.reasoning += v.ReasoningTokens
	u.tools += v.ServerToolUses
}

func (u usageTotals) view() UsageView {
	return UsageView{
		InputTokens:      u.input,
		OutputTokens:     u.output,
		CacheReadTokens:  u.cacheRead,
		CacheWriteTokens: u.cacheWrite,
		ReasoningTokens:  u.reasoning,
		ServerToolUses:   u.tools,
	}
}

// totalsAcc 是窗口内的总计累加器。
type totalsAcc struct {
	requests     int
	succeeded    int
	failed       int
	stream       int
	attempts     int
	retried      int
	usageUnknown int
	writtenBytes int64
	latency      histogram
	usage        usageTotals
}

func (a *totalsAcc) add(req Request) {
	a.requests++
	if req.Status < 400 {
		a.succeeded++
	} else {
		a.failed++
	}
	if req.Stream {
		a.stream++
	}
	a.attempts += len(req.Attempts)
	if len(req.Attempts) > 1 {
		a.retried++
	}
	if req.UsageUnknown > 0 {
		a.usageUnknown++
	}
	a.writtenBytes += int64(req.WrittenBytes)
	a.latency.add(req.DurationMS)
	a.usage.add(req.Usage)
}

func (a totalsAcc) view() Totals {
	return Totals{
		Requests:             a.requests,
		Succeeded:            a.succeeded,
		Failed:               a.failed,
		Stream:               a.stream,
		Attempts:             a.attempts,
		RetriedRequests:      a.retried,
		UsageUnknownRequests: a.usageUnknown,
		WrittenBytes:         a.writtenBytes,
		DurationMS:           a.latency.view(),
	}
}

// bucketAcc 是一个分组桶的累加器。
type bucketAcc struct {
	requests int
	attempts int
	failed   int
	usage    usageTotals
	latency  histogram
}

func (a *bucketAcc) view() bucketStats {
	return bucketStats{
		Requests:   a.requests,
		Attempts:   a.attempts,
		Failed:     a.failed,
		Usage:      a.usage.view(),
		DurationMS: a.latency.view(),
	}
}

// reportMeta 是聚合所需的、不在记录里的那些事实。
type reportMeta struct {
	now             time.Time
	startedAt       time.Time
	accountingSince time.Time
	restarts        int64
	lifetime        lifetimeCounters
	retention       time.Duration
	scanLimited     bool
	droppedPending  int64
	orphanAttempts  int64
	writeErrors     int64
	lastWriteError  string
	persisted       bool
}

// buildReport 把一批已读入的记录聚合成查询结果。
//
// 过滤与分组全部在这里完成，不推给 SQL：过滤语义是 pattern.Match（`*` 跨 `/`、
// 大小写按 rune 折叠），SQL 的 LIKE 与 GLOB 都不等价，两处各写一份会让同一份
// 查询条件在不同后端上给出不同结果。
func buildReport(records []Request, meta reportMeta, q Query) Report {
	report := Report{
		GeneratedAt: meta.now,
		Process: Process{
			StartedAt:     meta.startedAt,
			UptimeSeconds: int64(meta.now.Sub(meta.startedAt).Seconds()),
			Restarts:      meta.restarts,
		},
		Accounting: Accounting{
			Since:          meta.accountingSince,
			Persisted:      meta.persisted,
			RetentionDays:  int(meta.retention.Hours() / 24),
			WriteErrors:    meta.writeErrors,
			LastWriteError: meta.lastWriteError,
		},
		Lifetime: Lifetime{
			Requests:             meta.lifetime.requests,
			Succeeded:            meta.lifetime.succeeded,
			Failed:               meta.lifetime.failed,
			RetriedRequests:      meta.lifetime.retriedRequests,
			UsageUnknownRequests: meta.lifetime.usageUnknownRequests,
			Usage:                meta.lifetime.usage.view(),
		},
	}

	window := Window{
		Retained:       len(records),
		ScanLimited:    meta.scanLimited,
		DroppedPending: meta.droppedPending,
		OrphanAttempts: meta.orphanAttempts,
	}
	if len(records) > 0 {
		oldest, newest := records[0].Time, records[0].Time
		for _, req := range records {
			if req.Time.Before(oldest) {
				oldest = req.Time
			}
			if req.Time.After(newest) {
				newest = req.Time
			}
		}
		window.Oldest, window.Newest = &oldest, &newest
	}

	var totals totalsAcc
	matched := make([]Request, 0, len(records))
	for _, req := range records {
		if !q.matches(req) {
			continue
		}
		matched = append(matched, req)
		totals.add(req)
	}

	window.Totals = totals.view()
	window.Usage = totals.usage.view()
	window.Breakdowns, window.TruncatedAxes = buildBreakdowns(matched, q)
	if q.Detail {
		window.Requests = buildDetail(matched, q.Limit)
	}

	report.Window = window
	return report
}

// buildBreakdowns 按查询要求的轴逐个聚合。
func buildBreakdowns(records []Request, q Query) (Breakdowns, []string) {
	var breakdowns Breakdowns
	var truncated []string

	appendTruncated := func(axis string, cut bool) {
		if cut {
			truncated = append(truncated, axis)
		}
	}

	if q.groups("model") {
		items, cut := orderBuckets(requestAxis(records, func(req Request) string { return req.Model }))
		appendTruncated("by_model", cut)
		for _, item := range items {
			breakdowns.ByModel = append(breakdowns.ByModel, modelBucket{Model: item.key, bucketStats: item.stats})
		}
	}
	if q.groups("provider") {
		items, cut := orderBuckets(attemptAxis(records, func(attempt AttemptSummary) (string, bool) {
			return attempt.Provider, attempt.Provider != ""
		}))
		appendTruncated("by_provider", cut)
		for _, item := range items {
			breakdowns.ByProvider = append(breakdowns.ByProvider, providerBucket{Provider: item.key, bucketStats: item.stats})
		}
	}
	if q.groups("upstream") {
		items, cut := orderBuckets(attemptAxis(records, func(attempt AttemptSummary) (string, bool) {
			// 上游 id 是「渠道名 + 主机名」的展示串，这里只按值分组，不解析它。
			return attempt.Upstream, attempt.Upstream != ""
		}))
		appendTruncated("by_upstream", cut)
		for _, item := range items {
			breakdowns.ByUpstream = append(breakdowns.ByUpstream, upstreamBucket{Upstream: item.key, bucketStats: item.stats})
		}
	}
	if q.groups("client") {
		items, cut := orderBuckets(requestAxis(records, func(req Request) string { return req.Client }))
		appendTruncated("by_client", cut)
		for _, item := range items {
			breakdowns.ByClient = append(breakdowns.ByClient, clientBucket{Client: item.key, bucketStats: item.stats})
		}
	}
	if q.groups("status") {
		items, cut := orderBuckets(requestAxis(records, func(req Request) string { return strconv.Itoa(req.Status) }))
		appendTruncated("by_status", cut)
		for _, item := range items {
			breakdowns.ByStatus = append(breakdowns.ByStatus, statusBucket{Status: item.key, bucketStats: item.stats})
		}
	}
	if q.groups("protocol") {
		items, cut := orderBuckets(requestAxis(records, func(req Request) string { return req.Protocol }))
		appendTruncated("by_protocol", cut)
		for _, item := range items {
			breakdowns.ByProtocol = append(breakdowns.ByProtocol, protocolBucket{Protocol: item.key, bucketStats: item.stats})
		}
	}
	if q.groups("error_code") {
		items, cut := orderBuckets(errorCodeAxis(records))
		appendTruncated("by_error_code", cut)
		for _, item := range items {
			breakdowns.ByErrorCode = append(breakdowns.ByErrorCode, errorCodeBucket{ErrorCode: item.key, bucketStats: item.stats})
		}
	}

	return breakdowns, truncated
}

// orderedBucket 是一个已排好序、待渲染的分组桶。
type orderedBucket struct {
	key   string
	stats bucketStats
}

// requestAxis 按请求级事实分组：一次请求只进一个桶。
func requestAxis(records []Request, key func(Request) string) map[string]*bucketAcc {
	buckets := make(map[string]*bucketAcc, len(records))
	for _, req := range records {
		name := key(req)
		acc := buckets[name]
		if acc == nil {
			acc = &bucketAcc{}
			buckets[name] = acc
		}
		acc.requests++
		if req.Status >= 400 {
			acc.failed++
		}
		acc.usage.add(req.Usage)
		acc.latency.add(req.DurationMS)
	}
	return buckets
}

// errorCodeAxis 按错误码分组，只计失败请求。
//
// 成功请求的错误码是空串，把它们一起计进来会得到一个键为空串的桶，那个桶回答不了
// 任何问题，却会让「各错误码的请求数之和等于失败数」这条算术关系不再成立。
func errorCodeAxis(records []Request) map[string]*bucketAcc {
	buckets := make(map[string]*bucketAcc)
	for _, req := range records {
		if req.ErrorCode == "" {
			continue
		}
		acc := buckets[req.ErrorCode]
		if acc == nil {
			acc = &bucketAcc{}
			buckets[req.ErrorCode] = acc
		}
		acc.requests++
		acc.failed++
		acc.usage.add(req.Usage)
		acc.latency.add(req.DurationMS)
	}
	return buckets
}

// attemptAxis 按尝试级事实分组，一次请求可以进多个桶。
//
// requests 取「在该键上至少有一次尝试的不同请求数」：它可能小于 attempts（同一请求
// 在同一渠道上重试过），也可能使各桶 requests 之和大于窗口请求数（一次请求回退跨了
// 多条渠道）。两者并列出现，正是为了不把回退这件事藏起来。
func attemptAxis(records []Request, key func(AttemptSummary) (string, bool)) map[string]*bucketAcc {
	buckets := make(map[string]*bucketAcc)
	for _, req := range records {
		counted := make(map[string]bool, len(req.Attempts))
		for _, attempt := range req.Attempts {
			name, ok := key(attempt)
			if !ok {
				continue
			}
			acc := buckets[name]
			if acc == nil {
				acc = &bucketAcc{}
				buckets[name] = acc
			}
			acc.attempts++
			if attempt.Outcome != string(domain.AttemptOK) {
				acc.failed++
			}
			if attempt.Usage.Known() {
				acc.usage.add(attempt.Usage)
			}
			acc.latency.add(attempt.DurationMS)
			if !counted[name] {
				counted[name] = true
				acc.requests++
			}
		}
	}
	return buckets
}

// orderBuckets 把桶按请求数降序排列并截断，第二个返回值报告是否发生了截断。
func orderBuckets(buckets map[string]*bucketAcc) ([]orderedBucket, bool) {
	items := make([]orderedBucket, 0, len(buckets))
	for key, acc := range buckets {
		items = append(items, orderedBucket{key: key, stats: acc.view()})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].stats.Requests != items[j].stats.Requests {
			return items[i].stats.Requests > items[j].stats.Requests
		}
		// 尝试级轴上 requests 可能并列，再按尝试数排，最后按键名保证顺序确定。
		if items[i].stats.Attempts != items[j].stats.Attempts {
			return items[i].stats.Attempts > items[j].stats.Attempts
		}
		return items[i].key < items[j].key
	})
	if len(items) > maxBreakdownBuckets {
		return items[:maxBreakdownBuckets], true
	}
	return items, false
}

// buildDetail 取最新的若干条请求，最新的排在最前。
func buildDetail(records []Request, limit int) []RequestView {
	views := make([]RequestView, 0, limit)
	for i := len(records) - 1; i >= 0 && len(views) < limit; i-- {
		views = append(views, requestView(records[i]))
	}
	return views
}

func requestView(req Request) RequestView {
	view := RequestView{
		Time:         req.Time,
		RequestID:    req.RequestID,
		Client:       req.Client,
		UserAgent:    req.UserAgent,
		Protocol:     req.Protocol,
		Model:        req.Model,
		Stream:       req.Stream,
		HTTPStatus:   req.Status,
		DurationMS:   req.DurationMS,
		WrittenBytes: req.WrittenBytes,
		ErrorCode:    req.ErrorCode,
		RemoteAddr:   req.RemoteAddr,
		UsageUnknown: req.UsageUnknown,
	}
	if req.Usage.Known() {
		usage := usageViewOf(req.Usage)
		view.Usage = &usage
	}
	for _, attempt := range req.Attempts {
		item := AttemptView{
			Provider:   attempt.Provider,
			Upstream:   attempt.Upstream,
			Account:    attempt.Account,
			Outcome:    attempt.Outcome,
			ErrorCode:  attempt.ErrorCode,
			DurationMS: attempt.DurationMS,
		}
		if attempt.Usage.Known() {
			usage := usageViewOf(attempt.Usage)
			item.Usage = &usage
		}
		view.Attempts = append(view.Attempts, item)
	}
	return view
}

func usageViewOf(u domain.Usage) UsageView {
	return UsageView{
		Source:           string(u.Source),
		InputTokens:      u.InputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadTokens,
		CacheWriteTokens: u.CacheWriteTokens,
		ReasoningTokens:  u.ReasoningTokens,
		ServerToolUses:   u.ServerToolUses,
	}
}
