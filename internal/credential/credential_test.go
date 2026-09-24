package credential

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/sumwai/nova/internal/domain"
	"github.com/sumwai/nova/internal/upstream"
)

// 编译期断言：Provider 必须满足 upstream.HeaderProvider 的契约。
//
// 这里刻意导入 internal/upstream 而不是只做结构断言：契约的权威定义在 upstream 包，
// 结构断言只能复制一份接口形状，二者一旦漂移仍会编译通过。upstream 不导入本包，
// 该依赖不成环，因此用真实接口锁住实现更可靠。
var _ upstream.HeaderProvider = (*Provider)(nil)

// singleRef 构造只含一条凭据的提供者，供只关心注入形态的用例复用。
func singleRef(ref string, cred Credential) *Provider {
	return New(map[string][]Account{ref: {{Ref: "#1", APIKey: cred.APIKey}}})
}

// TestUpstreamHeadersInjectsPerProtocol 覆盖三个协议各自的凭据注入形态。
//
// 注入形态取自路由的协议（凭据只提供密钥），因此每一行都让 Route.Protocol 与期望的头形态对应。
func TestUpstreamHeadersInjectsPerProtocol(t *testing.T) {
	tests := []struct {
		name     string
		protocol domain.Protocol
		apiKey   string
		wantName string
		wantVal  string
	}{
		{
			name:     "OpenAI Chat 用 Authorization Bearer",
			protocol: domain.ProtocolOpenAIChat,
			apiKey:   "sk-chat",
			wantName: "Authorization",
			wantVal:  "Bearer sk-chat",
		},
		{
			name:     "OpenAI Responses 用 Authorization Bearer",
			protocol: domain.ProtocolOpenAIResponses,
			apiKey:   "sk-resp",
			wantName: "Authorization",
			wantVal:  "Bearer sk-resp",
		},
		{
			name:     "Anthropic Messages 用 x-api-key",
			protocol: domain.ProtocolAnthropicMessages,
			apiKey:   "sk-ant",
			wantName: "x-api-key",
			wantVal:  "sk-ant",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := singleRef("ref", Credential{APIKey: tt.apiKey})
			headers, err := provider.UpstreamHeaders(context.Background(),
				domain.Route{CredentialRef: "ref", Protocol: tt.protocol})
			if err != nil {
				t.Fatalf("UpstreamHeaders 返回错误：%v", err)
			}
			if got := headers.Get(tt.wantName); got != tt.wantVal {
				t.Fatalf("%s = %q，期望 %q", tt.wantName, got, tt.wantVal)
			}
			// 形态互斥：注入了 Authorization 就不应同时出现 x-api-key，反之亦然。
			other := "Authorization"
			if tt.wantName == other {
				other = "x-api-key"
			}
			if got := headers.Get(other); got != "" {
				t.Fatalf("不应出现 %s，实际为 %q", other, got)
			}
		})
	}
}

