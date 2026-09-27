package limits

import (
	"testing"

	"github.com/sumwai/nova/internal/profile"
)

// TestFromDocRoundTrip 验证额度文档与中性声明可以互相转换而不丢字段。
func TestFromDocRoundTrip(t *testing.T) {
	doc := &profile.LimitsDoc{
		Schema: profile.CurrentSchema,
		Account: []profile.Limit{
			{
				Kind:      "quota",
				Metric:    "usd",
				Window:    "5h",
				TZ:        "+08:00",
				Anchor:    "1h",
				Limit:     ptr(14),
				Remaining: ptr(0),
				Unbounded: false,
				Assumed:   true,
				Pool:      "shared",
			},
			{
				Kind:     "rate",
				Metric:   "requests",
				Window:   "1m",
				Limit:    ptr(60),
				ResetsAt: "2026-09-26T15:00:00Z",
			},
		},
		Models: map[string][]profile.Limit{
			"glm-5.3": {{Kind: "quota", Metric: "tokens", Window: "5h", Limit: ptr(2000000)}},
		},
	}

	declared := FromDoc(doc)
	if len(declared) != 2 {
		t.Fatalf("账号级声明应转换出 2 条，实际 %d 条", len(declared))
	}
	// 只取账号级条目：模型级条目由观测带入，不在档案声明这一层表达。
	if declared[0].Pool != "shared" || declared[0].TZ != "+08:00" || declared[0].Anchor != "1h" {
		t.Errorf("首条声明丢字段：%+v", declared[0])
	}
	if declared[0].Remaining == nil || *declared[0].Remaining != 0 {
		t.Errorf("remaining: 0 必须保留为显式零值，实际 %+v", declared[0].Remaining)
	}
	if !declared[0].Assumed {
		t.Errorf("assumed 丢失")
	}

	back := DeclaredDoc(declared)
	if back == nil || len(back.Account) != 2 {
		t.Fatalf("还原后的账号级条目应为 2 条，实际 %+v", back)
	}
	if back.Account[1].ResetsAt != "2026-09-26T15:00:00Z" {
		t.Errorf("resets_at 丢失：%q", back.Account[1].ResetsAt)
	}
}

// TestFromDocCopiesPointers 验证转换不复用原文档的指针。
//
// 声明会随配置在多个渠道之间共享，复用指针会让一处改写影响另一处。
func TestFromDocCopiesPointers(t *testing.T) {
	original := ptr(10.0)
	declared := FromDoc(&profile.LimitsDoc{Account: []profile.Limit{{Limit: original}}})
	if len(declared) != 1 || declared[0].Limit == original {
		t.Fatalf("Limit 指针应被复制，不能与原文档共用")
	}
	*declared[0].Limit = 99
	if *original != 10 {
		t.Errorf("修改声明不应改动原文档，实际原值 %v", *original)
	}
}

// TestFromDocEmpty 验证空文档返回空声明。
func TestFromDocEmpty(t *testing.T) {
	if got := FromDoc(nil); got != nil {
		t.Errorf("nil 文档应返回 nil，实际 %+v", got)
	}
	if got := DeclaredDoc(nil); got != nil {
		t.Errorf("空声明应返回 nil 文档，实际 %+v", got)
	}
}

// TestDeclaredCarriesExpiresAt 守护文档级 expires_at 往返不丢。
//
// 它是「计划到期」在静态声明侧的载体，丢了会让到期的计划在装配后不再被剔除。
func TestDeclaredCarriesExpiresAt(t *testing.T) {
	doc := &profile.LimitsDoc{
		Schema:    profile.CurrentSchema,
		ExpiresAt: "2026-10-03T00:00:00Z",
		Account: []profile.Limit{
			{Kind: "quota", Metric: "usd", Window: "5h", Limit: ptr(10)},
			{Kind: "quota", Metric: "requests", Window: "5h", Limit: ptr(100)},
		},
	}

	declared := FromDoc(doc)
	for i, item := range declared {
		if item.ExpiresAt != doc.ExpiresAt {
			t.Errorf("第 %d 条声明的 expires_at = %q，期望 %q", i+1, item.ExpiresAt, doc.ExpiresAt)
		}
	}
	if back := DeclaredDoc(declared); back.ExpiresAt != doc.ExpiresAt {
		t.Errorf("还原后的 expires_at = %q，期望 %q", back.ExpiresAt, doc.ExpiresAt)
	}
}
