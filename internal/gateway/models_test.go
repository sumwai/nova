package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/nova/internal/config"
	"github.com/sumwai/nova/internal/domain"
)

// testModelEntries 造两条目录条目：一条来自发现并带上游给的展示名与时间，一条是显式声明。
func testModelEntries() []ModelEntry {
	return []ModelEntry{
		{
			Name:        "claude-sonnet-4",
			DisplayName: "Claude Sonnet 4",
			CreatedAt:   time.Date(2025, 2, 19, 0, 0, 0, 0, time.UTC),
			Source:      ModelSourceDiscovered,
		},
		{Name: "gpt-5", Source: ModelSourceStatic},
	}
}

func TestRenderModelsOpenAIShape(t *testing.T) {
	body, err := renderModels(testModelEntries(), protocolForModelsRequest(
		httptest.NewRequest(http.MethodGet, modelsPath, nil)))
	if err != nil {
		t.Fatalf("渲染失败：%v", err)
	}

	var got struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Created int64  `json:"created"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("解析失败：%v（原文 %s）", err, body)
	}
	if got.Object != "list" || len(got.Data) != 2 {
		t.Fatalf("响应 = %+v，期望 object list 与两条模型", got)
	}
	if got.Data[0].ID != "claude-sonnet-4" || got.Data[0].Object != "model" {
		t.Errorf("第一条 = %+v", got.Data[0])
	}
	if want := time.Date(2025, 2, 19, 0, 0, 0, 0, time.UTC).Unix(); got.Data[0].Created != want {
		t.Errorf("created = %d，期望透传上游给的时间 %d", got.Data[0].Created, want)
	}
	// 上游没给时间时填 0 而不是省略字段：两种协议的模型对象里这个字段都是必需。
	if got.Data[1].ID != "gpt-5" || got.Data[1].Created != 0 {
		t.Errorf("第二条 = %+v，期望 created 为 0", got.Data[1])
	}
	if got.Data[0].OwnedBy != ownedByNova {
		t.Errorf("owned_by = %q，期望固定填 %q", got.Data[0].OwnedBy, ownedByNova)
	}
}

func TestRenderModelsAnthropicShape(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, modelsPath, nil)
	req.Header.Set(anthropicVersionHeader, "2023-06-01")
	body, err := renderModels(testModelEntries(), protocolForModelsRequest(req))
	if err != nil {
		t.Fatalf("渲染失败：%v", err)
	}

	var got struct {
		Data []struct {
			Type        string `json:"type"`
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
			CreatedAt   string `json:"created_at"`
		} `json:"data"`
		HasMore bool   `json:"has_more"`
		FirstID string `json:"first_id"`
		LastID  string `json:"last_id"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("解析失败：%v（原文 %s）", err, body)
	}
	if len(got.Data) != 2 || got.HasMore {
		t.Fatalf("响应 = %+v，期望两条模型且 has_more 恒为 false", got)
	}
	if got.Data[0].Type != "model" || got.Data[0].DisplayName != "Claude Sonnet 4" {
		t.Errorf("第一条 = %+v", got.Data[0])
	}
	if got.Data[0].CreatedAt != "2025-02-19T00:00:00Z" {
		t.Errorf("created_at = %q，期望透传上游给的时间", got.Data[0].CreatedAt)
	}
	// 展示名缺失时退回对外名，时间缺失时填 Unix 纪元：都是必需字段，不能省略。
	if got.Data[1].DisplayName != "gpt-5" {
		t.Errorf("display_name = %q，期望退回对外名", got.Data[1].DisplayName)
	}
	if got.Data[1].CreatedAt != "1970-01-01T00:00:00Z" {
		t.Errorf("created_at = %q，期望 Unix 纪元", got.Data[1].CreatedAt)
	}
	if got.FirstID != "claude-sonnet-4" || got.LastID != "gpt-5" {
		t.Errorf("first_id / last_id = %q / %q", got.FirstID, got.LastID)
	}
}

// 空目录不写 first_id / last_id：空串在这两个字段上没有意义。
func TestRenderModelsAnthropicOmitsIDsWhenEmpty(t *testing.T) {
	body, err := renderModels(nil, domain.ProtocolAnthropicMessages)
	if err != nil {
		t.Fatalf("渲染失败：%v", err)
	}
	if strings.Contains(string(body), "first_id") || strings.Contains(string(body), "last_id") {
		t.Errorf("空目录不该写 first_id / last_id：%s", body)
	}
}

func TestModelsHandlerAnswersShapeByRequestHeader(t *testing.T) {
	handler := newModelsHandler(testModelEntries())

	tests := []struct {
		name       string
		version    string
		wantMarker string
	}{
		{name: "不带版本头按 OpenAI 形状", wantMarker: `"object":"list"`},
		{name: "带版本头按 Anthropic 形状", version: "2023-06-01", wantMarker: `"type":"model"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, modelsPath, nil)
			if tt.version != "" {
				req.Header.Set(anthropicVersionHeader, tt.version)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("状态码 = %d，期望 200", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), tt.wantMarker) {
				t.Errorf("响应体 = %q，期望含 %q", rec.Body.String(), tt.wantMarker)
			}
			if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
				t.Errorf("Content-Type = %q，期望 JSON", got)
			}
		})
	}
}

func TestModelsHandlerRejectsNonGet(t *testing.T) {
	handler := newModelsHandler(nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, modelsPath, nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("状态码 = %d，期望 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != http.MethodGet {
		t.Errorf("Allow = %q，期望 %q", got, http.MethodGet)
	}
}

// 带正确凭据时，精确路径优先于 "/"：模型清单不被转发入口当成未注册路径。
func TestDataPlaneServesModelsPathWithCredentials(t *testing.T) {
	var forward stubForwarder
	cfg := &config.Config{ClientKeys: []string{"secret"}}
	handler := newDataPlane(&forward, cfg, adapterResolver(newAdapters()), testModelEntries())

	req := httptest.NewRequest(http.MethodGet, modelsPath, nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（响应体 %q）", rec.Code, rec.Body.String())
	}
	if forward.called {
		t.Error("模型清单不该经过转发链路")
	}
	if !strings.Contains(rec.Body.String(), "claude-sonnet-4") {
		t.Errorf("响应体 = %q，期望含目录里的模型名", rec.Body.String())
	}
}

// 模型清单与转发路径一样要过鉴权；401 的形状按请求头判定，与成功响应一致。
func TestDataPlaneAuthorizesModelsPath(t *testing.T) {
	cfg := &config.Config{ClientKeys: []string{"secret"}}
	handler := newDataPlane(&stubForwarder{}, cfg, adapterResolver(newAdapters()), testModelEntries())

	tests := []struct {
		name       string
		version    string
		wantMarker string
	}{
		{name: "OpenAI 形状的错误体", wantMarker: `"error"`},
		{name: "Anthropic 形状的错误体", version: "2023-06-01", wantMarker: `"type":"error"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, modelsPath, nil)
			if tt.version != "" {
				req.Header.Set(anthropicVersionHeader, tt.version)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("状态码 = %d，期望 401", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), tt.wantMarker) {
				t.Errorf("错误体 = %q，期望含 %q", rec.Body.String(), tt.wantMarker)
			}
		})
	}
}