// TestUpstreamHeadersProtocolDrivesHeaderStyle 把「用哪个头」这件事完全钉在路由协议上。
//
// 一份凭据可以服务走不同协议的多个端点，注入形态因此不能来自凭据自身，只能来自本次路由。
// 表里三行分别确认两个 OpenAI 系协议走 Authorization: Bearer、Anthropic 走 x-api-key；
// 另外两行确认未识别协议与空协议都不注入任何头，并以平台内部错误失败——
// 空协议是装配缺陷（合成路由漏带协议）的典型形态，宁可显式失败也不猜一种形态：
// 猜错会把密钥送进错误的请求头，而那是静默发生的。
func TestUpstreamHeadersProtocolDrivesHeaderStyle(t *testing.T) {
	const apiKey = "sk-style"
	tests := []struct {
		name       string
		protocol   domain.Protocol
		wantName   string
		wantVal    string
		wantNoHead bool
	}{
		{
			name:     "openai_chat 走 Authorization Bearer",
			protocol: domain.ProtocolOpenAIChat,
			wantName: "Authorization",
			wantVal:  "Bearer " + apiKey,
		},
		{
			name:     "openai_responses 走 Authorization Bearer",
			protocol: domain.ProtocolOpenAIResponses,
			wantName: "Authorization",
			wantVal:  "Bearer " + apiKey,
		},
		{
			name:     "anthropic_messages 走 x-api-key",
			protocol: domain.ProtocolAnthropicMessages,
			wantName: "x-api-key",
			wantVal:  apiKey,
		},
		{
			name:       "未识别协议不注入任何头",
			protocol:   domain.Protocol("gemini_generate"),
			wantNoHead: true,
		},
		{
			name:       "空协议不注入任何头",
			protocol:   domain.Protocol(""),
			wantNoHead: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := singleRef("ref", Credential{APIKey: apiKey})
			headers, err := provider.UpstreamHeaders(context.Background(),
				domain.Route{CredentialRef: "ref", Protocol: tt.protocol})
			if !tt.wantNoHead {
				if err != nil {
					t.Fatalf("协议 %q 应能注入凭据头，实际报错：%v", string(tt.protocol), err)
				}
				if got := headers.Get(tt.wantName); got != tt.wantVal {
					t.Fatalf("%q = %q，期望 %q", tt.wantName, got, tt.wantVal)
				}
				return
			}
			if err == nil {
				t.Fatalf("协议 %q 不支持凭据注入，应返回错误", string(tt.protocol))
			}
			if headers != nil {
				t.Fatalf("不支持注入时不得返回任何头，实际 %v", headers)
			}
			domainErr := domain.AsError(err)
			if domainErr == nil {
				t.Fatalf("错误应为 domain.Error，实际 %T", err)
			}
			if domainErr.Code != domain.CodeInternal {
				t.Errorf("错误码 = %q，期望 %q", domainErr.Code, domain.CodeInternal)
			}
		})
	}
}

// TestUpstreamHeadersSelectsCredentialByRef 守护「按 route.CredentialRef 选凭据」：
//
// 一份配置里可以有多个上游渠道，每个渠道一份密钥。请求头必须按本次尝试的路由取出对应的那一份，
// 并且注入形态按该路由自己的协议决定——全局唯一的密钥或全局唯一的协议
// 都会让其中一个渠道拿到错误的密钥或错误的头名。
func TestUpstreamHeadersSelectsCredentialByRef(t *testing.T) {
	provider := New(map[string][]Account{
		"openai": {{Ref: "#1", APIKey: "sk-openai"}},
		"claude": {{Ref: "#1", APIKey: "sk-claude"}},
	})

	openaiHeaders, err := provider.UpstreamHeaders(context.Background(),
		domain.Route{CredentialRef: "openai", Protocol: domain.ProtocolOpenAIChat})
	if err != nil {
		t.Fatalf("取 openai 渠道的请求头失败：%v", err)
	}
	if got := openaiHeaders.Get("Authorization"); got != "Bearer sk-openai" {
		t.Errorf("openai 渠道的 Authorization = %q，期望 %q", got, "Bearer sk-openai")
	}
	if got := openaiHeaders.Get("x-api-key"); got != "" {
		t.Errorf("openai 渠道不应带 x-api-key，实际 %q", got)
	}

	claudeHeaders, err := provider.UpstreamHeaders(context.Background(),
		domain.Route{CredentialRef: "claude", Protocol: domain.ProtocolAnthropicMessages})
	if err != nil {
		t.Fatalf("取 claude 渠道的请求头失败：%v", err)
	}
	if got := claudeHeaders.Get("x-api-key"); got != "sk-claude" {
		t.Errorf("claude 渠道的 x-api-key = %q，期望 %q", got, "sk-claude")
	}
	if got := claudeHeaders.Get("Authorization"); got != "" {
		t.Errorf("claude 渠道不应带 Authorization，实际 %q", got)
	}
}

