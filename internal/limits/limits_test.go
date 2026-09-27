package limits

import (
	"strings"
	"testing"
	"time"

	"github.com/sumwai/nova/internal/profile"
)

func TestAvailableCombinesAccountAndModelScopes(t *testing.T) {
	clock := newTestClock()

	// 账号级触顶、模型级充足：账号级条目命中即不可用。
	accountExhausted := New(Options{Account: "acct", Clock: clock.Now, FlushInterval: -1})
	if err := accountExhausted.MergeDeclared(declaredDoc([]profile.Limit{
		accountQuota("5h", 70, 70),
	})); err != nil {
		t.Fatalf("合并声明失败：%v", err)
	}
	if err := accountExhausted.MergeDeclared(&profile.LimitsDoc{
		Schema: profile.CurrentSchema,
		Models: map[string][]profile.Limit{
			"glm": {{Kind: "quota", Metric: "tokens", Window: "5h", Limit: ptr(1000), Used: ptr(0)}},
		},
	}); err != nil {
		t.Fatalf("合并声明失败：%v", err)
	}
	if ok, reason := accountExhausted.Available(Scope{Account: "acct"}, "glm", clock.Now()); ok {
		t.Fatalf("账号级触顶时模型级充足也应不可用，reason=%+v", reason)
	}

	// 账号级充足、模型级触顶：模型级条目只影响所列模型，其他模型仍可用。
	modelExhausted := New(Options{Account: "acct", Clock: clock.Now, FlushInterval: -1})
	if err := modelExhausted.MergeDeclared(declaredDoc([]profile.Limit{
		accountQuota("5h", 70, 0),
	})); err != nil {
		t.Fatalf("合并声明失败：%v", err)
	}
	if err := modelExhausted.MergeDeclared(&profile.LimitsDoc{
		Schema: profile.CurrentSchema,
		Models: map[string][]profile.Limit{
			"glm": {{Kind: "quota", Metric: "tokens", Window: "5h", Limit: ptr(1000), Used: ptr(1000)}},
		},
	}); err != nil {
		t.Fatalf("合并声明失败：%v", err)
	}
	if ok, _ := modelExhausted.Available(Scope{Account: "acct"}, "glm", clock.Now()); ok {
		t.Fatal("模型级触顶时该模型应不可用")
	}
	if ok, reason := modelExhausted.Available(Scope{Account: "acct"}, "other", clock.Now()); !ok {
		t.Fatalf("模型级触顶不应波及其他模型，reason=%+v", reason)
	}
}

func TestObservationWorseningAppliesImmediately(t *testing.T) {
	clock := newTestClock()
	table := newTable(clock, Options{})
	if err := table.MergeDeclared(declaredDoc([]profile.Limit{accountQuota("5h", 100, 0)})); err != nil {
		t.Fatalf("合并声明失败：%v", err)
	}
	key := Key{Scope: Scope{Account: "acct"}, Metric: "usd", Window: "5h", Kind: "quota"}

	if err := table.MergeObserved(observedDoc(clock.Now(), []profile.Limit{
		{Kind: "quota", Metric: "usd", Window: "5h", Remaining: ptr(30)},
	})); err != nil {
		t.Fatalf("合并观测失败：%v", err)
	}
	if got := *table.entries[key].baseline; got != 30 {
		t.Fatalf("恶化应立即生效，baseline=%v", got)
	}
}

func TestObservationImprovementNeedsConfirmation(t *testing.T) {
	clock := newTestClock()
	table := newTable(clock, Options{})
	if err := table.MergeDeclared(declaredDoc([]profile.Limit{accountQuota("5h", 100, 70)})); err != nil {
		t.Fatalf("合并声明失败：%v", err)
	}
	key := Key{Scope: Scope{Account: "acct"}, Metric: "usd", Window: "5h", Kind: "quota"}

	observe := func(value float64) {
		t.Helper()
		if err := table.MergeObserved(observedDoc(clock.Now(), []profile.Limit{
			{Kind: "quota", Metric: "usd", Window: "5h", Remaining: ptr(value)},
		})); err != nil {
			t.Fatalf("合并观测失败：%v", err)
		}
	}

	observe(80)
	if got := *table.entries[key].baseline; got != 30 {
		t.Fatalf("改善未经确认不得生效，baseline=%v", got)
	}
	observe(80)
	if got := *table.entries[key].baseline; got != 80 {
		t.Fatalf("改善应经一次实测确认后生效，baseline=%v", got)
	}
}

