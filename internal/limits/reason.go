package limits

import "time"

// Verdict 是一次可用性判定的类别。
//
// 关键区分是 rate 与 quota：两者在客户端侧都表现为「稍后或换渠道重试」，
// 但 rate 只表示此刻跳过、不写账号状态，quota 才表示本窗口额度已耗尽。
type Verdict string

const (
	// VerdictOK 表示可用。
	VerdictOK Verdict = "ok"
	// VerdictRateLimited 表示秒级限流，此刻跳过，短退避后重试；未标记账号死亡。
	VerdictRateLimited Verdict = "rate-limited"
	// VerdictQuotaExhausted 表示窗口额度耗尽，至 Until 不可用。
	VerdictQuotaExhausted Verdict = "quota-exhausted"
	// VerdictExpired 表示额度文档已过 expires_at。
	VerdictExpired Verdict = "expired"
)

// Reason 说明一次可用性判定的依据。
//
// 任何不可用标记都带 Until（quota 且 absolute 的条目除外，那类只能显式清除，
// 用 RequiresClear 表达）。Estimated 表示判定基于估算而非权威观测：快照载入失败、
// 记账断档或观测过期都会置位，且剩余量已按保守系数缩小——偏保守，不静默乐观。
type Reason struct {
	Verdict       Verdict
	Until         time.Time
	Metric        string
	Window        string
	Kind          string
	Model         string
	Estimated     bool
	RequiresClear bool
	Detail        string
}

// block 是一条学习到的不可用标记，必带 until。
type block struct {
	until         time.Time
	verdict       Verdict
	requiresClear bool
}

// blockKey 是学习到的不可用标记的键：账号、池与模型。
type blockKey struct {
	Account string
	Pool    string
	Model   string
}
