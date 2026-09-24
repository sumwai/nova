package catalog

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/nova/internal/domain"
)

// stubFetcher 是一份可控的清单来源。
type stubFetcher struct {
	body []byte
	err  error
	// calls 记录被请求的端点，用于检查超时与凭据引用确实传到了实现那一侧。
	calls []Endpoint
}

func (f *stubFetcher) Listing(_ context.Context, ep Endpoint) ([]byte, error) {
	f.calls = append(f.calls, ep)
	return f.body, f.err
}

func testEndpoint() Endpoint {
	return Endpoint{
		Provider:      "relay",
		ListingURL:    "https://relay.example.com/v1/models",
		Protocol:      domain.ProtocolOpenAIChat,
		CredentialRef: "relay",
		Timeout:       5 * time.Second,
	}
}

func TestDiscoverKeepsAllowMinusDeny(t *testing.T) {
	fetcher := &stubFetcher{body: []byte(`{"object":"list","data":[
		{"id":"gpt-5","object":"model","created":1700000000,"owned_by":"openai"},
		{"id":"gpt-5-mini","object":"model"},
		{"id":"gpt-5-preview","object":"model"},
		{"id":"claude-sonnet-4","object":"model"},
		{"id":"text-embedding-3","object":"model"}
	]}`)}

	ep := testEndpoint()
	ep.Allow = []string{"gpt-5*", "claude-*"}
	ep.Deny = []string{"*-preview"}

	result, err := Discover(context.Background(), ep, fetcher)
	if err != nil {
		t.Fatalf("Discover 意外失败：%v", err)
	}
	if result.Shape != ShapeOpenAI {
		t.Errorf("形状 = %q，期望 %q", result.Shape, ShapeOpenAI)
	}
	if result.Found != 5 {
		t.Errorf("清单条目数 = %d，期望 5", result.Found)
	}
	if result.Filtered != 2 {
		t.Errorf("被排除数 = %d，期望 2（embedding 与 preview）", result.Filtered)
	}
	got := modelIDs(result.Models)
	want := []string{"gpt-5", "gpt-5-mini", "claude-sonnet-4"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("保留的模型 = %v，期望 %v", got, want)
	}
	if result.Models[0].CreatedAt.IsZero() {
		t.Errorf("第一条模型的时间没被读出：%+v", result.Models[0])
	}
	if result.Models[0].OwnedBy != "openai" {
		t.Errorf("第一条模型的归属 = %q，期望 openai", result.Models[0].OwnedBy)
	}
	if fetcher.calls[0].CredentialRef != "relay" {
		t.Errorf("传给取清单实现的凭据引用 = %q，期望 relay", fetcher.calls[0].CredentialRef)
	}
}

// 没写 allow 时清单里的模型全部保留，这是「只想排除少数几个」的用法。
func TestDiscoverWithoutAllowKeepsEverything(t *testing.T) {
	fetcher := &stubFetcher{body: []byte(`{"data":[{"id":"a"},{"id":"b"}]}`)}
	result, err := Discover(context.Background(), testEndpoint(), fetcher)
	if err != nil {
		t.Fatalf("Discover 意外失败：%v", err)
	}
	if result.Filtered != 0 || len(result.Models) != 2 {
		t.Errorf("保留 %d 条、排除 %d 条；期望全部保留", len(result.Models), result.Filtered)
	}
}

func TestDiscoverDeduplicatesKeepingFirst(t *testing.T) {
	fetcher := &stubFetcher{body: []byte(`{"data":[
		{"id":"a","owned_by":"first"},
		{"id":"a","owned_by":"second"},
		{"id":"b"}
	]}`)}
	result, err := Discover(context.Background(), testEndpoint(), fetcher)
	if err != nil {
		t.Fatalf("Discover 意外失败：%v", err)
	}
	if result.Found != 2 {
		t.Errorf("去重后条目数 = %d，期望 2", result.Found)
	}
	if result.Models[0].OwnedBy != "first" {
		t.Errorf("重复 id 应保留首个，实际归属 = %q", result.Models[0].OwnedBy)
	}
}

// 没有 id 的条目跳过而不是让整条端点失败：上游多回一条残缺记录，
// 不该让这条端点的全部模型一起不可用。
func TestDiscoverSkipsEntriesWithoutID(t *testing.T) {
	fetcher := &stubFetcher{body: []byte(`{"data":[{"id":"  "},{"id":"a"},{},{"id":""}]}`)}
	result, err := Discover(context.Background(), testEndpoint(), fetcher)
	if err != nil {
		t.Fatalf("Discover 意外失败：%v", err)
	}
	if len(result.Models) != 1 || result.Models[0].ID != "a" {
		t.Errorf("保留的模型 = %v，期望只有 a", modelIDs(result.Models))
	}
}

func TestDiscoverReportsHasMore(t *testing.T) {
	fetcher := &stubFetcher{body: []byte(`{"data":[{"type":"model","id":"a"}],"has_more":true,"last_id":"a"}`)}
	result, err := Discover(context.Background(), testEndpoint(), fetcher)
	if err != nil {
		t.Fatalf("Discover 意外失败：%v", err)
	}
	if !result.HasMore {
		t.Error("has_more 为真时应报告出来，装配层据此记提醒")
	}
}

func TestDiscoverRejectsBadListings(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "不是 JSON", body: `<html>`, want: "不是 JSON"},
		{name: "缺 data", body: `{"object":"list"}`, want: "没有 data"},
		{name: "data 不是数组", body: `{"data":{"id":"a"}}`, want: "无法解析"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Discover(context.Background(), testEndpoint(), &stubFetcher{body: []byte(tt.body)})
			if err == nil {
				t.Fatal("期望报错，但成功了")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("消息 = %q，期望含 %q", err.Error(), tt.want)
			}
			// 错误消息必须指向是哪条渠道的哪份清单，否则多端点配置里无从对上号。
			if !strings.Contains(err.Error(), "provider relay") {
				t.Errorf("消息 = %q，期望含渠道定位", err.Error())
			}
		})
	}
}

func TestDiscoverRejectsOversizedListing(t *testing.T) {
	body := make([]byte, MaxListingBytes+1)
	_, err := Discover(context.Background(), testEndpoint(), &stubFetcher{body: body})
	if err == nil || !strings.Contains(err.Error(), "超过") {
		t.Fatalf("超限响应体应被拒绝，实际：%v", err)
	}
}

func TestDiscoverWrapsFetcherError(t *testing.T) {
	boom := errors.New("上游 HTTP 状态码 503：上游维护中")
	_, err := Discover(context.Background(), testEndpoint(), &stubFetcher{err: boom})
	if err == nil {
		t.Fatal("取清单失败时应当返回错误")
	}
	if !errors.Is(err, boom) {
		t.Errorf("原始错误应可被取出，实际：%v", err)
	}
	if !strings.Contains(err.Error(), "请求失败") {
		t.Errorf("消息 = %q，期望说明是请求环节失败", err.Error())
	}
}

func TestDiscoverRejectsEmptyConfiguration(t *testing.T) {
	if _, err := Discover(context.Background(), Endpoint{Provider: "x"}, &stubFetcher{}); err == nil {
		t.Error("没有清单地址时应当报错")
	}
	if _, err := Discover(context.Background(), testEndpoint(), nil); err == nil {
		t.Error("没有取清单实现时应当报错")
	}
}

func modelIDs(models []Model) []string {
	ids := make([]string, len(models))
	for i, model := range models {
		ids[i] = model.ID
	}
	return ids
}
