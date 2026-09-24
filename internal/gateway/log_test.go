package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

func TestAccessRespectsLevelFilter(t *testing.T) {
	log, buf := newTestLogger(t, "warn", "text")
	log.LogAccess(transport.AccessRecord{HTTPStatus: 200})
	if buf.Len() != 0 {
		t.Errorf("2xx 的访问日志在 log_level warn 下应被丢弃，实际：%q", buf.String())
	}

	log.LogAccess(transport.AccessRecord{HTTPStatus: 502})
	if !strings.Contains(buf.String(), "ERROR") {
		t.Errorf("5xx 应记 error，实际：%q", buf.String())
	}
}

// 尝试记录先暂存，等访问日志到达时成块输出。上游尝试缺少的信息（协议、模型、
// 客户端结果）都在主行上，先单独打出明细只会在紧随其后的主行里再抄一遍。
func TestAttemptWaitsForAccessAndFormsBlock(t *testing.T) {
	log, buf := newTestLogger(t, "info", "text")
	moment := time.Now()
	rec := domain.AttemptRecord{
		RequestID: "req1", Attempt: 1, UpstreamID: "relay api.example.com",
		ClientProtocol: domain.ProtocolOpenAIChat, UpstreamProtocol: domain.ProtocolOpenAIChat,
		RequestedModel: "gpt-5", UpstreamModel: "gpt-5",
		Outcome:   domain.AttemptOK,
		Usage:     domain.Usage{Source: domain.UsageSourceUpstream, InputTokens: 35, OutputTokens: 442, ReasoningTokens: 378},
		StartedAt: moment, EndedAt: moment,
	}
	if err := log.RecordAttempt(context.Background(), rec); err != nil {
		t.Fatalf("RecordAttempt 不应报错：%v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("访问日志到达之前不该有输出，实际：%q", buf.String())
	}

	log.LogAccess(transport.AccessRecord{
		RequestID: "req1", Protocol: domain.ProtocolOpenAIChat, Model: "gpt-5",
		Stream: true, HTTPStatus: 200, DurationMS: 3700,
		RemoteAddr: "127.0.0.1:1234", UserAgent: "curl/8.22.0",
	})
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("单次成功的尝试应当折叠成一行，实际 %d 行：%q", len(lines), lines)
	}
	for _, want := range []string{
		"access", "req1", "gpt-5", "200", "3.7s",
		"35/442 tok", "reason 378", "via relay api.example.com", "curl/8.22.0",
	} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("折叠后的主行缺少 %q：%q", want, lines[0])
		}
	}
}

// 200 请求里失败过的尝试仍要输出：折叠成主行会让「重试过一次并且失败过」这条
// 事实随主行的 info 一起消失。
func TestFoldedMainLineStillShowsFailedAttempt(t *testing.T) {
	log, buf := newTestLogger(t, "info", "text")
	moment := time.Now()
	base := domain.AttemptRecord{
		RequestID: "req1", UpstreamID: "relay api.example.com",
		ClientProtocol: domain.ProtocolOpenAIChat, UpstreamProtocol: domain.ProtocolOpenAIChat,
		RequestedModel: "gpt-5", UpstreamModel: "gpt-5",
		StartedAt: moment, EndedAt: moment,
	}
	failed := base
	failed.Attempt = 1
	failed.Outcome = domain.AttemptFailed
	failed.ErrorCode = "upstream_timeout"
	succeeded := base
	succeeded.Attempt = 2
	succeeded.Outcome = domain.AttemptOK
	_ = log.RecordAttempt(context.Background(), failed)
	_ = log.RecordAttempt(context.Background(), succeeded)
	log.LogAccess(transport.AccessRecord{RequestID: "req1", HTTPStatus: 200})

	out := buf.String()
	if !strings.Contains(out, "tries 2") {
		t.Errorf("主行应交代经历了两次尝试：%q", out)
	}
	if !strings.Contains(out, "failed") || !strings.Contains(out, "upstream_timeout") {
		t.Errorf("失败过的那次尝试应保留 outcome 与错误码：%q", out)
	}
	if !strings.Contains(out, "#2") {
		t.Errorf("成功的第二次尝试也应有明细行：%q", out)
	}
}

