package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/nova/internal/config"
	"github.com/sumwai/nova/internal/domain"
	"github.com/sumwai/nova/internal/transport"
)

// newTestLogger 造一个写进缓冲区的输出口，方便断言排版。
func newTestLogger(t *testing.T, level, format string) (*logger, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	log, err := newLogger(&config.Config{LogLevel: level, LogFormat: format}, buf)
	if err != nil {
		t.Fatalf("newLogger 意外失败：%v", err)
	}
	return log, buf
}

func TestParseLevelAndFormatRejectUnknownValues(t *testing.T) {
	if _, err := parseLevel("verbose"); err == nil {
		t.Error("未登记的日志级别应当报错，而不是退回缺省")
	}
	if _, err := parseFormat("xml"); err == nil {
		t.Error("未登记的日志格式应当报错，而不是退回缺省")
	}
}

func TestStartupIgnoresLevelFilter(t *testing.T) {
	log, buf := newTestLogger(t, "error", "text")
	log.startup(&config.Config{
		Path: "/etc/nova/Novafile", Schema: 1, Listen: "127.0.0.1:8080",
		Admin: "localhost:2026", LogLevel: "error", LogFormat: "text",
	}, false)

	out := buf.String()
	// 横幅回答的是「这个进程在用哪份配置跑」，与「哪些日志值得看」是两件事：
	// 挂到 log_level 上会让 log_level error 的实例看不出自己监听在哪。
	if !strings.Contains(out, "nova 已启动") {
		t.Errorf("log_level error 时横幅仍应输出，实际：%q", out)
	}
	if !strings.Contains(out, "127.0.0.1:8080") {
		t.Errorf("横幅应报出监听地址，实际：%q", out)
	}
}

func TestAccessAndAttemptRespectLevelFilter(t *testing.T) {
	log, buf := newTestLogger(t, "warn", "text")
	log.LogAccess(transport.AccessRecord{HTTPStatus: 200})
	if buf.Len() != 0 {
		t.Errorf("2xx 的访问日志在 log_level warn 下应被丢弃，实际：%q", buf.String())
	}

	log.LogAccess(transport.AccessRecord{HTTPStatus: 502})
	if !strings.Contains(buf.String(), "ERROR") {
		t.Errorf("5xx 应记 error，实际：%q", buf.String())
	}

	buf.Reset()
	if err := log.RecordAttempt(context.Background(), domain.AttemptRecord{Outcome: domain.AttemptOK}); err != nil {
		t.Fatalf("RecordAttempt 不应报错：%v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("成功的尝试在 log_level warn 下应被丢弃，实际：%q", buf.String())
	}

	if err := log.RecordAttempt(context.Background(), domain.AttemptRecord{
		Outcome: domain.AttemptFailed,
	}); err != nil {
		t.Fatalf("RecordAttempt 不应报错：%v", err)
	}
	if !strings.Contains(buf.String(), "WARN") {
		t.Errorf("失败的尝试应记 warn，实际：%q", buf.String())
	}
}

func TestCancelledAttemptIsDebug(t *testing.T) {
	// 客户端主动断开是长流场景的常见结局：记成 warn 会把真正的故障淹没。
	if got := attemptLevel(domain.AttemptCancelled); got != slog.LevelDebug {
		t.Errorf("取消的尝试级别 = %v，期望 debug", got)
	}
}

func TestDisplayWidthCountsWideRunesAsTwoColumns(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{
		{in: "abc", want: 3},
		{in: "监听", want: 4},
		{in: "客户端鉴权", want: 10},
		{in: "a监", want: 3},
		{in: "", want: 0},
	}
	for _, tt := range tests {
		if got := displayWidth(tt.in); got != tt.want {
			t.Errorf("displayWidth(%q) = %d，期望 %d", tt.in, got, tt.want)
		}
	}
}

// valueColumn 返回一行里取值开始的显示列（从 1 数）。
//
// 不能用 strings.Index 找空格：标签与取值之间的填充宽度是算出来的，
// 找到的第一个空格只是填充的起点，不是取值的起点。
func valueColumn(line string) int {
	trimmed := strings.TrimLeft(line, " ")
	width := displayWidth(line) - displayWidth(trimmed)
	label := trimmed[:strings.Index(trimmed, " ")]
	width += displayWidth(label)
	tail := trimmed[len(label):]
	return width + displayWidth(tail) - displayWidth(strings.TrimLeft(tail, " "))
}

func TestStartupAlignsValuesByDisplayWidth(t *testing.T) {
	cfg := &config.Config{
		Path: "/etc/nova/Novafile", Schema: 1, Listen: "127.0.0.1:8080",
		Admin: "localhost:2026", LogLevel: "info", LogFormat: "text",
	}
	lines := strings.Split(renderStartup(cfg, false), "\n")
	if len(lines) < 2 {
		t.Fatalf("横幅应当是多行，实际：%q", lines)
	}
	// 所有取值必须从同一列开始。用 rune 数或字节数算都会被汉字带偏，
	// 而错位的横幅会让人以为某一行少了个字段。
	want := valueColumn(lines[1])
	for _, line := range lines[1:] {
		if got := valueColumn(line); got != want {
			t.Errorf("取值起始列 = %d，期望 %d：%q", got, want, line)
		}
	}
}

func TestJoinFieldsDropsEmptyPieces(t *testing.T) {
	// 被拒绝的请求没有 request_id、未注册路径没有协议名：空串若是照样占位，
	// 行里会冒出一串双空格，看起来像排版坏了。
	if got := joinFields("a", "", "b", ""); got != "a  b" {
		t.Errorf("joinFields = %q，期望 %q", got, "a  b")
	}
	if got := joinFields("", ""); got != "" {
		t.Errorf("全空时应得到空串，实际 %q", got)
	}
}

func TestDurationTextSwitchesUnitAtOneSecond(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{in: 0, want: "0ms"},
		{in: 843 * time.Millisecond, want: "843ms"},
		{in: 999 * time.Millisecond, want: "999ms"},
		{in: time.Second, want: "1.0s"},
		{in: 1234 * time.Millisecond, want: "1.2s"},
	}
	for _, tt := range tests {
		if got := durationText(tt.in); got != tt.want {
			t.Errorf("durationText(%v) = %q，期望 %q", tt.in, got, tt.want)
		}
	}
}