// TestUpstreamHeadersUnknownRefReturnsError 守护未知引用返回错误而不是空密钥。
//
// 静默注入空密钥会把「装配时漏了一个渠道的凭据」变成上游的 401，
// 使用者从一个与配置无关的症状出发排查，很难回到真正的原因。
// 空引用同样报错：它同样表示「这条路由没有可用的凭据」，而不是「用空密钥试试」。
func TestUpstreamHeadersUnknownRefReturnsError(t *testing.T) {
	provider := singleRef("openai", Credential{APIKey: "sk-openai"})

	for _, ref := range []string{"missing", ""} {
		headers, err := provider.UpstreamHeaders(context.Background(), domain.Route{CredentialRef: ref})
		if err == nil {
			t.Fatalf("凭据引用 %q 应返回错误", ref)
		}
		if headers != nil {
			t.Fatalf("出错时应返回 nil 头，实际 %v", headers)
		}
		domainErr := domain.AsError(err)
		if domainErr == nil {
			t.Fatalf("错误应为 domain.Error，实际 %T", err)
		}
		if domainErr.Code != domain.CodeInternal {
			t.Errorf("错误码 = %q，期望 %q", domainErr.Code, domain.CodeInternal)
		}
		if !strings.Contains(err.Error(), ref) && ref != "" {
			t.Errorf("错误信息应指出未知的引用名 %q，实际 %q", ref, err.Error())
		}
	}
}

// TestUpstreamHeadersMergesRouteHeaders 覆盖 route.Headers 与凭据头的合并。
func TestUpstreamHeadersMergesRouteHeaders(t *testing.T) {
	provider := singleRef("claude", Credential{APIKey: "sk-ant"})
	route := domain.Route{
		CredentialRef: "claude",
		Protocol:      domain.ProtocolAnthropicMessages,
		Headers: http.Header{
			"Anthropic-Version": []string{"2023-06-01"},
			"X-Trace":           []string{"a", "b"},
		},
	}
	headers, err := provider.UpstreamHeaders(context.Background(), route)
	if err != nil {
		t.Fatalf("UpstreamHeaders 返回错误：%v", err)
	}
	if got := headers.Get("x-api-key"); got != "sk-ant" {
		t.Fatalf("凭据头 = %q，期望 sk-ant", got)
	}
	if got := headers.Get("Anthropic-Version"); got != "2023-06-01" {
		t.Fatalf("渠道静态头 = %q，期望 2023-06-01", got)
	}
	if got := headers.Values("X-Trace"); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("多值静态头 = %v，期望 [a b]", got)
	}
}

// TestUpstreamHeadersCredentialWinsOnConflict 覆盖同名冲突时凭据头胜出。
//
// 键名大小写与注入形态不一致也要判为同名：HTTP 请求头名不区分大小写，
// 配置里写 "authorization" 同样意在覆盖凭据头，不能被放过。
// 头名由路由协议决定，因此表里把协议与要覆盖的静态头名成对给出。
func TestUpstreamHeadersCredentialWinsOnConflict(t *testing.T) {
	tests := []struct {
		name       string
		protocol   domain.Protocol
		staticName string
	}{
		{name: "Authorization 同名", protocol: domain.ProtocolOpenAIChat, staticName: "Authorization"},
		{name: "authorization 大小写不同", protocol: domain.ProtocolOpenAIChat, staticName: "authorization"},
		{name: "x-api-key 同名", protocol: domain.ProtocolAnthropicMessages, staticName: "x-api-key"},
		{name: "X-API-KEY 大小写不同", protocol: domain.ProtocolAnthropicMessages, staticName: "X-API-KEY"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := singleRef("ref", Credential{APIKey: "sk-real"})
			route := domain.Route{
				CredentialRef: "ref",
				Protocol:      tt.protocol,
				Headers:       http.Header{tt.staticName: []string{"Bearer leaked-from-config"}},
			}
			headers, err := provider.UpstreamHeaders(context.Background(), route)
			if err != nil {
				t.Fatalf("UpstreamHeaders 返回错误：%v", err)
			}
			want := "sk-real"
			if tt.protocol != domain.ProtocolAnthropicMessages {
				want = "Bearer sk-real"
			}
			if got := headers.Get(tt.staticName); got != want {
				t.Fatalf("冲突头 %s = %q，期望凭据头 %q", tt.staticName, got, want)
			}
			if got := len(headers.Values(tt.staticName)); got != 1 {
				t.Fatalf("冲突头应只剩凭据头一个取值，实际 %d 个", got)
			}
		})
	}
}

