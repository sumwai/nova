package catalog

import (
	"testing"
	"time"
)

func TestParseListingDetectsShapes(t *testing.T) {
	tests := []struct {
		name string
		body string
		want Shape
	}{
		{
			name: "OpenAI",
			body: `{"object":"list","data":[{"id":"gpt-5","object":"model","created":1700000000,"owned_by":"openai"}]}`,
			want: ShapeOpenAI,
		},
		{
			name: "Anthropic",
			body: `{"data":[{"type":"model","id":"claude-sonnet-4","display_name":"Claude Sonnet 4","created_at":"2025-02-19T00:00:00Z"}],"has_more":false}`,
			want: ShapeAnthropic,
		},
		{
			// 只有 id 的清单既不像 OpenAI 也不像 Anthropic，字段照样能用。
			name: "只有 id",
			body: `{"data":[{"id":"a"}]}`,
			want: ShapeUnknown,
		},
		{
			// 同一批条目两种风格的特征都有时不硬判：那种清单多半是第三种风格。
			name: "两种风格混在一起",
			body: `{"data":[{"id":"a","object":"model"},{"id":"b","display_name":"B"}]}`,
			want: ShapeUnknown,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, shape, _, err := parseListing([]byte(tt.body))
			if err != nil {
				t.Fatalf("parseListing 意外失败：%v", err)
			}
			if shape != tt.want {
				t.Errorf("形状 = %q，期望 %q", shape, tt.want)
			}
		})
	}
}

func TestParseListingNormalizesBothShapes(t *testing.T) {
	openai, _, _, err := parseListing([]byte(
		`{"data":[{"id":"gpt-5","object":"model","created":1700000000,"owned_by":"openai"}]}`))
	if err != nil {
		t.Fatalf("parseListing 意外失败：%v", err)
	}
	if openai[0].ID != "gpt-5" || openai[0].OwnedBy != "openai" {
		t.Errorf("OpenAI 形状的条目 = %+v", openai[0])
	}
	if want := time.Unix(1700000000, 0).UTC(); !openai[0].CreatedAt.Equal(want) {
		t.Errorf("created 应解析成 %v，实际 %v", want, openai[0].CreatedAt)
	}

	anthropic, _, _, err := parseListing([]byte(
		`{"data":[{"type":"model","id":"claude-sonnet-4","display_name":"Claude Sonnet 4","created_at":"2025-02-19T00:00:00Z"}]}`))
	if err != nil {
		t.Fatalf("parseListing 意外失败：%v", err)
	}
	if anthropic[0].DisplayName != "Claude Sonnet 4" {
		t.Errorf("display_name 没被读出：%+v", anthropic[0])
	}
	if want, _ := time.Parse(time.RFC3339, "2025-02-19T00:00:00Z"); !anthropic[0].CreatedAt.Equal(want) {
		t.Errorf("created_at 应解析成 %v，实际 %v", want, anthropic[0].CreatedAt)
	}
}

// 上游给了解析不出的时间时留零值，不回退成 Unix 纪元：
// 零值在内部一路表示「上游没给」，面向客户端的填充只发生在渲染层。
func TestParseListingLeavesUnknownTimeZero(t *testing.T) {
	models, _, _, err := parseListing([]byte(`{"data":[{"id":"a","created_at":"不是时间"}]}`))
	if err != nil {
		t.Fatalf("parseListing 意外失败：%v", err)
	}
	if !models[0].CreatedAt.IsZero() {
		t.Errorf("无法解析的时间应为零值，实际 %v", models[0].CreatedAt)
	}
}

func TestParseListingRejectsMissingData(t *testing.T) {
	for _, body := range []string{`{"object":"list"}`, `{"data":null}`} {
		if _, _, _, err := parseListing([]byte(body)); err == nil {
			t.Errorf("%s 应当被拒绝", body)
		}
	}
}

func TestParseListingAcceptsEmptyData(t *testing.T) {
	models, _, _, err := parseListing([]byte(`{"data":[]}`))
	if err != nil {
		t.Fatalf("空清单是合法响应，实际报错：%v", err)
	}
	if len(models) != 0 {
		t.Errorf("空清单应当给出 0 条，实际 %d 条", len(models))
	}
}
