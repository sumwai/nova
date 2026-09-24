package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sumwai/nova/internal/config"
)

// testAssembly 造一份可供测试使用的装配。
//
// 它把 cfg 里没写的字段补成缺省：config.Load 总会填上它们，而测试手写 Config 时很容易漏。
// 漏掉的结果是一个与本次测试无关的装配错误（比如「日志格式 "" 不认识」），
// 那会让人去查被测逻辑，而问题其实在构造输入那一行。
func testAssembly(t *testing.T, cfg *config.Config) *Assembly {
	t.Helper()
	if cfg == nil {
		cfg = &config.Config{}
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	if cfg.LogFormat == "" {
		cfg.LogFormat = "text"
	}
	if cfg.Listen == "" {
		cfg.Listen = ":0"
	}
	if cfg.Admin == "" {
		cfg.Admin = "localhost:0"
	}
	assembled, err := Assemble(cfg, io.Discard)
	if err != nil {
		t.Fatalf("Assemble 意外失败：%v", err)
	}
	return assembled
}

func TestHolderReplacesAssemblyAsAWhole(t *testing.T) {
	first := testAssembly(t, &config.Config{LogLevel: "info", Listen: ":1", Admin: "localhost:1"})
	holder := NewHolder(first)

	if holder.Current() != first {
		t.Fatal("Current 返回的不是初始化时给的那份装配")
	}

	second := testAssembly(t, &config.Config{LogLevel: "debug", Listen: ":2", Admin: "localhost:2"})
	previous := holder.replace(second)

	if previous != first {
		t.Error("replace 返回的旧装配不是被换下的那一份")
	}
	if holder.Current() != second {
		t.Error("replace 之后 Current 没有返回新装配")
	}
}

// nil 装配在构造处就被挡掉：每个请求的入口都要读当前装配，
// 若它可能是 nil，那次读取就得先判空。
func TestNewHolderRejectsNil(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("NewHolder(nil) 没有 panic")
		}
	}()
	NewHolder(nil)
}

func TestHolderServeHTTPDelegatesToCurrentAssembly(t *testing.T) {
	holder := NewHolder(testAssembly(t, nil))

	rec := httptest.NewRecorder()
	holder.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("状态码 = %d，期望 200", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Errorf("响应体 = %q，期望 %q", rec.Body.String(), "ok")
	}
}

func TestAssembleRejectsUnknownLogLevel(t *testing.T) {
	_, err := Assemble(&config.Config{LogLevel: "verbose", LogFormat: "text"}, io.Discard)
	if err == nil {
		t.Fatal("期望 Assemble 报错，但它成功了")
	}
	// 装配期就报出，而不是退化成缺省级别：后者会让「写了 debug 却没有 debug 日志」
	// 变成一桩要靠读源码才能破的悬案。
	if !strings.Contains(err.Error(), "日志级别") {
		t.Errorf("消息 = %q，期望提到日志级别", err.Error())
	}
}

func TestAssemblyCloseIsIdempotent(t *testing.T) {
	assembled := testAssembly(t, nil)

	if err := assembled.Close(); err != nil {
		t.Errorf("第一次 Close 报错：%v", err)
	}
	if err := assembled.Close(); err != nil {
		t.Errorf("第二次 Close 报错：%v", err)
	}

	// nil 接收者返回 nil：调用方在「没有旧装配」这个边界上不该还要先判空。
	var absent *Assembly
	if err := absent.Close(); err != nil {
		t.Errorf("nil 装配的 Close 报错：%v", err)
	}
}

// stubForwarder 是一个只留下痕迹的数据面转发入口，用于分辨「请求走到了转发」
// 与「请求在鉴权或路由那一步就被挡住了」。它回一个没人会误认成真实响应的状态码。
type stubForwarder struct {
	called bool
}

func (s *stubForwarder) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	s.called = true
	w.WriteHeader(http.StatusTeapot)
}

func TestDataPlaneServesHealthzWithoutCredentials(t *testing.T) {
	var forward stubForwarder
	// 故意配上 client_key：探活不该因此被挡住。
	handler := newDataPlane(&forward, &config.Config{ClientKeys: []string{"secret"}},
		adapterResolver(newAdapters()))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, healthzPath, nil))
	if rec.Code != http.StatusOK {
		t.Errorf("存活探针状态码 = %d，期望 200", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Errorf("存活探针响应体 = %q，期望 ok", rec.Body.String())
	}
	// 探活不该经过转发：宁可让编排系统看到一个「在线但转发有问题」的进程，
	// 也不要让它因为上游故障而重启一个本身健康的网关。
	if forward.called {
		t.Error("存活探针不该经过转发链路")
	}
}

func TestDataPlaneSkipsAuthWithoutClientKeys(t *testing.T) {
	var forward stubForwarder
	handler := newDataPlane(&forward, &config.Config{}, adapterResolver(newAdapters()))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if rec.Code != http.StatusTeapot {
		t.Errorf("未配 client_key 时状态码 = %d，期望请求直达转发链路", rec.Code)
	}
	if !forward.called {
		t.Error("未配 client_key 时请求应直达转发链路")
	}
}