// TestUpstreamHeadersUnsupportedProtocol 覆盖不支持的协议返回统一错误。
func TestUpstreamHeadersUnsupportedProtocol(t *testing.T) {
	provider := singleRef("ref", Credential{APIKey: "sk"})
	headers, err := provider.UpstreamHeaders(context.Background(),
		domain.Route{CredentialRef: "ref", Protocol: domain.Protocol("gemini")})
	if err == nil {
		t.Fatal("不支持的协议应返回错误")
	}
	if headers != nil {
		t.Fatalf("出错时应返回 nil 头，实际 %v", headers)
	}
	domainErr := domain.AsError(err)
	if domainErr == nil {
		t.Fatalf("错误应为 domain.Error，实际 %T", err)
	}
	if domainErr.Code != domain.CodeInternal {
		t.Fatalf("错误码 = %q，期望 %q", domainErr.Code, domain.CodeInternal)
	}
}

// TestUpstreamHeadersReturnsIndependentMaps 覆盖两次调用返回的 http.Header 互不影响。
func TestUpstreamHeadersReturnsIndependentMaps(t *testing.T) {
	provider := singleRef("ref", Credential{APIKey: "sk"})
	route := domain.Route{
		CredentialRef: "ref",
		Protocol:      domain.ProtocolOpenAIChat,
		Headers:       http.Header{"X-Static": []string{"v"}},
	}

	first, err := provider.UpstreamHeaders(context.Background(), route)
	if err != nil {
		t.Fatalf("第一次 UpstreamHeaders 返回错误：%v", err)
	}
	second, err := provider.UpstreamHeaders(context.Background(), route)
	if err != nil {
		t.Fatalf("第二次 UpstreamHeaders 返回错误：%v", err)
	}

	first.Set("X-Static", "mutated")
	first.Set("X-Extra", "only-in-first")
	if got := second.Get("X-Static"); got != "v" {
		t.Fatalf("改第一个 map 影响了第二个：X-Static = %q", got)
	}
	if got := second.Get("X-Extra"); got != "" {
		t.Fatalf("改第一个 map 影响了第二个：X-Extra = %q", got)
	}
	// route.Headers 本身也不得被返回值反向修改。
	if got := route.Headers.Get("X-Static"); got != "v" {
		t.Fatalf("返回值修改污染了 route.Headers：X-Static = %q", got)
	}
	// 空 apiKey 时行为可预测：头存在但取值为空，不 panic。
	empty, err := singleRef("ref", Credential{}).
		UpstreamHeaders(context.Background(), domain.Route{
			CredentialRef: "ref",
			Protocol:      domain.ProtocolAnthropicMessages,
		})
	if err != nil {
		t.Fatalf("空 apiKey 不应报错：%v", err)
	}
	if got := empty.Get("x-api-key"); got != "" {
		t.Fatalf("空 apiKey 的头取值 = %q，期望空串", got)
	}
}

// TestNewCopiesCredentialTable 守护构造时整表拷贝：调用方之后继续改自己的表，
// 不得改变已生效的凭据表——凭据表在运行期只读，被外部改动会让注入结果与装配时的意图不一致。
func TestNewCopiesCredentialTable(t *testing.T) {
	table := map[string][]Account{
		"openai": {{Ref: "#1", APIKey: "sk-before"}},
	}
	provider := New(table)
	table["openai"] = []Account{{Ref: "#1", APIKey: "sk-after"}}
	table["late"] = []Account{{Ref: "#1", APIKey: "sk-late"}}

	headers, err := provider.UpstreamHeaders(context.Background(),
		domain.Route{CredentialRef: "openai", Protocol: domain.ProtocolOpenAIChat})
	if err != nil {
		t.Fatalf("UpstreamHeaders 返回错误：%v", err)
	}
	if got := headers.Get("Authorization"); got != "Bearer sk-before" {
		t.Errorf("装配后修改入参表改变了凭据：Authorization = %q，期望 %q", got, "Bearer sk-before")
	}
	if _, err := provider.UpstreamHeaders(context.Background(),
		domain.Route{CredentialRef: "late", Protocol: domain.ProtocolOpenAIChat}); err == nil {
		t.Error("装配后新增的引用不应生效，实际取到了凭据")
	}
}

