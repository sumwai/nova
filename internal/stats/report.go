package stats

import "time"

// Report 是一次统计查询的结果。
//
// 结构上分成四块，界限是「范围」而不是「类别」：
//   - Process 是本次进程的事实；
//   - Accounting 是跨重启的记账事实（累计从哪算起、库在哪、写失败了多少）；
//   - Lifetime 是跨重启的标量累计，不受查询条件影响；
//   - Window 是不大于保留期的明细聚合，受查询条件影响，并自述覆盖范围。
//
// 这样分是因为重启与裁剪都会改变「能看到多少」：把不同范围混在同一组数字里，
// 读者无法判断「少了的那部分」是被过滤掉了、被裁剪掉了，还是本次进程还没跑。
type Report struct {
	GeneratedAt time.Time  `json:"generated_at"`
	Process     Process    `json:"process"`
	Accounting  Accounting `json:"accounting"`
	Lifetime    Lifetime   `json:"lifetime"`
	Window      Window     `json:"window"`
}

// Process 是本次进程的事实。
type Process struct {
	StartedAt time.Time `json:"started_at"`
	// UptimeSeconds 是本次进程已运行的秒数，与 Accounting.Since 不同。
	UptimeSeconds int64 `json:"uptime_seconds"`
	// Restarts 是本次启动之前已经启动过的次数；首次运行为 0。
	Restarts int64 `json:"restarts"`
}

// Accounting 是跨重启的记账事实。
type Accounting struct {
	// Since 是累计的起点：库第一次建立时的时刻，不是本次进程的启动时刻。
	Since time.Time `json:"since"`
	// Persisted 报告本次是否接上了持久化存储；为假时 Lifetime 会随重启归零。
	Persisted bool `json:"persisted"`
	// RetentionDays 是请求记录的保留天数；Lifetime 不受它影响。
	RetentionDays int `json:"retention_days"`
	// WriteErrors / LastWriteError 是落库失败的累计与最后一条消息。
	// 观测失败不影响转发，但也不能惄声消失：它们随查询结果一起返回。
	WriteErrors    int64  `json:"write_errors,omitempty"`
	LastWriteError string `json:"last_write_error,omitempty"`
}

// Lifetime 是跨重启的标量累计。
//
// 只放标量：任何按维度拆开的东西都会随维度基数增长，而这里不裁剪，
// 于是「不裁剪 + 高基数键」会变成一条没有上限的增长路径。
type Lifetime struct {
	Requests             int64     `json:"requests"`
	Succeeded            int64     `json:"succeeded"`
	Failed               int64     `json:"failed"`
	RetriedRequests      int64     `json:"retried_requests"`
	UsageUnknownRequests int64     `json:"usage_unknown_requests"`
	Usage                UsageView `json:"usage"`
}

// Window 是保留期内的明细聚合与覆盖范围。
type Window struct {
	// Oldest / Newest 是本次读到的记录里最早与最晚一条的时刻；窗口为空时省略。
	Oldest *time.Time `json:"oldest,omitempty"`
	Newest *time.Time `json:"newest,omitempty"`
	// Retained 是本次读入的记录条数。它受保留期与扫描上限共同限制。
	Retained int `json:"retained"`
	// ScanLimited 为真表示触到了扫描上限，还有更早的记录没被读入。
	// 它单独出现，是为了让「数字只覆盖了一部分」不是一件靠猜的事。
	ScanLimited bool `json:"scan_limited,omitempty"`
	// DroppedPending 是 join 暂存超限被丢弃的尝试数；正常路径恒为 0。
	DroppedPending int64 `json:"dropped_pending,omitempty"`
	// OrphanAttempts 是没有关联键、无法并入任何请求的尝试数；正常路径恒为 0。
	OrphanAttempts int64 `json:"orphan_attempts,omitempty"`
	// TruncatedAxes 列出因桶数超过上限而被截断的分组轴。
	TruncatedAxes []string `json:"truncated_axes,omitempty"`

	Totals     Totals        `json:"totals"`
	Usage      UsageView     `json:"usage"`
	Breakdowns Breakdowns    `json:"breakdowns"`
	Requests   []RequestView `json:"requests,omitempty"`
}

// Totals 是窗口内不分组的总计。
type Totals struct {
	Requests             int         `json:"requests"`
	Succeeded            int         `json:"succeeded"`
	Failed               int         `json:"failed"`
	Stream               int         `json:"stream"`
	Attempts             int         `json:"attempts"`
	RetriedRequests      int         `json:"retried_requests"`
	UsageUnknownRequests int         `json:"usage_unknown_requests"`
	WrittenBytes         int64       `json:"written_bytes"`
	DurationMS           LatencyView `json:"duration_ms"`
}