func TestObservationCannotCreateEntry(t *testing.T) {
	clock := newTestClock()
	table := newTable(clock, Options{})
	if err := table.MergeDeclared(declaredDoc([]profile.Limit{accountQuota("5h", 100, 0)})); err != nil {
		t.Fatalf("合并声明失败：%v", err)
	}
	before := len(table.entries)

	err := table.MergeObserved(observedDoc(clock.Now(), []profile.Limit{
		{Kind: "quota", Metric: "tokens", Window: "5h", Remaining: ptr(10)},
	}))
	if err == nil {
		t.Fatal("观测到未声明的条目应报错")
	}
	if !strings.Contains(err.Error(), "tokens") {
		t.Fatalf("报错未点明缺失的键：%v", err)
	}
	if got := len(table.entries); got != before {
		t.Fatalf("观测不得新建条目：合并前 %d 条，合并后 %d 条", before, got)
	}
}

func TestMissingRemainingDiffersFromZero(t *testing.T) {
	clock := newTestClock()

	missing := newTable(clock, Options{})
	if err := missing.MergeDeclared(declaredDoc([]profile.Limit{
		{Kind: "quota", Metric: "usd", Window: "absolute", Limit: ptr(10), Used: ptr(0)},
	})); err != nil {
		t.Fatalf("合并声明失败：%v", err)
	}
	if ok, reason := missing.Available(accountsScope(), "", clock.Now()); !ok {
		t.Fatalf("只剩 10 的余额应可用，reason=%+v", reason)
	}

	zero := newTable(clock, Options{})
	if err := zero.MergeDeclared(declaredDoc([]profile.Limit{
		{Kind: "quota", Metric: "usd", Window: "absolute", Limit: ptr(10), Remaining: ptr(0)},
	})); err != nil {
		t.Fatalf("合并声明失败：%v", err)
	}
	ok, reason := zero.Available(accountsScope(), "", clock.Now())
	if ok {
		t.Fatal("remaining: 0 必须判为不可用")
	}
	if reason.Verdict != VerdictQuotaExhausted {
		t.Fatalf("remaining: 0 的判定类别=%q，期望 %q", reason.Verdict, VerdictQuotaExhausted)
	}
	if !reason.RequiresClear {
		t.Fatal("absolute 条目触顶应要求显式清除")
	}
	if !reason.Until.IsZero() {
		t.Fatalf("absolute 条目触顶不得安排 until，得到 %v", reason.Until)
	}
}

func TestStaleObservationDegradesToEstimate(t *testing.T) {
	clock := newTestClock()
	table := newTable(clock, Options{TTL: time.Minute, TTLFactor: 3})
	if err := table.MergeDeclared(declaredDoc([]profile.Limit{accountQuota("5h", 100, 0)})); err != nil {
		t.Fatalf("合并声明失败：%v", err)
	}
	key := Key{Scope: Scope{Account: "acct"}, Metric: "usd", Window: "5h", Kind: "quota"}

	freshAt := clock.Now()
	if err := table.MergeObserved(observedDoc(freshAt, []profile.Limit{
		{Kind: "quota", Metric: "usd", Window: "5h", Remaining: ptr(50)},
	})); err != nil {
		t.Fatalf("合并观测失败：%v", err)
	}

	// 观测时刻比新鲜度上限 min(3×60s, 5h 的 10%) = 180s 还早 200s：该观测过期。
	clock.Advance(200 * time.Second)
	if err := table.MergeObserved(observedDoc(freshAt, []profile.Limit{
		{Kind: "quota", Metric: "usd", Window: "5h", Remaining: ptr(80)},
	})); err != nil {
		t.Fatalf("合并观测失败：%v", err)
	}
	if got := *table.entries[key].baseline; got != 50 {
		t.Fatalf("过期观测不得改写基准，baseline=%v", got)
	}
	if !table.entries[key].stale {
		t.Fatal("过期观测应把条目标记为降级")
	}

	ok, reason := table.Available(accountsScope(), "", clock.Now())
	if !ok {
		t.Fatalf("剩余量仍为正时应可用，reason=%+v", reason)
	}
	if !reason.Estimated {
		t.Fatal("降级状态必须在 Reason 里体现")
	}
}

