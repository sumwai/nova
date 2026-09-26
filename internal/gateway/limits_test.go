package gateway

import (
	"bytes"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/nova/internal/config"
	"github.com/sumwai/nova/internal/domain"
	"github.com/sumwai/nova/internal/limits"
	"github.com/sumwai/nova/internal/pipeline"
)

// hintedError 让测试构造一个携带 Retry-After 提示的额度耗尽错误。
type hintedError struct {
	err   error
	delay time.Duration
}

func (e hintedError) Error() string                     { return e.err.Error() }
func (e hintedError) Unwrap() error                     { return e.err }
func (e hintedError) RetryAfter() (time.Duration, bool) { return e.delay, true }

// quotaErr 造一条上游额度耗尽错误，供自适应计数使用。
func quotaErr() error {
	return domain.NewError(domain.CodeUpstreamQuotaExhausted, "上游额度耗尽")
}

// limitedConfig 造一条单账号渠道，账号带一份 quota 声明。
func limitedConfig() *config.Config {
	account := acct(1, 1)
	account.Limits = []limits.Declared{{
		Kind:   "quota",
		Metric: "requests",
		Window: "1m",
		Limit:  floatPtr(100),
	}}
	return accountConfig(false, account)
}

// TestObserveLogsRejectedHeaderKeys 守护被拒绝的响应头键不会静默丢弃。
//
// 声明写的是 quota/requests，响应头产出 rate/requests，键不匹配时 MergeObserved
// 会拒收；这一事实必须体现在运行日志里，否则「响应头观测已生效」无从验证。
func TestObserveLogsRejectedHeaderKeys(t *testing.T) {
	var out bytes.Buffer
	log, err := newLogger(&config.Config{LogLevel: "info", LogFormat: "json"}, &out)
	if err != nil {
		t.Fatalf("建日志失败：%v", err)
	}
	runtime, err := newLimitRuntime(limitedConfig().Providers, "", 0, log, nil)
	if err != nil {
		t.Fatalf("建额度表失败：%v", err)
	}

	header := http.Header{}
	header.Set("x-ratelimit-remaining-requests", "5")
	runtime.Observe(domain.Route{Provider: "relay"}, "shared", header, true)

	got := out.String()
	if !strings.Contains(got, "额度响应头观测未合并") {
		t.Fatalf("被拒绝的观测应在日志里可见，实际：%s", got)
	}
	if !strings.Contains(got, "requests") {
		t.Fatalf("日志应点明缺失的键，实际：%s", got)
	}

	// 同一份声明下第二个请求只应复用提醒，不再重复输出。
	out.Reset()
	runtime.Observe(domain.Route{Provider: "relay"}, "shared", header, true)
	if strings.Contains(out.String(), "额度响应头观测未合并") {
		t.Fatalf("同一组被拒绝的键不应逐请求重复输出，实际：%s", out.String())
	}
}

// TestAvailabilityCarriesReason 守护判定原因不被丢弃。
func TestAvailabilityCarriesReason(t *testing.T) {
	cfg := limitedConfig()
	cfg.Providers[0].Accounts[0].Limits = []limits.Declared{{
		Kind:      "quota",
		Metric:    "requests",
		Window:    "1m",
		Remaining: floatPtr(0),
	}}
	runtime, err := newLimitRuntime(cfg.Providers, "", 0, nil, nil)
	if err != nil {
		t.Fatalf("建额度表失败：%v", err)
	}

	got := runtime.availability("relay", "", "shared")
	if got.ok {
		t.Fatal("remaining==0 应判为不可用")
	}
	if got.reason.Verdict != limits.VerdictQuotaExhausted {
		t.Fatalf("判定类别=%q，期望 %q", got.reason.Verdict, limits.VerdictQuotaExhausted)
	}
	if got.reason.Metric != "requests" || got.reason.Window != "1m" {
		t.Fatalf("原因应带 metric/window，实际 %+v", got.reason)
	}
}

