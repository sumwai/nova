package stats

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandlerServesJSON(t *testing.T) {
	store := testStore(t)
	store.LogAccess(accessRecord("r1", 200))

	rec := httptest.NewRecorder()
	NewHandler(store).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/stats", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q，期望 no-store", got)
	}

	var report Report
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("响应不是合法 JSON：%v", err)
	}
	if report.Window.Totals.Requests != 1 {
		t.Errorf("窗口请求数 = %d，期望 1", report.Window.Totals.Requests)
	}
	if report.Process.StartedAt.IsZero() {
		t.Error("process.started_at 不应为零值")
	}
}

func TestHandlerRejectsNonGET(t *testing.T) {
	store := testStore(t)
	rec := httptest.NewRecorder()
	NewHandler(store).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/debug/stats", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("状态码 = %d，期望 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
		t.Errorf("Allow = %q，期望 GET, HEAD", got)
	}
}

func TestHandlerRejectsBadQueryWithJSON(t *testing.T) {
	store := testStore(t)
	rec := httptest.NewRecorder()
	NewHandler(store).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/stats?modle=gpt-5", nil))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("错误响应不是合法 JSON：%v", err)
	}
	if body["error"] == "" {
		t.Error("错误响应缺少 error 字段")
	}
}

func TestHandlerPrettyOutput(t *testing.T) {
	store := testStore(t)
	store.LogAccess(accessRecord("r1", 200))

	rec := httptest.NewRecorder()
	NewHandler(store).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/stats?pretty=true", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", rec.Code)
	}
	if body := rec.Body.String(); body == "" || body[0] != '{' {
		t.Errorf("响应体不像缩进 JSON：%q", body)
	}
}
