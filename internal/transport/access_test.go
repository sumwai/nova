package transport

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sumwai/nova/internal/adapters/openaichat"
	"github.com/sumwai/nova/internal/domain"
)

// stubForwarder 是不做任何转发的转发器：本文件考察入口层记录了什么，不涉及流水线行为。
type stubForwarder struct{}

func (stubForwarder) Forward(context.Context, domain.Adapter, *domain.Request, io.Writer) error {
	return nil
}

// capturingLogger 收集入口层交出的访问记录。
type capturingLogger struct{ records []AccessRecord }

func (c *capturingLogger) LogAccess(record AccessRecord) { c.records = append(c.records, record) }

// TestServeHTTPRecordsClientFacts 守护访问记录里的客户端侧事实：
// User-Agent 取自请求并按上限截断，协议与模型名分别取自路径与请求体。
func TestServeHTTPRecordsClientFacts(t *testing.T) {
	logger := &capturingLogger{}
	handler, err := New(Options{
		Forwarder: stubForwarder{},
		Adapters: func(r *http.Request) (domain.Adapter, bool) {
			if r.URL.Path == "/v1/chat/completions" {
				return openaichat.New(), true
			}
			return nil, false
		},
		Logger: logger,
	})
	if err != nil {
		t.Fatalf("构造入口失败: %v", err)
	}

	longUserAgent := strings.Repeat("u", maxUserAgentRunes+10)
	body := `{"model":"glm-5.3-flash","messages":[{"role":"user","content":"hi"}]}`
	request := httptest.NewRequestWithContext(context.Background(),
		http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	request.Header.Set("User-Agent", longUserAgent)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if len(logger.records) != 1 {
		t.Fatalf("应写出恰好一条访问记录，实际 %d 条", len(logger.records))
	}
	record := logger.records[0]
	if record.Protocol != domain.ProtocolOpenAIChat {
		t.Errorf("客户端协议 = %q，期望 %q", string(record.Protocol), string(domain.ProtocolOpenAIChat))
	}
	if record.Model != "glm-5.3-flash" {
		t.Errorf("客户端请求的模型名 = %q，期望 glm-5.3-flash", record.Model)
	}
	if record.HTTPStatus != http.StatusOK {
		t.Errorf("HTTP 状态 = %d，期望 200", record.HTTPStatus)
	}
	if got := len([]rune(record.UserAgent)); got != maxUserAgentRunes {
		t.Errorf("User-Agent 应按上限截断到 %d 个字符，实际 %d", maxUserAgentRunes, got)
	}
}

// TestTruncateUserAgent 守护按字符（而非字节）截断：
// 多字节的 User-Agent 不能被截成半个字符，否则日志里会出现乱码。
func TestTruncateUserAgent(t *testing.T) {
	short := "curl/8.5.0"
	if got := truncateUserAgent(short); got != short {
		t.Errorf("未超限的 User-Agent 不应被改动：%q", got)
	}

	exact := strings.Repeat("a", maxUserAgentRunes)
	if got := truncateUserAgent(exact); got != exact {
		t.Errorf("恰好达到上限的 User-Agent 不应被截断，实际长度 %d", len([]rune(got)))
	}

	over := strings.Repeat("中", maxUserAgentRunes+5)
	got := truncateUserAgent(over)
	if len([]rune(got)) != maxUserAgentRunes {
		t.Errorf("超限的 User-Agent 应截断到 %d 个字符，实际 %d", maxUserAgentRunes, len([]rune(got)))
	}
	if !utf8.ValidString(got) {
		t.Error("截断后的 User-Agent 必须是合法 UTF-8")
	}
}