// 上游持续限流时逐字相同的报文会连刷几十行；同类失败在一个窗口内只留首条全文，
// 其余折叠计数。主行是请求级记录，每次请求都要留。
func TestRepeatedUpstreamFailureIsSuppressed(t *testing.T) {
	moment := time.Now()
	log, buf := newTestLogger(t, "info", "text")
	log.clock = func() time.Time { return moment }

	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("req%d", i)
		_ = log.RecordAttempt(context.Background(), domain.AttemptRecord{
			RequestID: id, Attempt: 1, UpstreamID: "relay api.example.com",
			ClientProtocol: domain.ProtocolOpenAIChat, UpstreamProtocol: domain.ProtocolOpenAIChat,
			RequestedModel: "gpt-5", UpstreamModel: "gpt-5",
			Outcome: domain.AttemptFailed, ErrorCode: "upstream_rate_limited",
			ErrorDetail: "上游 HTTP 状态码 429：{}",
			StartedAt:   moment, EndedAt: moment,
		})
		log.LogAccess(transport.AccessRecord{
			RequestID: id, HTTPStatus: 503, ErrorCode: "upstream_rate_limited",
		})
		moment = moment.Add(time.Second)
	}

	out := buf.String()
	if got := strings.Count(out, "access"); got != 4 {
		t.Errorf("主行数 = %d，期望 4：%q", got, out)
	}
	if got := strings.Count(out, "上游 HTTP 状态码 429"); got != 1 {
		t.Errorf("同一类失败的报文明文应只出现一次，实际 %d 次：%q", got, out)
	}
}

func TestSuppressedCountIsFlushedWhenWindowExpires(t *testing.T) {
	moment := time.Now()
	log, buf := newTestLogger(t, "info", "text")
	log.clock = func() time.Time { return moment }
	log.window = 5 * time.Second

	attempt := func(i int) {
		id := fmt.Sprintf("req%d", i)
		_ = log.RecordAttempt(context.Background(), domain.AttemptRecord{
			RequestID: id, Attempt: 1, UpstreamID: "relay api.example.com",
			Outcome: domain.AttemptFailed, ErrorCode: "upstream_rate_limited",
			ErrorDetail: "上游 HTTP 状态码 429：{}",
			StartedAt:   moment, EndedAt: moment,
		})
		log.LogAccess(transport.AccessRecord{RequestID: id, HTTPStatus: 503, ErrorCode: "upstream_rate_limited"})
	}
	attempt(0)
	moment = moment.Add(time.Second)
	attempt(1)
	moment = moment.Add(time.Second)
	attempt(2)
	if strings.Contains(buf.String(), "同类上游失败已折叠") {
		t.Fatalf("窗口未过期时不该出现汇总行：%q", buf.String())
	}

	// 窗口过期后，下一条日志到达时补出上一窗口被压掉的量级。
	moment = moment.Add(10 * time.Second)
	log.LogAccess(transport.AccessRecord{HTTPStatus: 200})
	if !strings.Contains(buf.String(), "×2") {
		t.Errorf("窗口结束后应补出被折叠的计数：%q", buf.String())
	}
}

func TestAttemptLineLevelTakesHigherSide(t *testing.T) {
	cancelled := domain.AttemptRecord{Outcome: domain.AttemptCancelled}
	// 取消记 debug 是为了不在长流场景刷屏，跟主行走会把这条取舍作废。
	if got := attemptLineLevel(cancelled, 200); got != slog.LevelDebug {
		t.Errorf("取消的明细应保持 debug，实际 %v", got)
	}
	if got := attemptLineLevel(cancelled, 503); got != slog.LevelDebug {
		t.Errorf("主行 error 也不抬升取消明细，实际 %v", got)
	}
	failed := domain.AttemptRecord{Outcome: domain.AttemptFailed}
	if got := attemptLineLevel(failed, 200); got != slog.LevelWarn {
		t.Errorf("主行 info 时失败明细仍是 warn，实际 %v", got)
	}
	if got := attemptLineLevel(failed, 503); got != slog.LevelError {
		t.Errorf("主行 error 时明细不能低一档，实际 %v", got)
	}
}