// UsageView 是 token 用量的输出形状。
//
// 与 logjson 的 usage 分组同名同形，日志与统计因此能用同一套 jq 表达式。
// Source 只在单条请求或单条尝试上有意义，聚合结果里恒为空并被省略。
type UsageView struct {
	Source           string `json:"source,omitempty"`
	InputTokens      int    `json:"input_tokens"`
	OutputTokens     int    `json:"output_tokens"`
	CacheReadTokens  int    `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int    `json:"cache_write_tokens,omitempty"`
	ReasoningTokens  int    `json:"reasoning_tokens,omitempty"`
	ServerToolUses   int    `json:"server_tool_uses,omitempty"`
}

// LatencyView 是耗时分位数。
//
// 取值来自定长桶直方图，因此是近似：每个分位给出它所在桶的上界。分位数要求精确值
// 时得保留全部原始时长，那与「内存与请求量解耦」直接冲突。
type LatencyView struct {
	P50 int64 `json:"p50"`
	P90 int64 `json:"p90"`
	P95 int64 `json:"p95"`
	P99 int64 `json:"p99"`
	Max int64 `json:"max"`
}

// Breakdowns 是按各维度拆开的聚合结果。
//
// 轴固定且各自一个命名字段，而不是 map：map 的键序在 JSON 里按字典序排出，
// 于是字段顺序会随键名漂移，而这里的顺序是给人读的。
type Breakdowns struct {
	ByModel     []modelBucket     `json:"by_model,omitempty"`
	ByProvider  []providerBucket  `json:"by_provider,omitempty"`
	ByUpstream  []upstreamBucket  `json:"by_upstream,omitempty"`
	ByClient    []clientBucket    `json:"by_client,omitempty"`
	ByStatus    []statusBucket    `json:"by_status,omitempty"`
	ByProtocol  []protocolBucket  `json:"by_protocol,omitempty"`
	ByErrorCode []errorCodeBucket `json:"by_error_code,omitempty"`
}

// bucketStats 是每个分组桶共有的计数部分，由各轴的结构体嵌入。
type bucketStats struct {
	Requests int `json:"requests"`
	// Attempts 只在尝试级轴（provider / upstream）上出现：请求级轴上它恒为 0 并被省略。
	Attempts   int         `json:"attempts,omitempty"`
	Failed     int         `json:"failed"`
	Usage      UsageView   `json:"usage"`
	DurationMS LatencyView `json:"duration_ms"`
}

type modelBucket struct {
	Model string `json:"model"`
	bucketStats
}

type providerBucket struct {
	Provider string `json:"provider"`
	bucketStats
}

type upstreamBucket struct {
	Upstream string `json:"upstream"`
	bucketStats
}

type clientBucket struct {
	Client string `json:"client"`
	bucketStats
}

type statusBucket struct {
	Status string `json:"status"`
	bucketStats
}

type protocolBucket struct {
	Protocol string `json:"protocol"`
	bucketStats
}

type errorCodeBucket struct {
	ErrorCode string `json:"error_code"`
	bucketStats
}

// RequestView 是逐请求的明细。
type RequestView struct {
	Time         time.Time     `json:"time"`
	RequestID    string        `json:"request_id,omitempty"`
	Client       string        `json:"client"`
	UserAgent    string        `json:"user_agent,omitempty"`
	Protocol     string        `json:"protocol"`
	Model        string        `json:"model"`
	Stream       bool          `json:"stream"`
	HTTPStatus   int           `json:"http_status"`
	DurationMS   int64         `json:"duration_ms"`
	WrittenBytes int           `json:"written_bytes"`
	ErrorCode    string        `json:"error_code,omitempty"`
	RemoteAddr   string        `json:"remote_addr,omitempty"`
	UsageUnknown int           `json:"usage_unknown,omitempty"`
	Usage        *UsageView    `json:"usage,omitempty"`
	Attempts     []AttemptView `json:"attempts,omitempty"`
}

// AttemptView 是明细里的一条上游尝试。
type AttemptView struct {
	Provider   string     `json:"provider"`
	Upstream   string     `json:"upstream"`
	Account    string     `json:"account,omitempty"`
	Outcome    string     `json:"outcome"`
	ErrorCode  string     `json:"error_code,omitempty"`
	DurationMS int64      `json:"duration_ms"`
	Usage      *UsageView `json:"usage,omitempty"`
}