func TestUsageTextOmitsZeroSubitems(t *testing.T) {
	known := domain.Usage{Source: domain.UsageSourceUpstream, InputTokens: 9, OutputTokens: 12}
	if got := usageText(known); got != "9/12 tok" {
		t.Errorf("usageText = %q，期望只写主计数", got)
	}

	withSub := domain.Usage{
		Source: domain.UsageSourceUpstream, InputTokens: 9, OutputTokens: 12,
		CacheReadTokens: 5, ReasoningTokens: 3,
	}
	got := usageText(withSub)
	if !strings.Contains(got, "cache 5/0") || !strings.Contains(got, "reason 3") {
		t.Errorf("usageText = %q，期望带上非零子项", got)
	}
	if strings.Contains(got, "cache 0/0") {
		t.Errorf("usageText = %q，零值子项不该出现", got)
	}

	// 用量未知时写 unknown 而不是 0/0：后者看起来像「这次请求不花钱」，
	// 与事实正好相反。
	if got := usageText(domain.Usage{}); got != "usage unknown" {
		t.Errorf("未知用量 = %q，期望 usage unknown", got)
	}
}

func TestRenderAttemptShowsArrowsOnlyOnChange(t *testing.T) {
	base := domain.AttemptRecord{
		RequestID: "req1", Attempt: 1, UpstreamID: "relay api.example.com",
		ClientProtocol: domain.ProtocolOpenAIChat, UpstreamProtocol: domain.ProtocolOpenAIChat,
		RequestedModel: "gpt-5", UpstreamModel: "gpt-5",
		Outcome: domain.AttemptOK, StartedAt: time.Now(), EndedAt: time.Now().Add(time.Second),
	}
	if got := renderAttempt(base, false); strings.Contains(got, "→") {
		t.Errorf("同协议同模型时不该出现箭头：%q", got)
	}

	switched := base
	switched.ClientProtocol = domain.ProtocolAnthropicMessages
	switched.UpstreamModel = "gpt-4o"
	got := renderAttempt(switched, false)
	if !strings.Contains(got, "anthropic_messages→openai_chat") {
		t.Errorf("跨协议时应写出两侧协议：%q", got)
	}
	if !strings.Contains(got, "gpt-5→gpt-4o") {
		t.Errorf("改写模型时应写出两侧模型名：%q", got)
	}
}

func TestRenderAttemptPutsErrorDetailOnItsOwnRecord(t *testing.T) {
	rec := domain.AttemptRecord{
		RequestID: "req1", Attempt: 2, UpstreamID: "relay api.example.com",
		ClientProtocol: domain.ProtocolOpenAIChat, UpstreamProtocol: domain.ProtocolOpenAIChat,
		RequestedModel: "gpt-5", UpstreamModel: "gpt-5",
		Outcome: domain.AttemptFailed, ErrorCode: "upstream_rate_limited",
		ErrorDetail: "上游 HTTP 状态码 429：{}",
		StartedAt:   time.Now(), EndedAt: time.Now(),
	}
	lines := strings.Split(renderAttempt(rec, false), "\n")
	if len(lines) != 2 {
		t.Fatalf("失败尝试应占两条记录（attempt + 上游明细），实际 %d 行：%q", len(lines), lines)
	}
	// 明细带自己的时间与级别，因此能被按级别单独筛出来；它也因此不能是缩进的尾巴。
	detail := lines[1]
	if !strings.Contains(detail, "ERROR") {
		t.Errorf("明细行的级别应为 ERROR：%q", detail)
	}
	if !strings.Contains(detail, "upstream") {
		t.Errorf("明细行应有自己的消息名：%q", detail)
	}
	if strings.HasPrefix(detail, " ") {
		t.Errorf("明细行不该缩进：%q", detail)
	}
	if !strings.Contains(detail, "429") {
		t.Errorf("明细行应带上游状态码：%q", detail)
	}
	// 带上 request_id 与尝试序号，才能在一堆日志里认出它属于哪一次尝试。
	if !strings.Contains(detail, "req1") || !strings.Contains(detail, "#2") {
		t.Errorf("明细行应带上关联键：%q", detail)
	}
}