func TestLearnExhaustedUntilExpiry(t *testing.T) {
	clock := newTestClock()
	table := newTable(clock, Options{})
	if err := table.MergeDeclared(declaredDoc([]profile.Limit{accountQuota("5h", 100, 0)})); err != nil {
		t.Fatalf("合并声明失败：%v", err)
	}

	until := clock.Now().Add(10 * time.Minute)
	table.LearnExhausted(accountsScope(), "glm", until)

	ok, reason := table.Available(accountsScope(), "glm", clock.Now())
	if ok {
		t.Fatal("错误学习后应不可用")
	}
	if reason.Verdict != VerdictQuotaExhausted {
		t.Fatalf("判定类别=%q，期望 %q", reason.Verdict, VerdictQuotaExhausted)
	}
	if !reason.Until.Equal(until) {
		t.Fatalf("until=%v，期望 %v", reason.Until, until)
	}

	ok, reason = table.Available(accountsScope(), "glm", until.Add(time.Second))
	if !ok {
		t.Fatalf("到期后应自动恢复，reason=%+v", reason)
	}
}

func TestLearnExhaustedNeedsDeclaredEntry(t *testing.T) {
	clock := newTestClock()
	table := newTable(clock, Options{})
	table.LearnExhausted(accountsScope(), "glm", clock.Now().Add(time.Minute))
	if len(table.blocks) != 0 {
		t.Fatal("没有任何已声明条目时，错误学习不得落标记")
	}
}

func TestRateExhaustionDoesNotWriteState(t *testing.T) {
	clock := newTestClock()
	table := newTable(clock, Options{})
	if err := table.MergeDeclared(declaredDoc([]profile.Limit{
		{Kind: "rate", Metric: "requests", Window: "1m", Limit: ptr(60), Remaining: ptr(0)},
	})); err != nil {
		t.Fatalf("合并声明失败：%v", err)
	}

	ok, reason := table.Available(accountsScope(), "", clock.Now())
	if ok {
		t.Fatal("rate 触顶时此刻应跳过")
	}
	if reason.Verdict != VerdictRateLimited {
		t.Fatalf("判定类别=%q，期望 %q", reason.Verdict, VerdictRateLimited)
	}
	if reason.Until.IsZero() {
		t.Fatal("rate 触顶的短退避必须带 until")
	}
	if len(table.blocks) != 0 {
		t.Fatalf("rate 触顶不得写账号状态，blocks=%v", table.blocks)
	}

	// 窗口切换后自动恢复，无需任何观测或显式清除。
	if ok, reason := table.Available(accountsScope(), "", clock.Now().Add(2*time.Minute)); !ok {
		t.Fatalf("rate 窗口切换后应恢复，reason=%+v", reason)
	}
}

func TestWindowRolloverClearsLocalUsage(t *testing.T) {
	clock := newTestClock()
	table := newTable(clock, Options{})
	if err := table.MergeDeclared(declaredDoc([]profile.Limit{
		{Kind: "quota", Metric: "tokens", Window: "5h", Limit: ptr(100), Used: ptr(0)},
	})); err != nil {
		t.Fatalf("合并声明失败：%v", err)
	}
	key := Key{Scope: Scope{Account: "acct"}, Metric: "tokens", Window: "5h", Kind: "quota"}

	table.Consume(accountsScope(), "", Usage{Tokens: 60}, clock.Now())
	if got := *table.entries[key].baseline - table.entries[key].localUsed; got != 40 {
		t.Fatalf("本地累计后剩余=%v，期望 40", got)
	}

	clock.Advance(5*time.Hour + time.Second)
	table.Consume(accountsScope(), "", Usage{Tokens: 10}, clock.Now())
	item := table.entries[key]
	if item.localUsed != 10 {
		t.Fatalf("窗口切换后本地累计应清零并只记新窗口的用量，localUsed=%v", item.localUsed)
	}
	if got := *item.baseline - item.localUsed; got != 90 {
		t.Fatalf("窗口切换后剩余=%v，期望 90", got)
	}
}

