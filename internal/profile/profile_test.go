package profile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// validProfile 是一份覆盖主要字段的合法档案：认证头、两条端点、两个模型（其中一个用
// price_from 引用另一个）、探测、带静态限制的计划与错误映射。
const validProfile = `$schema: https://nova.dev/schema/profile-1.yaml
schema: 1
id: opencode-go
name: OpenCode Go
updated_at: 2026-09-26T00:00:00Z
docs:
  - https://opencode.ai/docs/go/
api_pattern: '^https://opencode\.ai/zen/go/'
auth:
  header: authorization
  scheme: Bearer
  env: OPENCODE_API_KEY
headers:
  x-nova-profile: opencode-go
endpoints:
  openai_chat:
    url: https://opencode.ai/zen/go/v1/chat/completions
    protocol: openai_chat
  anthropic_messages:
    url: https://opencode.ai/zen/go/v1/messages
    protocol: anthropic_messages
models:
  - id: deepseek-flash
    public: deepseek-flash
    endpoints:
      - openai_chat
    price:
      currency: USD
      input_mtok: 0.28
      output_mtok: 0.42
  - id: kimi-k3
    endpoints:
      - openai_chat
    price_from: deepseek-flash
usage:
  probe: opencode-go
  interval: 60s
  schema: limits-1
plans:
  - id: go
    paid:
      amount: 10
      currency: USD
      period: month
    limits:
      - kind: quota
        metric: usd
        window: month
        tz: "+08:00"
        limit: 70
      - kind: quota
        metric: usd
        window: 5h
        limit: 14
      - kind: rate
        metric: requests
        window: 1m
        limit: 60
limits_mapping:
  - status: 429
    match_body: "quota|exhaust|insufficient"
    class: window-exhausted
  - status: 429
    class: transient-rate
  - status: 402
    class: window-exhausted
  - status: 403
    class: permanent
`

// minimalProfile 是只含必填字段的合法档案，供拒绝用例作为基底。
const minimalProfile = `schema: 1
id: sample
endpoints:
  openai_chat:
    url: https://example.com/v1/chat/completions
    protocol: openai_chat
`

// writeProfile 把档案内容落到临时文件并返回路径。
func writeProfile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "profile.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写入测试档案失败：%v", err)
	}
	return path
}

// requireLocatedError 断言 err 是一条带文件路径与行号的档案错误，且文案含指定片段。
func requireLocatedError(t *testing.T, path string, err error, wantSubstrings ...string) *Error {
	t.Helper()
	if err == nil {
		t.Fatalf("期望加载失败，实际成功")
	}
	var profileErr *Error
	if !errors.As(err, &profileErr) {
		t.Fatalf("错误必须是 *profile.Error，实际类型为 %T", err)
	}
	if profileErr.File != path {
		t.Errorf("错误路径 = %q，期望 %q", profileErr.File, path)
	}
	if profileErr.Line <= 0 {
		t.Errorf("错误必须带行号，实际 Line = %d（错误：%s）", profileErr.Line, profileErr)
	}
	if !strings.Contains(profileErr.Error(), path+":") {
		t.Errorf("错误文本应含「%s:」，实际为 %q", path, profileErr.Error())
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(profileErr.Msg, want) {
			t.Errorf("错误文案 %q 应含 %q", profileErr.Msg, want)
		}
	}
	return profileErr
}

func TestLoadValidProfile(t *testing.T) {
	path := writeProfile(t, validProfile)
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("合法档案应通过，实际失败：%v", err)
	}
	if loaded.ID != "opencode-go" {
		t.Errorf("id = %q，期望 %q", loaded.ID, "opencode-go")
	}
	if loaded.Schema != 1 {
		t.Errorf("schema = %d，期望 1", loaded.Schema)
	}
	if len(loaded.Endpoints) != 2 || len(loaded.Models) != 2 {
		t.Errorf("端点 %d 条、模型 %d 条，期望各 2 条", len(loaded.Endpoints), len(loaded.Models))
	}
	if loaded.Usage == nil || loaded.Usage.Probe != "opencode-go" {
		t.Errorf("usage.probe = %+v，期望 opencode-go", loaded.Usage)
	}
	if len(loaded.Plans) != 1 || len(loaded.Plans[0].Limits) != 3 {
		t.Fatalf("计划与静态限制解析不完整：%+v", loaded.Plans)
	}
	if got := loaded.Plans[0].Limits[0].Limit; got == nil || *got != 70 {
		t.Errorf("首条限制的 limit = %v，期望 70", got)
	}
	if got := loaded.Plans[0].Limits[0].Remaining; got != nil {
		t.Errorf("静态限制未写 remaining，应为 nil，实际为 %v", *got)
	}
}

