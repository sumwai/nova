package upstream

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/nova/internal/domain"
)

// TestUpstreamQuotaKeywordsCoverRequiredSemantics 守护关键词表的覆盖面：表一旦缩窄，
// 402 之外的额度耗尽就只能靠状态码猜测，而各平台的用词并不一致。
//
// 这里只列「本身就表示额度」的强词。insufficient/exceed/exhaust 这类弱词刻意不在表内，
// 它们会把权限错误或限流误判成额度耗尽，反向守护见 TestClassifyHTTPStatus 的
// 「403 权限错误」「429 限流标准文案」两例。
func TestUpstreamQuotaKeywordsCoverRequiredSemantics(t *testing.T) {
	required := []string{
		"quota", "balance", "credit", "billing",
		"额度", "配额", "余额", "欠费",
	}
	present := make(map[string]bool, len(upstreamQuotaKeywords))
	for _, keyword := range upstreamQuotaKeywords {
		present[keyword] = true
	}
	for _, keyword := range required {
		if !present[keyword] {
			t.Errorf("关键词表缺少 %q", keyword)
		}
	}
}

// TestClassifyHTTPStatus 覆盖上游非 2xx 的分级表，重点是额度耗尽与其余 4xx 的分界。
//
// 额度耗尽与上游限流都允许换渠道重试，与不可重试的上游拒绝分开，回退链才不会
// 在 402/403 表示额度耗尽时当场断掉。
func TestClassifyHTTPStatus(t *testing.T) {
	// 关键词落在截断片段之外的响应体：判定必须只依据 errorSnippet 返回的片段。
	beyondSnippet := strings.Repeat("x", errorDetailLimit) + "quota exhausted"

	tests := []struct {
		name          string
		status        int
		body          string
		wantCode      domain.Code
		wantRetryable bool
	}{
		{"402 一律额度耗尽", http.StatusPaymentRequired, "", domain.CodeUpstreamQuotaExhausted, true},
		{"403 命中额度关键词", http.StatusForbidden, `{"error":{"message":"insufficient quota"}}`, domain.CodeUpstreamQuotaExhausted, true},
		{"403 中文额度关键词", http.StatusForbidden, `{"message":"账户余额不足"}`, domain.CodeUpstreamQuotaExhausted, true},
		{"403 无额度关键词维持上游拒绝", http.StatusForbidden, `{"error":{"message":"model not allowed"}}`, domain.CodeUpstreamRejected, false},
		{"403 权限作用域不足不当作额度耗尽", http.StatusForbidden, `{"error":{"code":403,"message":"Request had insufficient authentication scopes.","status":"PERMISSION_DENIED"}}`, domain.CodeUpstreamRejected, false},
		{"403 OAuth 作用域不足不当作额度耗尽", http.StatusForbidden, `{"error":"insufficient_scope"}`, domain.CodeUpstreamRejected, false},
		{"429 命中额度关键词", http.StatusTooManyRequests, `{"error":"QUOTA EXCEEDED"}`, domain.CodeUpstreamQuotaExhausted, true},
		{"429 无额度关键词维持上游限流", http.StatusTooManyRequests, `{"error":"slow down"}`, domain.CodeUpstreamRateLimited, true},
		{"429 限流标准文案不当作额度耗尽", http.StatusTooManyRequests, `{"error":{"message":"Rate limit exceeded","type":"rate_limit_error"}}`, domain.CodeUpstreamRateLimited, true},
		{"429 rate_limit_exceeded 不当作额度耗尽", http.StatusTooManyRequests, `{"error":{"code":"rate_limit_exceeded"}}`, domain.CodeUpstreamRateLimited, true},
		{"429 关键词在截断片段之外仍按上游限流", http.StatusTooManyRequests, beyondSnippet, domain.CodeUpstreamRateLimited, true},
		{"400 维持上游拒绝", http.StatusBadRequest, `{"error":"bad request"}`, domain.CodeUpstreamRejected, false},
		{"404 维持上游拒绝", http.StatusNotFound, `{"error":"no such model"}`, domain.CodeUpstreamRejected, false},
		{"500 维持上游不可用", http.StatusInternalServerError, `boom`, domain.CodeUpstreamUnavailable, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := classifyHTTPStatus(tt.status, http.Header{}, []byte(tt.body), nil)
			domainErr := domain.AsError(err)
			if domainErr == nil {
				t.Fatalf("错误必须是 domain.Error，实际类型为 %T", err)
			}
			if domainErr.Code != tt.wantCode {
				t.Errorf("错误码 = %q，期望 %q", domainErr.Code, tt.wantCode)
			}
			if got := domain.Retryable(err); got != tt.wantRetryable {
				t.Errorf("domain.Retryable = %v，期望 %v", got, tt.wantRetryable)
			}
		})
	}
}