func TestNaturalWindowBoundaries(t *testing.T) {
	at := time.Date(2026, 9, 26, 18, 30, 0, 0, time.FixedZone("+08:00", 8*3600))

	day, err := parseWindow("day", "+08:00", "")
	if err != nil {
		t.Fatalf("解析 day 失败：%v", err)
	}
	if got := day.start(at); !got.Equal(time.Date(2026, 9, 26, 0, 0, 0, 0, at.Location())) {
		t.Fatalf("day 边界=%v", got)
	}

	month, err := parseWindow("month", "+08:00", "")
	if err != nil {
		t.Fatalf("解析 month 失败：%v", err)
	}
	if got := month.start(at); !got.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, at.Location())) {
		t.Fatalf("month 边界=%v", got)
	}

	week, err := parseWindow("week", "+08:00", "")
	if err != nil {
		t.Fatalf("解析 week 失败：%v", err)
	}
	weekStart := week.start(at)
	if weekStart.Weekday() != time.Monday {
		t.Fatalf("周应从周一开始，得到 %v", weekStart.Weekday())
	}
	if delta := at.Sub(weekStart); delta < 0 || delta >= 7*24*time.Hour {
		t.Fatalf("周起点 %v 不在 %v 所在周内", weekStart, at)
	}
}

// TestExpiredDeclaredDocIsUnavailable 守护到期的静态声明被剔除。
//
// expires_at 来自档案计划的到期时刻；未接上时该分支永远不可达，
// 到期的计划会继续被选中。
func TestExpiredDeclaredDocIsUnavailable(t *testing.T) {
	clock := newTestClock()

	expired := declaredDoc([]profile.Limit{accountQuota("5h", 70, 0)})
	expired.ExpiresAt = clock.Now().Add(-time.Hour).Format(time.RFC3339)
	table := newTable(clock, Options{})
	if err := table.MergeDeclared(expired); err != nil {
		t.Fatalf("合并声明失败：%v", err)
	}
	ok, reason := table.Available(accountsScope(), "", clock.Now())
	if ok || reason.Verdict != VerdictExpired {
		t.Fatalf("已到期的计划应不可用，ok=%v reason=%+v", ok, reason)
	}

	// 同一份限制，到期时刻在未来：额度充足时应可用。
	future := declaredDoc([]profile.Limit{accountQuota("5h", 70, 0)})
	future.ExpiresAt = clock.Now().Add(time.Hour).Format(time.RFC3339)
	later := newTable(clock, Options{})
	if err := later.MergeDeclared(future); err != nil {
		t.Fatalf("合并声明失败：%v", err)
	}
	if ok, reason := later.Available(accountsScope(), "", clock.Now()); !ok {
		t.Fatalf("未到期的计划应可用，reason=%+v", reason)
	}
}

// TestClearReleasesDeclaredAbsoluteExhaustion 守护显式清除能撤销 absolute 的静态判定。
//
// absolute 没有窗口可等，静态写的 remaining: 0 会一直挡住该账号；这正是
// 「没有自动恢复点」的那一类，必须有显式出口。
func TestClearReleasesDeclaredAbsoluteExhaustion(t *testing.T) {
	clock := newTestClock()
	table := newTable(clock, Options{})
	doc := declaredDoc([]profile.Limit{
		{Kind: "quota", Metric: "credits", Window: "absolute", Remaining: ptr(0)},
	})
	if err := table.MergeDeclared(doc); err != nil {
		t.Fatalf("合并声明失败：%v", err)
	}
	if ok, _ := table.Available(accountsScope(), "", clock.Now()); ok {
		t.Fatal("remaining: 0 的 absolute 声明应判为不可用")
	}

	table.Clear(accountsScope(), "")
	if ok, reason := table.Available(accountsScope(), "", clock.Now()); !ok {
		t.Fatalf("清除后应重新可用，reason=%+v", reason)
	}
}

