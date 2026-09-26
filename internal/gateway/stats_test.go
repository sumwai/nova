package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sumwai/nova/internal/config"
	"github.com/sumwai/nova/internal/stats"
	"github.com/sumwai/nova/internal/transport"
)

// testStatsStore 造一个纯内存库的统计存储，测试结束自动关闭。
func testStatsStore(t *testing.T) *stats.Store {
	t.Helper()
	store, err := stats.New(stats.Options{})
	if err != nil {
		t.Fatalf("创建统计存储失败：%v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// 回环监听且未配 client_key 时统计端点不鉴权：这是本机用法，也是缺省形态。
func TestDataPlaneServesStatsOnLoopbackWithoutAuth(t *testing.T) {
	var forward stubForwarder
	store := testStatsStore(t)
	store.LogAccess(transport.AccessRecord{RequestID: "r1", HTTPStatus: 200, UserAgent: "curl/8.5.0"})

	cfg := &config.Config{Listen: "127.0.0.1:8080"}
	handler := newDataPlane(&forward, cfg, adapterResolver(newAdapters()), nil, store)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, statsPath, nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（响应体 %q）", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"requests":1`) {
		t.Errorf("响应体 = %q，期望含窗口请求数", rec.Body.String())
	}
	if forward.called {
		t.Error("统计端点不该经过转发链路")
	}
}

// 绑到非回环地址后统计端点与客户端入口用同一份 client_key。
func TestDataPlaneAuthorizesStatsOffLoopback(t *testing.T) {
	var forward stubForwarder
	store := testStatsStore(t)
	cfg := &config.Config{Listen: "0.0.0.0:8080", ClientKeys: []string{"secret"}}
	handler := newDataPlane(&forward, cfg, adapterResolver(newAdapters()), nil, store)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, statsPath, nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无凭据状态码 = %d，期望 401", rec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, statsPath, nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("带凭据状态码 = %d，期望 200（响应体 %q）", rec.Code, rec.Body.String())
	}
}

// 没有统计存储时（只做装配检查的调用方）不注册统计路径，请求回落到转发入口。
func TestDataPlaneOmitsStatsPathWithoutStore(t *testing.T) {
	var forward stubForwarder
	cfg := &config.Config{Listen: "127.0.0.1:8080"}
	handler := newDataPlane(&forward, cfg, adapterResolver(newAdapters()), nil, nil)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, statsPath, nil))
	if !forward.called {
		t.Error("没有统计存储时该路径应回落到转发入口，而不是被统计端点接管")
	}
}
