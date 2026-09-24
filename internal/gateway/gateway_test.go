package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sumwai/nova/internal/config"
)

// testAssembly 造一份可供测试使用的装配。cfg 为 nil 时用一套无害的缺省值。
func testAssembly(t *testing.T, cfg *config.Config) *Assembly {
	t.Helper()
	if cfg == nil {
		cfg = &config.Config{LogLevel: "info", Listen: ":0", Admin: "localhost:0"}
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
	_, err := Assemble(&config.Config{LogLevel: "verbose"}, io.Discard)
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

func TestDataPlaneServesHealthzAndRefusesForwarding(t *testing.T) {
	handler := newDataPlane()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("存活探针状态码 = %d，期望 200", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Errorf("存活探针响应体 = %q，期望 %q", rec.Body.String(), "ok")
	}

	// 转发路径回 501 而不是 404：「路径对、功能还没做」与「路径不存在」
	// 对调用方是两件事，混成 404 会让人反复检查自己拼的 URL。
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("转发路径状态码 = %d，期望 501", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "尚未实现") {
		t.Errorf("转发路径响应体 = %q，期望说明转发尚未实现", rec.Body.String())
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