func TestLoadRejectsMissingRequiredField(t *testing.T) {
	// 去掉 id：schema 的 required 列表应报出缺失字段。
	content := strings.Replace(minimalProfile, "id: sample\n", "", 1)
	path := writeProfile(t, content)
	_, err := Load(path)
	requireLocatedError(t, path, err, "缺少必填字段", `"id"`)
}

func TestLoadRejectsUnknownField(t *testing.T) {
	content := "schema: 1\nid: sample\nunknown_field: true\nendpoints:\n  openai_chat:\n    url: https://example.com/v1/chat/completions\n    protocol: openai_chat\n"
	path := writeProfile(t, content)
	_, err := Load(path)
	requireLocatedError(t, path, err, "未知字段", "unknown_field")
}

func TestLoadRejectsTooNewSchemaGeneration(t *testing.T) {
	content := "schema: 2\nid: sample\nendpoints:\n  openai_chat:\n    url: https://example.com/v1/chat/completions\n    protocol: openai_chat\n"
	path := writeProfile(t, content)
	_, err := Load(path)
	requireLocatedError(t, path, err, "档案格式代数 2", "更高的代数", "请升级 nova")
}

func TestLoadRejectsAnchor(t *testing.T) {
	content := "schema: 1\nid: sample\nendpoints:\n  openai_chat: &base\n    url: https://example.com/v1/chat/completions\n    protocol: openai_chat\n  anthropic_messages: *base\n"
	path := writeProfile(t, content)
	_, err := Load(path)
	requireLocatedError(t, path, err, "锚点")
}

func TestLoadRejectsDuplicateKey(t *testing.T) {
	content := "schema: 1\nid: sample\nid: other\nendpoints:\n  openai_chat:\n    url: https://example.com/v1/chat/completions\n    protocol: openai_chat\n"
	path := writeProfile(t, content)
	_, err := Load(path)
	requireLocatedError(t, path, err, "重复")
}

func TestLoadRejectsMultipleDocuments(t *testing.T) {
	content := minimalProfile + "---\nschema: 1\nid: second\nendpoints:\n  openai_chat:\n    url: https://example.com/v1/chat/completions\n    protocol: openai_chat\n"
	path := writeProfile(t, content)
	_, err := Load(path)
	requireLocatedError(t, path, err, "一个 YAML 文档")
}

func TestLoadRejectsDanglingEndpointReference(t *testing.T) {
	content := minimalProfile + "models:\n  - id: a\n    endpoints: [missing_endpoint]\n"
	path := writeProfile(t, content)
	_, err := Load(path)
	requireLocatedError(t, path, err, "不存在的端点", "missing_endpoint")
}

// TestLoadAcceptsExternalPriceFrom 守护 price_from 不因「档案内不存在同名条目」被拒：
// 设计文档 §3.3 的 price_from 可以是 `namespace/model` 形式的外部价格表引用。
func TestLoadAcceptsExternalPriceFrom(t *testing.T) {
	content := minimalProfile + "models:\n  - id: a\n    price_from: deepseek/deepseek-v4-flash\n"
	path := writeProfile(t, content)
	if _, err := Load(path); err != nil {
		t.Fatalf("外部价格表引用应通过，实际失败：%v", err)
	}
}

func TestLoadRejectsUncompilableAPIPattern(t *testing.T) {
	content := "schema: 1\nid: sample\napi_pattern: '(['\nendpoints:\n  openai_chat:\n    url: https://example.com/v1/chat/completions\n    protocol: openai_chat\n"
	path := writeProfile(t, content)
	_, err := Load(path)
	requireLocatedError(t, path, err, "api_pattern", "正则")
}

