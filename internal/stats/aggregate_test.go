package stats

import (
	"testing"
	"time"

	"github.com/sumwai/nova/internal/domain"
)

// testMeta 造一份聚合所需的元信息：时刻固定在 testNow，累计与保留期取确定值。
func testMeta() reportMeta {
	return reportMeta{
		now:             testNow(),
		startedAt:       testNow().Add(-time.Hour),
		accountingSince: testNow().Add(-24 * time.Hour),
		retention:       DefaultRetention,
		persisted:       true,
	}
}

func requestWith(model string, status int, code string, attempts ...AttemptSummary) Request {
	return Request{
		Time:      testNow().Add(-time.Minute),
		RequestID: "req-" + model,
		Protocol:  "openai_chat",
		Model:     model,
		Status:    status,
		ErrorCode: code,
		Client:    "claude-cli",
		Attempts:  attempts,
	}
}

func okAttempt(provider string, usage domain.Usage) AttemptSummary {
	return AttemptSummary{
		Provider:   provider,
		Upstream:   provider + " api.example.com",
		Outcome:    string(domain.AttemptOK),
		DurationMS: 5,
		Usage:      usage,
	}
}

func failedAttempt(provider string, durationMS int64) AttemptSummary {
	return AttemptSummary{
		Provider:   provider,
		Upstream:   provider + " api.example.com",
		Outcome:    string(domain.AttemptFailed),
		ErrorCode:  "upstream_unavailable",
		DurationMS: durationMS,
	}
}

// 回退跨渠道时：一次请求进多个 provider 桶，因此各桶 requests 之和大于窗口请求数；
// 而 attempts 之和恒等于窗口尝试数。两条不变量都要成立。
func TestProviderBreakdownIsAttemptScoped(t *testing.T) {
	record := requestWith("gpt-5", 200, "",
		failedAttempt("relay-a", 4000),
		okAttempt("relay-b", upstreamUsage(10, 20)),
	)
	report := buildReport([]Request{record}, testMeta(), Query{GroupBy: []string{"provider"}})

	if report.Window.Totals.Requests != 1 {
		t.Fatalf("窗口请求数 = %d，期望 1", report.Window.Totals.Requests)
	}
	if report.Window.Totals.Attempts != 2 {
		t.Fatalf("窗口尝试数 = %d，期望 2", report.Window.Totals.Attempts)
	}

	buckets := report.Window.Breakdowns.ByProvider
	if len(buckets) != 2 {
		t.Fatalf("渠道桶数 = %d，期望 2", len(buckets))
	}

	var requests, attempts, failed int
	for _, bucket := range buckets {
		requests += bucket.Requests
		attempts += bucket.Attempts
		failed += bucket.Failed
	}
	if attempts != report.Window.Totals.Attempts {
		t.Errorf("各渠道尝试数之和 = %d，期望等于窗口尝试数 %d", attempts, report.Window.Totals.Attempts)
	}
	if requests <= report.Window.Totals.Requests {
		t.Errorf("回退跨渠道时各渠道请求数之和 = %d，应大于窗口请求数 %d", requests, report.Window.Totals.Requests)
	}
	if failed != 1 {
		t.Errorf("失败尝试数 = %d，期望 1", failed)
	}
	// 降序：失败的 relay-a 与成功的 relay-b 请求数并列，按尝试数再排也并列，
	// 最终按键名，因此顺序确定而不是随 map 遍历漂移。
	if buckets[0].Provider != "relay-a" || buckets[1].Provider != "relay-b" {
		t.Errorf("渠道顺序 = %s / %s，期望 relay-a / relay-b", buckets[0].Provider, buckets[1].Provider)
	}
}