// TestCredentialRedactsPlaintextInFmt 守护凭据与凭据表经 fmt 打印时不落明文密钥。
//
// APIKey 是导出字段，凭据被 %v / %+v 打印就会把明文密钥写进日志，泄漏一旦发生无法收回。
// 因此这里要求输出只有占位符、既不含密钥原文也不含能反查渠道的引用名；
// 值与指针两种形态都覆盖，因为日志里两种写法都常见。
func TestCredentialRedactsPlaintextInFmt(t *testing.T) {
	const (
		// 取值写成一句「这不是真密钥」的说明，只作「有没有漏出明文」的标记。
		// 不写成密钥形状的高熵串：那会命中 gosec 按密码强度判分的硬编码凭据启发式。
		secret = "not-a-real-key"
		ref    = "ref-must-not-appear"
	)
	cred := Credential{APIKey: secret}
	provider := New(map[string][]Account{ref: {{Ref: "#1", APIKey: secret}}})

	cases := []struct {
		name string
		got  string
	}{
		{name: "凭据 %v", got: fmt.Sprintf("%v", cred)},
		{name: "凭据 %+v", got: fmt.Sprintf("%+v", cred)},
		{name: "凭据指针 %v", got: fmt.Sprintf("%v", &cred)},
		{name: "凭据表 %v", got: fmt.Sprintf("%v", provider)},
		{name: "凭据表 %+v", got: fmt.Sprintf("%+v", provider)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if strings.Contains(tc.got, secret) {
				t.Fatalf("打印结果含明文密钥：%q", tc.got)
			}
			if strings.Contains(tc.got, ref) {
				t.Fatalf("打印结果含凭据引用名：%q", tc.got)
			}
			if !strings.Contains(tc.got, "<redacted>") {
				t.Errorf("打印结果应是占位符，实际 %q", tc.got)
			}
		})
	}
}

// TestCredentialRedactsPlaintextInSlog 守护凭据与凭据表经 slog 打印时不落明文密钥。
//
// structured logging 不经 fmt 格式化，Stringer 拦不住，必须由 LogValue 覆盖。
// 这里用真实的 TextHandler 打印一份凭据与一整张凭据表，断言输出里既没有密钥原文，
// 也没有「哪份密钥属于哪个渠道」的引用名，且两个字段的位置都换成了占位符。
func TestCredentialRedactsPlaintextInSlog(t *testing.T) {
	const (
		// 与 fmt 那条用例同一份明文标记，同样不写成密钥形状。
		secret = "not-a-real-key"
		ref    = "ref-must-not-appear"
	)
	cred := Credential{APIKey: secret}
	provider := New(map[string][]Account{ref: {{Ref: "#1", APIKey: secret}}})

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	logger.Info("打印凭据", "credential", cred, "provider", provider)
	out := buf.String()

	if strings.Contains(out, secret) {
		t.Fatalf("日志含明文密钥：%s", out)
	}
	if strings.Contains(out, ref) {
		t.Fatalf("日志含凭据引用名：%s", out)
	}
	for _, field := range []string{"credential", "provider"} {
		if !strings.Contains(out, field+"=<redacted>") {
			t.Errorf("日志字段 %s 应是占位符，实际输出：%s", field, out)
		}
	}
}