// TestClassifyHTTPStatusHonorsProfileMapping 守护档案声明优先于状态码启发式。
//
// 声明命中时不再看关键词：平台写下的对应关系是事实，通用启发式不得盖过它。
func TestClassifyHTTPStatusHonorsProfileMapping(t *testing.T) {
	rules := []domain.LimitsRule{
		{Status: http.StatusTooManyRequests, MatchBody: "quota|exhaust", Class: domain.LimitsWindowExhausted},
		{Status: http.StatusTooManyRequests, Class: domain.LimitsTransientRate},
		{Status: http.StatusForbidden, Class: domain.LimitsPermanent},
	}
	tests := []struct {
		name          string
		status        int
		body          string
		wantCode      domain.Code
		wantRetryable bool
	}{
		{"429 命中额度正则归窗口耗尽", http.StatusTooManyRequests, `{"error":"quota exceeded"}`, domain.CodeUpstreamQuotaExhausted, true},
		{"429 未命中正则归秒级限流", http.StatusTooManyRequests, `{"error":"slow down"}`, domain.CodeUpstreamRateLimited, true},
		{"403 声明为永久拒绝时不再看关键词", http.StatusForbidden, `{"message":"账户余额不足"}`, domain.CodeUpstreamRejected, false},
		{"未声明的 402 仍按过渡启发式", http.StatusPaymentRequired, ``, domain.CodeUpstreamQuotaExhausted, true},
		{"未声明的 400 仍按过渡启发式", http.StatusBadRequest, `{"error":"bad"}`, domain.CodeUpstreamRejected, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := classifyHTTPStatus(tt.status, http.Header{}, []byte(tt.body), rules)
			domainErr := domain.AsError(err)
			if domainErr == nil {
				t.Fatalf("错误必须是 domain.Error，实际类型为 %T", err)
			}
			if domainErr.Code != tt.wantCode {
				t.Errorf("错误码 = %q，期望 %q", domainErr.Code, tt.wantCode)
			}
			if got := domain.Retryable(err); got != tt.wantRetryable {
				t.Errorf("domain.Retryable = %v，期望 %v", got, tt.wantRetryable)
			}
		})
	}
}

// TestClassifyQuotaExhaustedRetainsRetryAfter 守护「额度耗尽」的 Retry-After 能力：
// retryAfterError 的包装不得改变新错误码的分级与可重试性，同时该提示要能被调用方取出。
func TestClassifyQuotaExhaustedRetainsRetryAfter(t *testing.T) {
	header := http.Header{"Retry-After": []string{"5"}}
	err := classifyHTTPStatus(http.StatusPaymentRequired, header, []byte(`{"error":"payment required"}`), nil)
	if got := domain.AsError(err); got == nil || got.Code != domain.CodeUpstreamQuotaExhausted {
		t.Fatalf("错误码 = %v，期望 %q", got, domain.CodeUpstreamQuotaExhausted)
	}
	if !domain.Retryable(err) {
		t.Errorf("额度耗尽错误应可重试")
	}
	retryAfter, ok := err.(interface{ RetryAfter() (time.Duration, bool) })
	if !ok {
		t.Fatalf("错误应携带 Retry-After 能力，实际类型为 %T", err)
	}
	if delay, _ := retryAfter.RetryAfter(); delay != 5*time.Second {
		t.Errorf("Retry-After = %v，期望 5s", delay)
	}
}
