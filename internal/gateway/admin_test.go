package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func noopReload(context.Context, string) error { return nil }

func newTestAdminHandler(t *testing.T, reload func(context.Context, string) error) http.Handler {
	t.Helper()
	return newAdminHandler(NewHolder(testAssembly(t, nil)), reload)
}

func TestAdminHandlerRejectsWrongPath(t *testing.T) {
	handler := newTestAdminHandler(t, noopReload)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/nope", nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("状态码 = %d，期望 404", rec.Code)
	}
}

func TestAdminHandlerRejectsNonPost(t *testing.T) {
	handler := newTestAdminHandler(t, noopReload)

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(method, AdminPath, nil))

			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("状态码 = %d，期望 405", rec.Code)
			}
			// Allow 头是 405 的一半信息量：只说「不允许」而不说「允许什么」，
			// 调用方还得去翻文档。
			if got := rec.Header().Get("Allow"); got != http.MethodPost {
				t.Errorf("Allow 头 = %q，期望 %q", got, http.MethodPost)
			}
		})
	}
}

func TestAdminHandlerRejectsBadBody(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"空体", ""},
		{"只有空白", "  \n\t "},
		// 超长输入必须被拒绝而不是截断：截断会得到一个看似合法的短路径，
		// 然后去重载一个完全不同的文件。
		{"超过上限", strings.Repeat("a", reloadPathLimit+1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := newTestAdminHandler(t, noopReload)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, AdminPath, strings.NewReader(tt.body)))

			if rec.Code != http.StatusBadRequest {
				t.Errorf("状态码 = %d，期望 400", rec.Code)
			}
		})
	}
}

func TestReadReloadPathAcceptsLimitExactly(t *testing.T) {
	// 路径恰好等于上限时必须被接受：多读一字节正是为了区分「恰好读满」与「超限」，
	// 这条断言守的就是那个边界。
	path := strings.Repeat("a", reloadPathLimit)
	req := httptest.NewRequest(http.MethodPost, AdminPath, strings.NewReader(path))

	got, err := readReloadPath(req)
	if err != nil {
		t.Fatalf("readReloadPath 报错：%v", err)
	}
	if got != path {
		t.Errorf("读回的路径长度 = %d，期望 %d", len(got), len(path))
	}
}

func TestReadReloadPathTrimsSurroundingSpace(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, AdminPath, strings.NewReader("  /etc/nova/Novafile \n"))

	got, err := readReloadPath(req)
	if err != nil {
		t.Fatalf("readReloadPath 报错：%v", err)
	}
	if want := "/etc/nova/Novafile"; got != want {
		t.Errorf("读回的路径 = %q，期望 %q", got, want)
	}
}

func TestAdminHandlerPassesPathToReload(t *testing.T) {
	var got string
	handler := newTestAdminHandler(t, func(_ context.Context, path string) error {
		got = path
		return nil
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, AdminPath, strings.NewReader("/etc/nova/Novafile")))

	if rec.Code != http.StatusOK {
		t.Errorf("状态码 = %d，期望 200", rec.Code)
	}
	if got != "/etc/nova/Novafile" {
		t.Errorf("投递给 reload 的路径 = %q，期望原样传递", got)
	}
}

// 失败一律 500，且响应体就是错误文本本身：错误里已经带了 文件:行:列，
// 调用方（nova reload）把它交给使用者即可，不需要再翻译一层状态码。
func TestAdminHandlerReportsReloadFailure(t *testing.T) {
	handler := newTestAdminHandler(t, func(context.Context, string) error {
		return errors.New("Novafile:3:5: 未知指令 \"log_levle\"")
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, AdminPath, strings.NewReader("Novafile")))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("状态码 = %d，期望 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Novafile:3:5") {
		t.Errorf("响应体 = %q，期望原样带回错误位置", rec.Body.String())
	}
}

// 重载串行化：两次请求不会并发进入装配流程，因此换入顺序与到达顺序一致。
func TestAdminHandlerSerializesReloads(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 2)

	handler := newTestAdminHandler(t, func(context.Context, string) error {
		entered <- struct{}{}
		<-release
		return nil
	})

	done := make(chan struct{}, 2)
	for i := 0; i < 2; i++ {
		go func() {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, AdminPath, strings.NewReader("Novafile")))
			done <- struct{}{}
		}()
	}

	<-entered
	select {
	case <-entered:
		t.Fatal("第二个重载在第一个还没做完时就进入了装配流程")
	default:
	}

	close(release)
	<-done
	<-done
}