// TestUpstreamHeadersSelectsAccountWithinChannel 守护「按 route.AccountRef 在同一个渠道里选账号」。
//
// 同一个渠道的多份凭据共用地址、协议与模型，差异只在密钥本身。
// 选不到账号时退回第一份会让分摊静默退化成只用主账号，因此必须按引用精确选取。
func TestUpstreamHeadersSelectsAccountWithinChannel(t *testing.T) {
	provider := New(map[string][]Account{
		"relay": {
			{Ref: "#1", APIKey: "sk-first"},
			{Ref: "#2", APIKey: "sk-second"},
		},
	})

	headers, err := provider.UpstreamHeaders(context.Background(), domain.Route{
		CredentialRef: "relay",
		AccountRef:    "#2",
		Protocol:      domain.ProtocolOpenAIChat,
	})
	if err != nil {
		t.Fatalf("UpstreamHeaders 返回错误：%v", err)
	}
	if got := headers.Get("Authorization"); got != "Bearer sk-second" {
		t.Fatalf("Authorization = %q，期望第二个账号的密钥", got)
	}
}

// TestEmptyAccountRefUsesFirstAccount 守护单账号配置不必在路由里写账号引用。
//
// 账号池引入之前的路由没有这个字段；空引用取声明顺序的第一项，
// 因此单账号渠道的既有点位一个都不用改。
func TestEmptyAccountRefUsesFirstAccount(t *testing.T) {
	provider := New(map[string][]Account{
		"relay": {
			{Ref: "#1", APIKey: "sk-first"},
			{Ref: "#2", APIKey: "sk-second"},
		},
	})

	headers, err := provider.UpstreamHeaders(context.Background(), domain.Route{
		CredentialRef: "relay",
		Protocol:      domain.ProtocolAnthropicMessages,
	})
	if err != nil {
		t.Fatalf("UpstreamHeaders 返回错误：%v", err)
	}
	if got := headers.Get("x-api-key"); got != "sk-first" {
		t.Fatalf("x-api-key = %q，期望默认账号的密钥", got)
	}
}

// TestUnknownAccountAndChannelReportSeparately 守护两种失败分开报。
//
// 渠道不存在说明装配时漏了整个渠道，账号不存在说明装配时少了一个账号：
// 它们指向配置里的不同地方，合成一句「凭据取不到」会把排查引到错的方向。
func TestUnknownAccountAndChannelReportSeparately(t *testing.T) {
	provider := New(map[string][]Account{
		"relay": {{Ref: "#1", APIKey: "sk-first"}},
	})

	_, err := provider.UpstreamHeaders(context.Background(), domain.Route{
		CredentialRef: "missing",
		Protocol:      domain.ProtocolOpenAIChat,
	})
	if err == nil || !strings.Contains(err.Error(), "不在凭据表里") {
		t.Errorf("未知渠道应报「不在凭据表里」，实际：%v", err)
	}

	_, err = provider.UpstreamHeaders(context.Background(), domain.Route{
		CredentialRef: "relay",
		AccountRef:    "#9",
		Protocol:      domain.ProtocolOpenAIChat,
	})
	if err == nil || !strings.Contains(err.Error(), "没有账号") || !strings.Contains(err.Error(), "#9") {
		t.Errorf("未知账号应报出渠道与账号引用，实际：%v", err)
	}
}

// TestNewCopiesAccountSlices 守护构造时连每个渠道的账号切片一起拷贝：
// 调用方之后继续改自己切片的元素，不得改变已生效的凭据表。
func TestNewCopiesAccountSlices(t *testing.T) {
	accounts := []Account{{Ref: "#1", APIKey: "sk-before"}}
	table := map[string][]Account{"relay": accounts}
	provider := New(table)
	accounts[0] = Account{Ref: "#1", APIKey: "sk-after"}

	headers, err := provider.UpstreamHeaders(context.Background(), domain.Route{
		CredentialRef: "relay",
		Protocol:      domain.ProtocolOpenAIChat,
	})
	if err != nil {
		t.Fatalf("UpstreamHeaders 返回错误：%v", err)
	}
	if got := headers.Get("Authorization"); got != "Bearer sk-before" {
		t.Errorf("装配后修改入参切片改变了凭据：Authorization = %q", got)
	}
}