func TestOutcomeTextDropsRedundantConclusion(t *testing.T) {
	tests := []struct {
		outcome  domain.AttemptOutcome
		status   int
		multiple bool
		want     string
	}{
		{domain.AttemptOK, 200, false, ""},
		{domain.AttemptFailed, 503, false, ""},
		{domain.AttemptFailed, 400, false, ""},
		{domain.AttemptCancelled, 200, false, "cancelled"},
		{domain.AttemptOK, 503, false, "ok"},
		{domain.AttemptFailed, 200, false, "failed"},
		{domain.AttemptOK, 200, true, "ok"},
	}
	for _, tt := range tests {
		if got := attemptOutcomeText(tt.outcome, tt.status, tt.multiple); got != tt.want {
			t.Errorf("attemptOutcomeText(%q, %d, %v) = %q，期望 %q",
				tt.outcome, tt.status, tt.multiple, got, tt.want)
		}
	}
}

func TestShortRequestIDKeepsPrefix(t *testing.T) {
	id := "fc6e4c3e7afcca77d265e8224f052db0"
	if got := shortRequestID(id); got != "fc6e4c3e" {
		t.Errorf("shortRequestID = %q，期望 %q", got, "fc6e4c3e")
	}
	// 截断值必须是完整值的前缀，否则按前缀 grep 就找不到原来的记录。
	if !strings.HasPrefix(id, shortRequestID(id)) {
		t.Error("截断值应是完整关联键的前缀")
	}
	if got := shortRequestID("abc"); got != "abc" {
		t.Errorf("短于上限的关联键不该改动，实际 %q", got)
	}
}

// fieldColumn 返回一行里锚点字段开始的显示列（从 1 数）。
//
// 不能用双空格切分前缀：级别标签 WARN 自带尾随空格，与分隔用的两个空格连起来会让
// 切分点提前落到级别列里，算出的列宽因此随级别变化。
func fieldColumn(t *testing.T, line, anchor string) int {
	t.Helper()
	idx := strings.Index(line, anchor)
	if idx < 0 {
		t.Fatalf("行里找不到锚点 %q：%q", anchor, line)
	}
	return displayWidth(line[:idx])
}

// 消息名不等宽会让字段随记录种类左右漂移，而「一眼扫过一屏」正是固定列要买的东西。
func TestKindColumnIsFixedWidth(t *testing.T) {
	for _, kind := range []string{kindAccess, kindUpstream, kindReload, kindWarning} {
		if got := displayWidth(kindColumn(kind)); got != kindWidth+4 {
			t.Errorf("kindColumn(%q) 显示宽度 = %d，期望 %d", kind, got, kindWidth+4)
		}
	}
}

// 三种消息名的宽度不同（access、reload、upstream），但字段必须从同一列开始。
func TestKindColumnAlignsFieldStart(t *testing.T) {
	moment := time.Now()
	log, buf := newTestLogger(t, "debug", "text")
	log.clock = func() time.Time { return moment }

	log.reloadApplied("/etc/nova/Novafile")
	_ = log.RecordAttempt(context.Background(), domain.AttemptRecord{
		RequestID: "req1", Attempt: 1, UpstreamID: "relay api.example.com",
		Outcome: domain.AttemptFailed, ErrorCode: "upstream_rate_limited",
		StartedAt: moment, EndedAt: moment,
	})
	log.LogAccess(transport.AccessRecord{RequestID: "req1", HTTPStatus: 503, ErrorCode: "upstream_rate_limited"})

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("期望 1 条 reload 加 1 个两行块，实际 %d 行：%q", len(lines), lines)
	}
	want := fieldColumn(t, lines[0], "/etc/nova/Novafile")
	for _, line := range lines[1:] {
		if got := fieldColumn(t, line, "req1"); got != want {
			t.Errorf("字段起始列 = %d，期望 %d：%q", got, want, line)
		}
	}
}

