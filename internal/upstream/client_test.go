package upstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sumwai/nova/internal/adapters/openaichat"
	"github.com/sumwai/nova/internal/domain"
)

// stubHeaders 是 HeaderProvider 的空实现：本文件只考察响应体解码路径，不关心请求头。
type stubHeaders struct{}

func (stubHeaders) UpstreamHeaders(context.Context, domain.Route) (http.Header, error) {
	return http.Header{}, nil
}

// openAIChatAdapters 返回只认 Chat Completions 报文的适配器查找函数。
func openAIChatAdapters(domain.Protocol) (domain.Adapter, error) { return openaichat.New(), nil }

// TestCompleteAttachesUpstreamBodyToDecodeFailure 守护「上游响应无法解析」的排障信息。
//
// 上游用 2xx 回了不符合本协议的正文时（典型成因：url 写错被重定向到官网首页，
// 或上游用 200 回了错误信封），只报一个笼统原因无法定位，必须把上游正文片段带进详情。
// 详情只进服务端日志，不进面向客户端的错误体。
func TestCompleteAttachesUpstreamBodyToDecodeFailure(t *testing.T) {
	const html = `<html><head><title>301 Moved Permanently</title></head><body>nginx</body></html>`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(html))
	}))
	defer server.Close()

	client, err := New(Options{Headers: stubHeaders{}, Adapters: openAIChatAdapters})
	if err != nil {
		t.Fatalf("构造上游客户端失败: %v", err)
	}

	route := domain.Route{
		UpstreamID: "u1",
		Protocol:   domain.ProtocolOpenAIChat,
		BaseURL:    server.URL,
	}
	_, callErr := client.Complete(context.Background(), route, &domain.Request{Model: "m"}, []byte(`{"model":"m"}`))
	if callErr == nil {
		t.Fatal("上游返回非本协议的正文时必须报错，实际返回了成功")
	}
	domainErr := domain.AsError(callErr)
	if domainErr == nil {
		t.Fatalf("错误必须是 domain.Error，实际类型为 %T", callErr)
	}
	if domainErr.Code != domain.CodeUpstreamUnavailable {
		t.Errorf("错误码 = %q，期望 %q", domainErr.Code, domain.CodeUpstreamUnavailable)
	}
	if !strings.Contains(domainErr.Detail, "301 Moved Permanently") {
		t.Errorf("排障详情应包含上游正文片段，实际为 %q", domainErr.Detail)
	}
}