func TestDataPlaneRequiresMatchingClientKey(t *testing.T) {
	cfg := &config.Config{ClientKeys: []string{"first", "second"}}
	tests := []struct {
		name     string
		path     string
		bearer   string
		xAPIKey  string
		wantCode int
	}{
		{name: "没有凭据", path: "/v1/chat/completions", wantCode: http.StatusUnauthorized},
		{name: "Bearer 命中第一条", path: "/v1/chat/completions", bearer: "first", wantCode: http.StatusTeapot},
		{name: "Bearer 命中第二条", path: "/v1/chat/completions", bearer: "second", wantCode: http.StatusTeapot},
		{name: "x-api-key 命中", path: "/v1/messages", xAPIKey: "second", wantCode: http.StatusTeapot},
		{name: "方案名大小写不敏感", path: "/v1/responses", bearer: "first", wantCode: http.StatusTeapot},
		{name: "凭据不匹配", path: "/v1/chat/completions", bearer: "wrong", wantCode: http.StatusUnauthorized},
		{name: "凭据是前缀也不算命中", path: "/v1/chat/completions", bearer: "firs", wantCode: http.StatusUnauthorized},
		// 路径不认识时，不泄露路由事实，但仍要说清缺凭据：只回 404 会让一个只是
		// 漏了 token 的客户端去反复检查自己拼的 URL。
		{name: "未知路径也要求凭据", path: "/nope", wantCode: http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var forward stubForwarder
			handler := newDataPlane(&forward, cfg, adapterResolver(newAdapters()))
			req := httptest.NewRequest(http.MethodPost, tt.path, nil)
			if tt.bearer != "" {
				req.Header.Set("Authorization", "Bearer "+tt.bearer)
			}
			if tt.xAPIKey != "" {
				req.Header.Set(apiKeyHeader, tt.xAPIKey)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tt.wantCode {
				t.Fatalf("状态码 = %d，期望 %d（响应体 %q）", rec.Code, tt.wantCode, rec.Body.String())
			}
			if tt.wantCode == http.StatusUnauthorized {
				if forward.called {
					t.Error("被鉴权拦下的请求不该到达转发链路")
				}
				if rec.Body.Len() == 0 {
					t.Error("401 的响应体不该为空")
				}
			}
		})
	}
}

// TestDataPlaneEncodesUnauthorizedPerClientProtocol 守住「鉴权失败也按客户端协议编码」。
//
// 调用方按一种协议写解析，就不该为「先被鉴权挡住」再写第二套。
func TestDataPlaneEncodesUnauthorizedPerClientProtocol(t *testing.T) {
	cfg := &config.Config{ClientKeys: []string{"secret"}}
	tests := []struct {
		path        string
		contentType string
	}{
		{path: "/v1/chat/completions", contentType: "application/json"},
		{path: "/v1/messages", contentType: "application/json"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			var forward stubForwarder
			handler := newDataPlane(&forward, cfg, adapterResolver(newAdapters()))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, tt.path, nil))
			if got := rec.Header().Get("Content-Type"); !strings.Contains(got, tt.contentType) {
				t.Errorf("Content-Type = %q，期望包含 %q", got, tt.contentType)
			}
			if !strings.HasPrefix(rec.Body.String(), "{") {
				t.Errorf("响应体 = %q，期望按协议编码的 JSON", rec.Body.String())
			}
		})
	}
}

func TestCheckReloadable(t *testing.T) {
	current := &config.Config{LogLevel: "info", Listen: ":8080", Admin: "localhost:2026"}

	tests := []struct {
		name     string
		next     *config.Config
		wantErrs []string
	}{
		{
			name: "完全没有变化",
			next: &config.Config{LogLevel: "info", Listen: ":8080", Admin: "localhost:2026"},
		},
		{
			name: "只改日志级别可以热重载",
			next: &config.Config{LogLevel: "debug", Listen: ":8080", Admin: "localhost:2026"},
		},
		{
			name:     "改监听地址要求重启",
			next:     &config.Config{LogLevel: "info", Listen: ":9090", Admin: "localhost:2026"},
			wantErrs: []string{"listen", ":8080", ":9090", "重启"},
		},
		{
			name:     "改管理端点要求重启",
			next:     &config.Config{LogLevel: "info", Listen: ":8080", Admin: "localhost:9999"},
			wantErrs: []string{"admin", "localhost:2026", "localhost:9999", "重启"},
		},
		{
			name:     "两者都改时两条都报出来",
			next:     &config.Config{LogLevel: "info", Listen: ":9090", Admin: "localhost:9999"},
			wantErrs: []string{"listen", "admin"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkReloadable(current, tt.next)

			if len(tt.wantErrs) == 0 {
				if err != nil {
					t.Fatalf("期望可以重载，但报错：%v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("期望拒绝这次重载，但它通过了")
			}
			for _, want := range tt.wantErrs {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("消息 = %q，期望包含 %q", err.Error(), want)
				}
			}
		})
	}
}