// TestAdaptiveEscalationNeedsThreshold 守护保守自适应规则：未达 N 次不写账号状态。
func TestAdaptiveEscalationNeedsThreshold(t *testing.T) {
	cfg := limitedConfig()
	runtime, err := newLimitRuntime(cfg.Providers, "", 3, nil, nil)
	if err != nil {
		t.Fatalf("建额度表失败：%v", err)
	}
	route := domain.Route{Provider: "relay"}

	runtime.LearnExhausted(route, "shared", quotaErr())
	runtime.LearnExhausted(route, "shared", quotaErr())
	if !runtime.available("relay", "", "shared") {
		t.Fatal("连续失败未达阈值时不应标记不可用")
	}

	runtime.LearnExhausted(route, "shared", quotaErr())
	ok, reason := runtime.tables["relay"][""].Available(limits.Scope{Account: "relay"}, "shared", time.Now())
	if ok || reason.Verdict != limits.VerdictQuotaExhausted {
		t.Fatalf("第 N 次失败后应标记 quota-exhausted，实际 ok=%v verdict=%v", ok, reason.Verdict)
	}
}

// TestSuccessResetsAdaptiveState 守护「出现成功立即降级并 Clear」。
func TestSuccessResetsAdaptiveState(t *testing.T) {
	cfg := limitedConfig()
	runtime, err := newLimitRuntime(cfg.Providers, "", 2, nil, nil)
	if err != nil {
		t.Fatalf("建额度表失败：%v", err)
	}
	route := domain.Route{Provider: "relay"}

	runtime.LearnExhausted(route, "shared", quotaErr())
	runtime.Observe(route, "shared", nil, true)
	runtime.LearnExhausted(route, "shared", quotaErr())
	if !runtime.available("relay", "", "shared") {
		t.Fatal("成功应把连续失败计数清零，不应在本轮标记不可用")
	}

	// 再失败一次达到阈值，标记写入；随后一次成功必须清除它。
	runtime.LearnExhausted(route, "shared", quotaErr())
	if runtime.available("relay", "", "shared") {
		t.Fatal("连续两次失败后应标记不可用")
	}
	runtime.Observe(route, "shared", nil, true)
	if !runtime.available("relay", "", "shared") {
		t.Fatal("出现成功返回后应清除不可用标记")
	}
}

// TestLearnExhaustedCapsRetryAfter 守护上游 Retry-After 不得把渠道停用任意长。
func TestLearnExhaustedCapsRetryAfter(t *testing.T) {
	cfg := limitedConfig()
	runtime, err := newLimitRuntime(cfg.Providers, "", 1, nil, nil)
	if err != nil {
		t.Fatalf("建额度表失败：%v", err)
	}
	route := domain.Route{Provider: "relay"}

	runtime.LearnExhausted(route, "shared", hintedError{err: quotaErr(), delay: 365 * 24 * time.Hour})

	_, reason := runtime.tables["relay"][""].Available(limits.Scope{Account: "relay"}, "shared", time.Now())
	if reason.Verdict != limits.VerdictQuotaExhausted {
		t.Fatalf("应标记为额度耗尽，实际 %v", reason.Verdict)
	}
	if reason.Until.After(time.Now().Add(2 * time.Hour)) {
		t.Fatalf("超长 Retry-After 应被保守上限截断，until=%v", reason.Until)
	}
	if !reason.Until.After(time.Now()) {
		t.Fatalf("until 应在未来，实际 %v", reason.Until)
	}
}

// TestLearnExhaustedHonorsShortRetryAfter 守护小于保守上限的 Retry-After 被尊重。
func TestLearnExhaustedHonorsShortRetryAfter(t *testing.T) {
	cfg := limitedConfig()
	runtime, err := newLimitRuntime(cfg.Providers, "", 1, nil, nil)
	if err != nil {
		t.Fatalf("建额度表失败：%v", err)
	}
	route := domain.Route{Provider: "relay"}

	runtime.LearnExhausted(route, "shared", hintedError{err: quotaErr(), delay: 30 * time.Second})

	_, reason := runtime.tables["relay"][""].Available(limits.Scope{Account: "relay"}, "shared", time.Now())
	if got := time.Until(reason.Until); got > time.Minute || got < 10*time.Second {
		t.Fatalf("短 Retry-After 应被尊重，距 now = %v", got)
	}
}

