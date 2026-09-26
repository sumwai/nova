package profile

import (
	"strings"
	"testing"
)

// validLimits 是一份覆盖两处作用域与三种窗口形态的合法额度文档。
const validLimits = `schema: 1
observed_at: 2026-09-26T10:00:00Z
source: exec
expires_at: 2026-10-03T00:00:00Z
account:
  - { kind: quota, metric: usd, window: month, tz: "+08:00", limit: 70, used: 30 }
  - { kind: quota, metric: usd, window: 5h, limit: 14, remaining: 5.2, resets_at: 2026-09-26T15:00:00Z }
  - { kind: rate, metric: requests, window: 1m, limit: 60, remaining: 42 }
  - { kind: quota, metric: credits, window: absolute, unbounded: true }
models:
  glm-5.3:
    - { kind: quota, metric: tokens, window: 5h, limit: 2000000, remaining: 900000 }
`

func TestLoadLimitsAcceptsValidDocument(t *testing.T) {
	doc, err := LoadLimits("limits.yaml", []byte(validLimits))
	if err != nil {
		t.Fatalf("合法文档被拒绝：%v", err)
	}
	if doc.Schema != 1 || doc.Source != "exec" || doc.ObservedAt == "" || doc.ExpiresAt == "" {
		t.Fatalf("顶层字段解析不完整：%+v", doc)
	}
	if len(doc.Account) != 4 {
		t.Fatalf("account 条目数=%d，期望 4", len(doc.Account))
	}
	if got := doc.Models["glm-5.3"]; len(got) != 1 || got[0].Metric != "tokens" {
		t.Fatalf("models 条目解析错误：%+v", doc.Models)
	}
	// 只给 limit 与 used 的条目：remaining 必须保持缺失，由 limit-used 推出。
	if doc.Account[0].Remaining != nil {
		t.Fatalf("未给出的 remaining 不应被填值：%v", *doc.Account[0].Remaining)
	}
	if doc.Account[0].Limit == nil || *doc.Account[0].Limit != 70 {
		t.Fatalf("limit 解析错误：%+v", doc.Account[0].Limit)
	}
}

func TestLoadLimitsDistinguishesMissingFromZero(t *testing.T) {
	missing := `schema: 1
account:
  - { kind: quota, metric: usd, window: absolute, limit: 10, used: 0 }
`
	doc, err := LoadLimits("limits.yaml", []byte(missing))
	if err != nil {
		t.Fatalf("文档被拒绝：%v", err)
	}
	// used: 0 是显式零值，remaining 缺失：两者必须分开。
	if doc.Account[0].Remaining != nil {
		t.Fatalf("缺失的 remaining 被填成 %v", *doc.Account[0].Remaining)
	}
	if doc.Account[0].Used == nil || *doc.Account[0].Used != 0 {
		t.Fatalf("used: 0 应被解析为显式的零值：%+v", doc.Account[0].Used)
	}

	zero := `schema: 1
account:
  - { kind: quota, metric: usd, window: absolute, limit: 10, remaining: 0 }
`
	doc, err = LoadLimits("limits.yaml", []byte(zero))
	if err != nil {
		t.Fatalf("文档被拒绝：%v", err)
	}
	if doc.Account[0].Remaining == nil || *doc.Account[0].Remaining != 0 {
		t.Fatalf("remaining: 0 应被解析为显式的零值：%+v", doc.Account[0].Remaining)
	}
}

func TestLoadLimitsRejectsNaturalWindowWithoutTZ(t *testing.T) {
	doc := `schema: 1
account:
  - { kind: quota, metric: usd, window: month, limit: 70, used: 30 }
`
	_, err := LoadLimits("limits.yaml", []byte(doc))
	if err == nil {
		t.Fatal("自然周期缺 tz 应被拒绝")
	}
	if !strings.Contains(err.Error(), "limits.yaml:3") {
		t.Fatalf("报错未指向具体行：%v", err)
	}
}

func TestLoadLimitsRejectsAbsoluteWithResetsAt(t *testing.T) {
	doc := `schema: 1
account:
  - { kind: quota, metric: usd, window: absolute, remaining: 0, resets_at: 2026-09-26T15:00:00Z }
`
	_, err := LoadLimits("limits.yaml", []byte(doc))
	if err == nil {
		t.Fatal("absolute 携带 resets_at 应被拒绝")
	}
	if !strings.Contains(err.Error(), "resets_at") {
		t.Fatalf("报错未点明字段：%v", err)
	}
	if !strings.Contains(err.Error(), "limits.yaml:3") {
		t.Fatalf("报错未指向具体行：%v", err)
	}
}

func TestLoadLimitsRejectsInvalidTimestamp(t *testing.T) {
	doc := `schema: 1
observed_at: 昨天
account:
  - { kind: quota, metric: usd, window: absolute, remaining: 1 }
`
	_, err := LoadLimits("limits.yaml", []byte(doc))
	if err == nil {
		t.Fatal("非法时刻应被拒绝")
	}
	if !strings.Contains(err.Error(), "observed_at") {
		t.Fatalf("报错未点明字段：%v", err)
	}
}

func TestLoadLimitsRejectsUndeterminableRemaining(t *testing.T) {
	doc := `schema: 1
account:
  - { kind: quota, metric: usd, window: absolute, used: 3 }
`
	_, err := LoadLimits("limits.yaml", []byte(doc))
	if err == nil {
		t.Fatal("只给 used 无法确定 remaining，应被拒绝")
	}
}

func TestParseLimitsYAMLRejectsYAMLFeatures(t *testing.T) {
	anchored := "schema: 1\naccount: &a []\nmodels:\n  m: *a\n"
	if _, err := ParseLimitsYAML([]byte(anchored)); err == nil {
		t.Fatal("额度文档不得使用 YAML 锚点与别名")
	}
}