// TestAccountingGapDegradesUntilObservation 守护记账缺口→估算态，且一次真实观测能清掉它。
func TestAccountingGapDegradesUntilObservation(t *testing.T) {
	clock := newTestClock()
	table := newTable(clock, Options{})
	if err := table.MergeDeclared(declaredDoc([]profile.Limit{accountQuota("5h", 100, 0)})); err != nil {
		t.Fatalf("合并声明失败：%v", err)
	}

	table.MarkAccountingGap("统计落库失败")
	ok, reason := table.Available(accountsScope(), "", clock.Now())
	if !ok {
		t.Fatalf("缺口下剩余量 100 经保守系数后仍应可用，reason=%+v", reason)
	}
	if !reason.Estimated {
		t.Fatal("记账缺口必须在 Reason 里体现为估算态")
	}
	if degraded, _ := table.Degraded(); !degraded {
		t.Fatal("记账缺口应让表报告降级")
	}

	observed := observedDoc(clock.Now(), []profile.Limit{
		{Kind: "quota", Metric: "usd", Window: "5h", Limit: ptr(100), Remaining: ptr(80)},
	})
	if err := table.MergeObserved(observed); err != nil {
		t.Fatalf("合并观测失败：%v", err)
	}
	ok, reason = table.Available(accountsScope(), "", clock.Now())
	if !ok || reason.Estimated {
		t.Fatalf("真实观测到达后应退出估算态，ok=%v reason=%+v", ok, reason)
	}
	if degraded, _ := table.Degraded(); degraded {
		t.Fatal("真实观测到达后不应再报告降级")
	}
}

// TestClearKeepsObservedBaseline 守护清除不动权威观测得到的基准。
//
// 观测代表上游实测，清除只针对「静态声明判为耗尽」；把观测也抹掉会把一个
// 的确实测到余额为 0 的账号重新放出去。
func TestClearKeepsObservedBaseline(t *testing.T) {
	clock := newTestClock()
	table := newTable(clock, Options{})
	doc := declaredDoc([]profile.Limit{
		{Kind: "quota", Metric: "credits", Window: "absolute", Limit: ptr(10)},
	})
	if err := table.MergeDeclared(doc); err != nil {
		t.Fatalf("合并声明失败：%v", err)
	}
	observed := observedDoc(clock.Now(), []profile.Limit{
		{Kind: "quota", Metric: "credits", Window: "absolute", Limit: ptr(10), Remaining: ptr(0)},
	})
	if err := table.MergeObserved(observed); err != nil {
		t.Fatalf("合并观测失败：%v", err)
	}
	if ok, _ := table.Available(accountsScope(), "", clock.Now()); ok {
		t.Fatal("观测到 remaining: 0 时应判为不可用")
	}

	table.Clear(accountsScope(), "")
	if ok, _ := table.Available(accountsScope(), "", clock.Now()); ok {
		t.Fatal("清除不得抹掉权威观测得到的基准")
	}
}

func TestParseWindowForms(t *testing.T) {
	duration, err := parseWindow("5h", "", "")
	if err != nil || duration.kind != windowDuration || duration.length != 5*time.Hour {
		t.Fatalf("滚动窗口解析错误：%+v err=%v", duration, err)
	}
	natural, err := parseWindow("month", "+08:00", "")
	if err != nil || natural.kind != windowNatural || natural.loc == nil {
		t.Fatalf("自然周期解析错误：%+v err=%v", natural, err)
	}
	absolute, err := parseWindow("absolute", "", "")
	if err != nil || absolute.kind != windowAbsolute {
		t.Fatalf("absolute 解析错误：%+v err=%v", absolute, err)
	}
}