// 请求级轴：各桶请求数之和等于窗口请求数。
func TestRequestBreakdownsSumToTotals(t *testing.T) {
	records := []Request{
		requestWith("gpt-5", 200, "", okAttempt("relay-a", upstreamUsage(1, 1))),
		requestWith("gpt-5", 502, "upstream_unavailable", failedAttempt("relay-a", 100)),
		requestWith("gpt-4", 200, "", okAttempt("relay-b", upstreamUsage(1, 1))),
	}
	report := buildReport(records, testMeta(), Query{GroupBy: []string{"model", "client", "status", "protocol"}})

	modelRequests := 0
	for _, bucket := range report.Window.Breakdowns.ByModel {
		modelRequests += bucket.Requests
	}
	if modelRequests != report.Window.Totals.Requests {
		t.Errorf("按模型分组之和 = %d，期望 %d", modelRequests, report.Window.Totals.Requests)
	}
	if len(report.Window.Breakdowns.ByErrorCode) != 0 {
		t.Errorf("未要求的 error_code 轴不应产出分桶，实际 %d 个", len(report.Window.Breakdowns.ByErrorCode))
	}

	// 未分组的错误码桶只计失败请求，因此各桶之和等于失败数。
	errorCodes := buildReport(records, testMeta(), Query{GroupBy: []string{"error_code"}})
	total := 0
	for _, bucket := range errorCodes.Window.Breakdowns.ByErrorCode {
		total += bucket.Requests
	}
	if total != errorCodes.Window.Totals.Failed {
		t.Errorf("各错误码请求数之和 = %d，期望等于失败数 %d", total, errorCodes.Window.Totals.Failed)
	}
}

func TestBreakdownTruncatesAndReportsAxis(t *testing.T) {
	records := make([]Request, 0, maxBreakdownBuckets+10)
	for i := 0; i < maxBreakdownBuckets+10; i++ {
		records = append(records, requestWith("model-"+string(rune('A'+i%26))+string(rune('a'+i/26)), 200, ""))
	}
	report := buildReport(records, testMeta(), Query{GroupBy: []string{"model"}})

	if len(report.Window.Breakdowns.ByModel) != maxBreakdownBuckets {
		t.Errorf("模型桶数 = %d，期望截断到 %d", len(report.Window.Breakdowns.ByModel), maxBreakdownBuckets)
	}
	found := false
	for _, axis := range report.Window.TruncatedAxes {
		if axis == "by_model" {
			found = true
		}
	}
	if !found {
		t.Errorf("截断轴 = %v，期望含 by_model", report.Window.TruncatedAxes)
	}
}

func TestDetailReturnsNewestFirstWithLimit(t *testing.T) {
	records := []Request{
		{Time: testNow().Add(-2 * time.Minute), RequestID: "old"},
		{Time: testNow().Add(-1 * time.Minute), RequestID: "mid"},
		{Time: testNow(), RequestID: "new"},
	}
	report := buildReport(records, testMeta(), Query{Detail: true, Limit: 2})
	if len(report.Window.Requests) != 2 {
		t.Fatalf("明细条数 = %d，期望 2", len(report.Window.Requests))
	}
	if report.Window.Requests[0].RequestID != "new" || report.Window.Requests[1].RequestID != "mid" {
		t.Errorf("明细顺序 = %s / %s，期望 new / mid",
			report.Window.Requests[0].RequestID, report.Window.Requests[1].RequestID)
	}
}

func TestHistogramQuantiles(t *testing.T) {
	var h histogram
	for i := 0; i < 100; i++ {
		h.add(int64(i + 1))
	}
	view := h.view()
	if view.P50 != 50 {
		t.Errorf("p50 = %d，期望 50", view.P50)
	}
	if view.P99 != 100 {
		t.Errorf("p99 = %d，期望 100", view.P99)
	}
	if view.Max != 100 {
		t.Errorf("max = %d，期望 100", view.Max)
	}

	var overflow histogram
	overflow.add(120000)
	if got := overflow.view().P99; got != 120000 {
		t.Errorf("溢出桶 p99 = %d，期望回落到最大值 120000", got)
	}

	var empty histogram
	if got := empty.view(); got != (LatencyView{}) {
		t.Errorf("空直方图 = %+v，期望零值", got)
	}
}

// 分位数不得大于最大值：桶上界会高估，而 p50 > max 看起来像算错了。
func TestHistogramQuantileNeverExceedsMax(t *testing.T) {
	var zero histogram
	zero.add(0)
	zero.add(0)
	if view := zero.view(); view.P50 > view.Max {
		t.Errorf("p50 = %d 大于 max = %d", view.P50, view.Max)
	}

	var single histogram
	single.add(40)
	view := single.view()
	if view.P50 != 40 {
		t.Errorf("p50 = %d，期望钳到 max=40", view.P50)
	}
}
