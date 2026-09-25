package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sumwai/nova/internal/config"
)

// geminiConfig 解析一份只含一条 Gemini 渠道的配置，把上游地址指向测试服务器。
//
// 地址写成带引号的模板：`{model}` 没有被引号包住时会被词法器当成块开启。
func geminiConfig(t *testing.T, baseURL string, models ...string) *config.Config {
	t.Helper()
	var declared strings.Builder
	for _, model := range models {
		fmt.Fprintf(&declared, "    model %s\n", model)
	}
	src := fmt.Sprintf(`version 1
log_level info
log_format text
client_key client-secret
provider gemini-up {
    api_key up-secret
    url "%s/v1beta/models/{model}:generateContent"
%s}
`, baseURL, declared.String())
	cfg, err := config.Parse([]byte(src), "gemini-test.nova")
	if err != nil {
		t.Fatalf("解析测试配置失败：%v", err)
	}
	return cfg
}

// assembleDataPlane 装配配置并返回数据面处理器。
func assembleDataPlane(t *testing.T, cfg *config.Config) http.Handler {
	t.Helper()
	assembled, err := Assemble(context.Background(), cfg, AssembleOptions{LogOutput: io.Discard})
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}
	t.Cleanup(func() { _ = assembled.Close() })
	return assembled.Handler
}

// TestForwardOpenAIClientToGeminiUpstream 覆盖「OpenAI 客户端 → Gemini 上游」的跨协议链路：
// 请求按 Gemini 线格式重建、地址按模型名与动作构造、凭据以 x-goog-api-key 注入，
// 响应再按 OpenAI 形状回给客户端。
func TestForwardOpenAIClientToGeminiUpstream(t *testing.T) {
	var gotPath, gotKey, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-goog-api-key")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"你好"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5},"modelVersion":"gemini-2.5-flash-001"}`)
	}))
	defer upstream.Close()

	handler := assembleDataPlane(t, geminiConfig(t, upstream.URL, "gemini-2.5-flash"))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer client-secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，响应体 %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1beta/models/gemini-2.5-flash:generateContent" {
		t.Errorf("上游路径 = %q", gotPath)
	}
	if gotKey != "up-secret" {
		t.Errorf("上游凭据头 = %q，期望 up-secret", gotKey)
	}
	var upstreamBody struct {
		Contents []struct {
			Role  string `json:"role"`
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal([]byte(gotBody), &upstreamBody); err != nil {
		t.Fatalf("上游请求体不是合法 JSON：%v（%s）", err, gotBody)
	}
	if len(upstreamBody.Contents) == 0 || upstreamBody.Contents[0].Parts[0].Text != "hi" {
		t.Errorf("上游请求体 = %s", gotBody)
	}

	var response struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("客户端响应不是合法 JSON：%v（%s）", err, rec.Body.String())
	}
	if len(response.Choices) == 0 || response.Choices[0].Message.Content != "你好" {
		t.Errorf("客户端响应 = %s", rec.Body.String())
	}
}

// TestForwardGeminiClientToGeminiUpstream 覆盖同协议透传：客户端拿到的就是上游原始报文。
func TestForwardGeminiClientToGeminiUpstream(t *testing.T) {
	const upstreamBody = `{"candidates":[{"content":{"role":"model","parts":[{"text":"世界"}]},"finishReason":"STOP"}],"modelVersion":"gemini-2.5-flash-001"}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/v1beta/models/gemini-2.5-flash:generateContent" {
			t.Errorf("上游路径 = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, upstreamBody)
	}))
	defer upstream.Close()

	handler := assembleDataPlane(t, geminiConfig(t, upstream.URL, "gemini-2.5-flash"))
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-flash:generateContent",
		strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
	// Gemini 客户端用 x-goog-api-key 提交网关凭据。
	req.Header.Set("x-goog-api-key", "client-secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，响应体 %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != upstreamBody {
		t.Errorf("同协议透传应逐字节返回上游报文，实际 %s", rec.Body.String())
	}
}

// TestForwardGeminiClientStream 覆盖 Gemini 客户端的流式透传：逐帧原样下发。
func TestForwardGeminiClientStream(t *testing.T) {
	frames := "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"Hel\"}]}}]}\n\n" +
		"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"lo\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":1,\"candidatesTokenCount\":2,\"totalTokenCount\":3}}\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.RequestURI(); got != "/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse" {
			t.Errorf("上游地址 = %q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, frames)
	}))
	defer upstream.Close()

	handler := assembleDataPlane(t, geminiConfig(t, upstream.URL, "gemini-2.5-flash"))
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-flash:streamGenerateContent",
		strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
	req.Header.Set("Authorization", "Bearer client-secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，响应体 %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != frames {
		t.Errorf("流式透传应逐帧原样下发，实际 %s", rec.Body.String())
	}
}

// TestForwardOpenAIClientStreamToGeminiUpstream 覆盖「OpenAI 客户端流式 → Gemini 上游」：
// 上游的 Gemini 帧被解码后再按 OpenAI 形状重建，客户端拿到 chat.completion.chunk 与 [DONE]。
func TestForwardOpenAIClientStreamToGeminiUpstream(t *testing.T) {
	frames := "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"Hel\"}]}}]}\n\n" +
		"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"lo\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":1,\"candidatesTokenCount\":2,\"totalTokenCount\":3}}\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, frames)
	}))
	defer upstream.Close()

	handler := assembleDataPlane(t, geminiConfig(t, upstream.URL, "gemini-2.5-flash"))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gemini-2.5-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer client-secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，响应体 %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"content":"Hel"`) || !strings.Contains(body, `"content":"lo"`) {
		t.Errorf("重建后的流式响应缺少文本增量：%s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("重建后的流式响应缺少结束哨兵：%s", body)
	}
}