func TestAttemptWithoutRequestIDPrintsImmediately(t *testing.T) {
	log, buf := newTestLogger(t, "info", "text")
	moment := time.Now()
	// 没有关联键的尝试无法与任何访问日志配对：宁可失去成块排版，也不把记录丢掉。
	_ = log.RecordAttempt(context.Background(), domain.AttemptRecord{
		Attempt: 1, UpstreamID: "relay api.example.com", Outcome: domain.AttemptOK,
		StartedAt: moment, EndedAt: moment,
	})
	if buf.Len() == 0 {
		t.Error("没有关联键的尝试应立刻按明细行输出")
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

func TestAttemptDetailShowsArrowsOnlyOnChange(t *testing.T) {
	base := domain.AttemptRecord{
		RequestID: "req1", Attempt: 1, UpstreamID: "relay api.example.com",
		ClientProtocol: domain.ProtocolOpenAIChat, UpstreamProtocol: domain.ProtocolOpenAIChat,
		RequestedModel: "gpt-5", UpstreamModel: "gpt-5",
		Outcome: domain.AttemptOK, StartedAt: time.Now(), EndedAt: time.Now().Add(time.Second),
	}
	if got := renderAttemptDetail("13:20:37", base, false, 200, false); strings.Contains(got, "→") {
		t.Errorf("同协议同模型时不该出现箭头：%q", got)
	}

	switched := base
	switched.ClientProtocol = domain.ProtocolAnthropicMessages
	switched.UpstreamModel = "gpt-4o"
	got := renderAttemptDetail("13:20:37", switched, false, 200, false)
	if !strings.Contains(got, "anthropic_messages→openai_chat") {
		t.Errorf("跨协议时应写出两侧协议：%q", got)
	}
	if !strings.Contains(got, "gpt-5→gpt-4o") {
		t.Errorf("改写模型时应写出两侧模型名：%q", got)
	}
}

// 明细行只写主行没有的事实。单次失败的结论、耗时与错误码都在主行上，重抄一遍会带来
// 「同一个名字给了两个数」（入口耗时与上游耗时并不严格相等）。
func TestFailedDetailOmitsMainLineFacts(t *testing.T) {
	moment := time.Now()
	rec := domain.AttemptRecord{
		RequestID: "req1", Attempt: 1, UpstreamID: "relay api.example.com",
		ClientProtocol: domain.ProtocolOpenAIChat, UpstreamProtocol: domain.ProtocolOpenAIChat,
		RequestedModel: "gpt-5", UpstreamModel: "gpt-5",
		Outcome: domain.AttemptFailed, ErrorCode: "upstream_rate_limited",
		ErrorDetail: "上游 HTTP 状态码 429：{}",
		StartedAt:   moment, EndedAt: moment.Add(198 * time.Millisecond),
	}
	line := renderAttemptDetail("13:19:27", rec, false, 503, false)
	for _, unwanted := range []string{"198ms", "upstream_rate_limited", "failed"} {
		if strings.Contains(line, unwanted) {
			t.Errorf("明细行不该重抄主行承载的 %q：%q", unwanted, line)
		}
	}
	for _, want := range []string{"relay api.example.com", "429", "#1", "req1"} {
		if !strings.Contains(line, want) {
			t.Errorf("明细行缺少 %q：%q", want, line)
		}
	}
	if strings.Contains(line, "\n") {
		t.Errorf("明细行应当是一行：%q", line)
	}
}

func TestMultipleAttemptsWriteOutcomeAndDuration(t *testing.T) {
	moment := time.Now()
	log, buf := newTestLogger(t, "info", "text")
	base := domain.AttemptRecord{
		RequestID: "req1", UpstreamID: "relay api.example.com",
		ClientProtocol: domain.ProtocolOpenAIChat, UpstreamProtocol: domain.ProtocolOpenAIChat,
		RequestedModel: "gpt-5", UpstreamModel: "gpt-5",
	}
	first := base
	first.Attempt, first.Outcome, first.ErrorCode = 1, domain.AttemptFailed, "upstream_timeout"
	first.StartedAt, first.EndedAt = moment, moment.Add(4*time.Second)
	second := base
	second.Attempt, second.Outcome = 2, domain.AttemptOK
	second.StartedAt, second.EndedAt = moment.Add(4*time.Second), moment.Add(8*time.Second)
	_ = log.RecordAttempt(context.Background(), first)
	_ = log.RecordAttempt(context.Background(), second)
	log.LogAccess(transport.AccessRecord{RequestID: "req1", HTTPStatus: 200, DurationMS: 8100})

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("多次尝试应占三行，实际 %d 行：%q", len(lines), lines)
	}
	if !strings.Contains(lines[0], "tries 2") {
		t.Errorf("主行应交代尝试次数：%q", lines[0])
	}
	for _, want := range []string{"failed", "4.0s", "upstream_timeout"} {
		if !strings.Contains(lines[1], want) {
			t.Errorf("首次尝试的明细缺少 %q：%q", want, lines[1])
		}
	}
	for _, want := range []string{"ok", "4.0s"} {
		if !strings.Contains(lines[2], want) {
			t.Errorf("第二次尝试的明细缺少 %q：%q", want, lines[2])
		}
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
	// 全零的 usage 对象看起来像「这次没花钱」，与「上游没有给出用量」是两件相反的事。
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