// TestUSDQuotaUsesPrice 守护 usd 额度按价格层折算：估量拒绝与本地累计都生效。
func TestUSDQuotaUsesPrice(t *testing.T) {
	provider := pricedProvider("relay", "shared", "USD", 100)
	provider.Accounts[0].Limits = []limits.Declared{{
		Kind:   "quota",
		Metric: "usd",
		Window: "1m",
		Limit:  floatPtr(1),
	}}
	cfg := &config.Config{Providers: []config.Provider{provider}}
	table := testPriceTable(t, cfg)
	runtime, err := newLimitRuntime(cfg.Providers, "", 1, nil, table)
	if err != nil {
		t.Fatalf("建额度表失败：%v", err)
	}
	route := domain.Route{Provider: "relay", PriceKey: "p/relay"}

	// 20000 输出 token × 100/Mtok = 2 USD，超过 1 的额度，应在准入阶段被拒。
	if _, ok := runtime.Reserve(route, "shared", pipeline.CostEstimate{Requests: 1, Tokens: 20000}); ok {
		t.Fatal("usd 估量超过额度时应拒绝预留")
	}

	settle, ok := runtime.Reserve(route, "shared", pipeline.CostEstimate{Requests: 1, Tokens: 100})
	if !ok {
		t.Fatal("小额 usd 估量应放行")
	}
	settle(domain.Usage{Source: domain.UsageSourceUpstream, OutputTokens: 20000}, nil)
	if runtime.available("relay", "", "shared") {
		t.Fatal("usd 本地累计超过额度后应判为不可用")
	}
}

// TestNoTableIsAlwaysAvailable 守护「没有声明额度的账号一律视为可用」。
func TestNoTableIsAlwaysAvailable(t *testing.T) {
	cfg := accountConfig(false, acct(1, 1))
	runtime, err := newLimitRuntime(cfg.Providers, "", 1, nil, nil)
	if err != nil {
		t.Fatalf("建额度表失败：%v", err)
	}
	// 没有声明 → 没有表；即使上游一直报额度耗尽也不应被剔除。
	route := domain.Route{Provider: "relay"}
	runtime.LearnExhausted(route, "shared", quotaErr())
	if !runtime.available("relay", "", "shared") {
		t.Fatal("没有额度声明的账号不得因缺少表被剔除")
	}
}

// TestTableStateDirKeepsSnapshotsInsideStateDir 守护额度快照的落点：
// 作用域名含路径分隔符或 .. 时不得把快照写到状态目录之外。
func TestTableStateDirKeepsSnapshotsInsideStateDir(t *testing.T) {
	const stateDir = "/var/state/nova"

	for _, scope := range []string{"relay", "../../etc/passwd", "relay/../../x", "..", ".", "a/b"} {
		got := tableStateDir(stateDir, scope)
		rel, err := filepath.Rel(stateDir, got)
		if err != nil {
			t.Fatalf("作用域 %q 的落点 %q 无法相对状态目录表达：%v", scope, got, err)
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Fatalf("作用域 %q 的落点 %q 越出了状态目录", scope, got)
		}
	}

	// 名字未被改动时保持可读，被改动时补短哈希以避开「不同作用域撞同一目录」。
	if got := tableStateDir(stateDir, "relay"); got != filepath.Join(stateDir, "limits", "relay") {
		t.Fatalf("未被改动的名字应当原样保留，得到 %q", got)
	}
	if tableStateDir(stateDir, "a/b") == tableStateDir(stateDir, "a-b") {
		t.Fatal("两个不同的作用域不得落到同一个快照目录")
	}
}