func TestAttemptWithoutErrorDetailIsOneLine(t *testing.T) {
	rec := domain.AttemptRecord{
		RequestID: "req1", Attempt: 1, UpstreamID: "relay api.example.com",
		Outcome: domain.AttemptOK, StartedAt: time.Now(), EndedAt: time.Now(),
	}
	if got := renderAttempt(rec, false); strings.Contains(got, "\n") {
		t.Errorf("成功尝试应是单行，实际 %q", got)
	}
}

func TestColorIsOffWhenDisabled(t *testing.T) {
	log, buf := newTestLogger(t, "info", "text")
	log.color = false
	log.LogAccess(transport.AccessRecord{HTTPStatus: 500, Model: "m"})
	if strings.Contains(buf.String(), "\x1b[") {
		t.Errorf("color 为假时不该出现转义序列：%q", buf.String())
	}

	buf.Reset()
	log.color = true
	log.LogAccess(transport.AccessRecord{HTTPStatus: 500, Model: "m"})
	if !strings.Contains(buf.String(), "\x1b[") {
		t.Errorf("color 为真时级别应当着色：%q", buf.String())
	}
}

func TestJSONPayloadGroupsUsage(t *testing.T) {
	log, buf := newTestLogger(t, "debug", "json")
	err := log.RecordAttempt(context.Background(), domain.AttemptRecord{
		RequestID: "req1", Attempt: 2, UpstreamID: "relay api.example.com",
		ClientProtocol: domain.ProtocolAnthropicMessages, UpstreamProtocol: domain.ProtocolOpenAIChat,
		RequestedModel: "claude-4", UpstreamModel: "gpt-4o",
		Outcome:   domain.AttemptFailed,
		Usage:     domain.Usage{Source: domain.UsageSourceUpstream, InputTokens: 9, OutputTokens: 12},
		ErrorCode: "upstream_unavailable",
		StartedAt: time.Now(), EndedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("RecordAttempt 不应报错：%v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("json 模式应当每条一行合法 JSON，实际 %q：%v", buf.String(), err)
	}
	usage, ok := got["usage"].(map[string]any)
	if !ok {
		t.Fatalf("用量应当是分组对象，实际 %v", got["usage"])
	}
	if usage["input_tokens"] != float64(9) || usage["output_tokens"] != float64(12) {
		t.Errorf("用量分组内容不对：%v", usage)
	}
	// 与 text 不同，json 不省略任何事实：两侧协议、两侧模型名与转换标记都在。
	if got["protocol_switched"] != true {
		t.Errorf("protocol_switched = %v，期望 true", got["protocol_switched"])
	}
	if got["model"] != "claude-4" || got["upstream_model"] != "gpt-4o" {
		t.Errorf("两个模型名都应保留：%v / %v", got["model"], got["upstream_model"])
	}
	if got["error_code"] != "upstream_unavailable" {
		t.Errorf("error_code = %v", got["error_code"])
	}
}

func TestJSONPayloadOmitsUnknownUsage(t *testing.T) {
	log, buf := newTestLogger(t, "debug", "json")
	_ = log.RecordAttempt(context.Background(), domain.AttemptRecord{
		Outcome: domain.AttemptOK, StartedAt: time.Now(), EndedAt: time.Now(),
	})
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	// 全零的 usage 对象看起来像「这次没花钱」，与「上游没告诉我们」是两件相反的事。
	if _, exists := got["usage"]; exists {
		t.Errorf("用量未知时不该有 usage 分组：%v", got["usage"])
	}
}

func TestJSONStartupCarriesCountsAndAuth(t *testing.T) {
	log, buf := newTestLogger(t, "info", "json")
	log.startup(&config.Config{
		Path: "/etc/nova/Novafile", Schema: 1, Listen: "127.0.0.1:8080",
		Admin: "localhost:2026", LogLevel: "info", LogFormat: "json",
		ClientKeys: []string{"k"},
		Providers: []config.Provider{{
			Name: "p",
			Endpoints: []config.Endpoint{{
				URL:    "https://a.example.com/v1/chat/completions",
				Models: []config.Model{{Name: "m1"}, {Name: "m2"}},
			}},
		}},
	}, false)

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if got["msg"] != "startup" {
		t.Errorf("msg = %v，期望 startup", got["msg"])
	}
	if got["providers"] != float64(1) || got["models"] != float64(2) {
		t.Errorf("渠道数与模型数不对：%v / %v", got["providers"], got["models"])
	}
	if got["client_auth"] != true {
		t.Errorf("client_auth = %v，期望 true", got["client_auth"])
	}
}