// TestPlanExpiresAt 守护计划到期时刻是带时区的绝对时刻，且能被解析读出。
func TestPlanExpiresAt(t *testing.T) {
	content := minimalProfile + `plans:
  - id: trial
    expires_at: 2026-10-03T00:00:00Z
    limits:
      - kind: quota
        metric: usd
        window: 5h
        limit: 10
`
	path := writeProfile(t, content)
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("带 expires_at 的计划应通过，实际失败：%v", err)
	}
	if got := loaded.Plans[0].ExpiresAt; got != "2026-10-03T00:00:00Z" {
		t.Errorf("expires_at = %q，期望原样保留", got)
	}

	bad := minimalProfile + `plans:
  - id: trial
    expires_at: 明天
    limits:
      - kind: quota
        metric: usd
        window: 5h
        limit: 10
`
	badPath := writeProfile(t, bad)
	_, err = Load(badPath)
	requireLocatedError(t, badPath, err, "expires_at", "RFC3339")
}

// TestSchemasCompile 守护三份嵌入 schema 本身可编译：它们随代码进版本历史，
// 手改一处 typo 应在测试期就暴露，而不是等到加载档案时。
func TestSchemasCompile(t *testing.T) {
	for _, entry := range schemaFiles {
		if _, err := compiledSchema(entry.url); err != nil {
			t.Errorf("内置 schema %s 编译失败：%v", entry.file, err)
		}
	}
}

// TestLimitsSchemaRequiresRemaining 覆盖额度文档「必须能确定 remaining」的 anyOf：
// 只给 used 无法推出剩余量，必须拒绝；给出 remaining 或 limit+used 则通过。
func TestLimitsSchemaRequiresRemaining(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{
			name: "只给 used 拒绝",
			content: `schema: 1
account:
  - {kind: quota, metric: usd, window: month, tz: "+08:00", used: 30}
`,
			wantErr: "remaining",
		},
		{
			name: "单给 remaining 通过",
			content: `schema: 1
account:
  - {kind: quota, metric: usd, window: 5h, remaining: 5.2}
`,
		},
		{
			name: "limit 与 used 通过",
			content: `schema: 1
account:
  - {kind: quota, metric: usd, window: month, tz: "+08:00", limit: 70, used: 30}
models:
  glm-5.3:
    - {kind: quota, metric: tokens, window: 5h, limit: 2000000, remaining: 900000}
`,
		},
		{
			name: "unbounded 不参与 remaining 判定",
			content: `schema: 1
account:
  - {kind: quota, metric: tokens, window: absolute, unbounded: true}
`,
		},
		{
			name: "unbounded: false 仍需能确定 remaining",
			content: `schema: 1
account:
  - {kind: quota, metric: tokens, window: absolute, unbounded: false}
`,
			wantErr: "remaining",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeProfile(t, tt.content)
			errs := validateAgainst(t, path, limitsSchemaURL)
			if tt.wantErr == "" {
				if len(errs) != 0 {
					t.Fatalf("期望通过，实际报错：%v", errs[0])
				}
				return
			}
			if len(errs) == 0 {
				t.Fatalf("期望报错含 %q，实际通过", tt.wantErr)
			}
			if !strings.Contains(errs[0].Msg, tt.wantErr) {
				t.Errorf("错误文案 %q 应含 %q", errs[0].Msg, tt.wantErr)
			}
		})
	}
}

// validateAgainst 用指定 schema 校验一份档案并返回全部可定位错误，供 schema 级别的用例使用。
func validateAgainst(t *testing.T, path string, schemaURL string) []*Error {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取测试档案失败：%v", err)
	}
	root, parseErr := parseDocument(path, data)
	if parseErr != nil {
		t.Fatalf("解析测试档案失败：%v", parseErr)
	}
	schema, err := compiledSchema(schemaURL)
	if err != nil {
		t.Fatalf("编译 schema 失败：%v", err)
	}
	instance, err := instanceFromNode(root)
	if err != nil {
		t.Fatalf("转换校验输入失败：%v", err)
	}
	var validationErr *jsonschema.ValidationError
	if err := schema.Validate(instance); err != nil {
		if !errors.As(err, &validationErr) {
			t.Fatalf("校验错误类型意外：%T", err)
		}
		return schemaErrors(path, root, validationErr)
	}
	return nil
}
