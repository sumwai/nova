package limits

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/sumwai/nova/internal/profile"
)

// findAccountLimit 按 metric 找到账号级条目。
func findAccountLimit(doc *profile.LimitsDoc, metric string) *profile.Limit {
	for i := range doc.Account {
		if doc.Account[i].Metric == metric {
			return &doc.Account[i]
		}
	}
	return nil
}

func TestParseHeadersOpenAI(t *testing.T) {
	h := http.Header{}
	h.Set("x-ratelimit-limit-requests", "60")
	h.Set("x-ratelimit-remaining-requests", "42")
	h.Set("x-ratelimit-reset-requests", "1s")
	h.Set("x-ratelimit-limit-tokens", "100000")
	h.Set("x-ratelimit-remaining-tokens", "90000")
	h.Set("x-ratelimit-reset-tokens", "6m0s")

	doc, err := ParseHeaders(h)
	if err != nil {
		t.Fatalf("OpenAI 响应头应完整解析：%v", err)
	}
	if doc.Source != "headers" || doc.ObservedAt == "" {
		t.Fatalf("文档来源与观测时刻缺失：%+v", doc)
	}
	requests := findAccountLimit(doc, "requests")
	if requests == nil || requests.Kind != "rate" || requests.Window != "1m" {
		t.Fatalf("requests 条目解析错误：%+v", requests)
	}
	if requests.Remaining == nil || *requests.Remaining != 42 {
		t.Fatalf("requests remaining 解析错误：%+v", requests.Remaining)
	}
	if requests.Limit == nil || *requests.Limit != 60 {
		t.Fatalf("requests limit 解析错误：%+v", requests.Limit)
	}
	at, err := time.Parse(time.RFC3339, requests.ResetsAt)
	if err != nil {
		t.Fatalf("requests resets_at 不是绝对时刻：%q", requests.ResetsAt)
	}
	if delay := time.Until(at); delay < 0 || delay > 10*time.Second {
		t.Fatalf("requests resets_at 距现在 %v，期望约 1s", delay)
	}

	tokens := findAccountLimit(doc, "tokens")
	if tokens == nil || tokens.Remaining == nil || *tokens.Remaining != 90000 {
		t.Fatalf("tokens 条目解析错误：%+v", tokens)
	}
}

func TestParseHeadersAnthropic(t *testing.T) {
	reset := "2026-09-26T15:00:00Z"
	h := http.Header{}
	h.Set("anthropic-ratelimit-requests-limit", "1000")
	h.Set("anthropic-ratelimit-requests-remaining", "999")
	h.Set("anthropic-ratelimit-requests-reset", reset)
	h.Set("anthropic-ratelimit-tokens-remaining", "500")

	doc, err := ParseHeaders(h)
	if err != nil {
		t.Fatalf("Anthropic 响应头应完整解析：%v", err)
	}
	requests := findAccountLimit(doc, "requests")
	if requests == nil || requests.Remaining == nil || *requests.Remaining != 999 {
		t.Fatalf("requests 条目解析错误：%+v", requests)
	}
	if requests.ResetsAt != reset {
		t.Fatalf("Anthropic 的 resets_at 应原样保留绝对时刻：%q", requests.ResetsAt)
	}
	tokens := findAccountLimit(doc, "tokens")
	if tokens == nil || tokens.Remaining == nil || *tokens.Remaining != 500 {
		t.Fatalf("tokens 条目解析错误：%+v", tokens)
	}
	if tokens.Limit != nil {
		t.Fatalf("未给出的 limit 不应被填值：%v", *tokens.Limit)
	}
}

func TestParseHeadersRetryAfter(t *testing.T) {
	h := http.Header{}
	h.Set("x-ratelimit-remaining-requests", "10")
	h.Set("retry-after", "30")

	doc, err := ParseHeaders(h)
	if err != nil {
		t.Fatalf("retry-after 应能解析：%v", err)
	}
	if len(doc.Account) != 1 {
		t.Fatalf("retry-after 应覆盖同键条目而不是新增一条：%d 条", len(doc.Account))
	}
	requests := findAccountLimit(doc, "requests")
	if requests.Remaining == nil || *requests.Remaining != 0 {
		t.Fatalf("retry-after 应把剩余量归零：%+v", requests.Remaining)
	}
	at, err := time.Parse(time.RFC3339, requests.ResetsAt)
	if err != nil {
		t.Fatalf("retry-after 的 resets_at 不是绝对时刻：%q", requests.ResetsAt)
	}
	if delay := time.Until(at); delay < 25*time.Second || delay > 35*time.Second {
		t.Fatalf("retry-after 退避 %v，期望约 30s", delay)
	}
}

func TestParseHeadersRetryAfterHTTPDate(t *testing.T) {
	h := http.Header{}
	h.Set("retry-after", time.Now().Add(40*time.Second).UTC().Format(http.TimeFormat))

	doc, err := ParseHeaders(h)
	if err != nil {
		t.Fatalf("HTTP 日期形态的 retry-after 应能解析：%v", err)
	}
	requests := findAccountLimit(doc, "requests")
	if requests == nil || requests.Remaining == nil || *requests.Remaining != 0 {
		t.Fatalf("retry-after 应产出 requests 条目：%+v", requests)
	}
	at, _ := time.Parse(time.RFC3339, requests.ResetsAt)
	if delay := time.Until(at); delay < 30*time.Second || delay > 50*time.Second {
		t.Fatalf("HTTP 日期形态的退避 %v，期望约 40s", delay)
	}
}

func TestParseHeadersBadValueWarnsButKeepsRest(t *testing.T) {
	h := http.Header{}
	h.Set("x-ratelimit-limit-requests", "很多")
	h.Set("x-ratelimit-remaining-requests", "5")
	h.Set("x-ratelimit-reset-requests", "一会儿")

	doc, err := ParseHeaders(h)
	var warn *Warn
	if !errors.As(err, &warn) {
		t.Fatalf("解析失败应返回 Warn，得到 %v", err)
	}
	if len(warn.Msgs) != 2 {
		t.Fatalf("应记录两条告警，得到 %v", warn.Msgs)
	}
	requests := findAccountLimit(doc, "requests")
	if requests == nil || requests.Remaining == nil || *requests.Remaining != 5 {
		t.Fatalf("可用的部分应照常返回：%+v", requests)
	}
}

func TestParseHeadersIgnoresUnknownHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("x-some-unknown-header", "1")
	doc, err := ParseHeaders(h)
	if err != nil {
		t.Fatalf("认不出的头应忽略：%v", err)
	}
	if len(doc.Account) != 0 {
		t.Fatalf("认不出的头不应产出条目：%+v", doc.Account)
	}
}
