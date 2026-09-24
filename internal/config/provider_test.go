package config

import (
	"strings"
	"testing"
	"time"

	"github.com/sumwai/nova/internal/domain"
)

func TestParseSingleEndpointProvider(t *testing.T) {
	cfg := mustParse(t, `
version 1
provider openai {
    url https://api.openai.com/v1/chat/completions
    api_key sk-test
    model gpt-5
    model gpt-5-mini gpt-5-mini-2025-01-01
}
`)

	if len(cfg.Providers) != 1 {
		t.Fatalf("provider 数 = %d，期望 1", len(cfg.Providers))
	}
	provider := cfg.Providers[0]
	if provider.Name != "openai" {
		t.Errorf("名字 = %q，期望 openai", provider.Name)
	}
	if provider.Accounts[0].APIKey != "sk-test" {
		t.Errorf("凭据 = %q，期望 sk-test", provider.Accounts[0].APIKey)
	}
	if len(provider.Endpoints) != 1 {
		t.Fatalf("端点 数 = %d，期望 1", len(provider.Endpoints))
	}

	endpoint := provider.Endpoints[0]
	if !endpoint.Default {
		t.Error("写在 provider 一级的端点应被标记为默认端点")
	}
	if endpoint.URL != "https://api.openai.com/v1/chat/completions" {
		t.Errorf("地址 = %q", endpoint.URL)
	}
	if endpoint.Protocol != domain.ProtocolOpenAIChat {
		t.Errorf("协议 = %q，期望从地址推导出 openai_chat", endpoint.Protocol)
	}
	if endpoint.Timeout != defaultTimeout {
		t.Errorf("超时 = %v，期望缺省 %v", endpoint.Timeout, defaultTimeout)
	}
	if len(endpoint.Models) != 2 {
		t.Fatalf("模型 数 = %d，期望 2", len(endpoint.Models))
	}
	if endpoint.Models[0].Name != "gpt-5" || endpoint.Models[0].Upstream != "gpt-5" {
		t.Errorf("第一条模型 = %+v，期望上游名与对外名相同", endpoint.Models[0])
	}
	// 两个记号的写法：对外名与上游名不同。
	if endpoint.Models[1].Name != "gpt-5-mini" || endpoint.Models[1].Upstream != "gpt-5-mini-2025-01-01" {
		t.Errorf("第二条模型 = %+v，期望上游名被改写", endpoint.Models[1])
	}
}

func TestParseMultiEndpointProvider(t *testing.T) {
	cfg := mustParse(t, `
version 1
provider relay {
    api_key k

    url https://relay.example.com/v1/chat/completions
    model gpt-5

    endpoint https://relay.example.com/v1/messages {
        protocol anthropic_messages
        timeout 120s
        model claude-sonnet-4 claude-sonnet-4-20250514
    }
}
`)

	provider := cfg.Providers[0]
	if len(provider.Endpoints) != 2 {
		t.Fatalf("端点 数 = %d，期望 2", len(provider.Endpoints))
	}
	// 顺序按声明位置：默认端点写在前，因此它排第一。
	if !provider.Endpoints[0].Default {
		t.Error("第一条端点应是默认端点")
	}
	if provider.Endpoints[0].URL != "https://relay.example.com/v1/chat/completions" {
		t.Errorf("第一条端点地址 = %q", provider.Endpoints[0].URL)
	}

	second := provider.Endpoints[1]
	if second.Default {
		t.Error("endpoint 子块不应被标记为默认端点")
	}
	if second.URL != "https://relay.example.com/v1/messages" {
		t.Errorf("第二条端点地址 = %q，期望取块头", second.URL)
	}
	if second.Protocol != domain.ProtocolAnthropicMessages {
		t.Errorf("第二条端点协议 = %q", second.Protocol)
	}
	if second.Timeout != 120*time.Second {
		t.Errorf("第二条端点超时 = %v，期望 120s", second.Timeout)
	}
	if second.Models[0].Upstream != "claude-sonnet-4-20250514" {
		t.Errorf("第二条端点模型 = %+v", second.Models[0])
	}
}

// 只写 endpoint 子块、不写 provider 级端点指令时，这条渠道没有默认端点。
func TestParseProviderWithOnlyNamedEndpoints(t *testing.T) {
	cfg := mustParse(t, `
version 1
provider both {
    api_key k
    endpoint https://both.example.com/v1/chat/completions {
        model gpt-5
    }
    endpoint https://both.example.com/v1/messages {
        model claude-sonnet-4
    }
}
`)

	provider := cfg.Providers[0]
	if len(provider.Endpoints) != 2 {
		t.Fatalf("端点 数 = %d，期望 2", len(provider.Endpoints))
	}
	for _, endpoint := range provider.Endpoints {
		if endpoint.Default {
			t.Errorf("端点 %s 不该被标记为默认端点", endpoint.URL)
		}
	}
}

// 候选顺序就是回退顺序：同一对外名跨端点的声明先后决定先试谁。
func TestRoutesFollowDeclarationOrder(t *testing.T) {
	cfg := mustParse(t, `
version 1
provider first {
    api_key k1
    url https://a.example.com/v1/chat/completions
    model gpt-5
}
provider second {
    api_key k2
    url https://b.example.com/v1/chat/completions
    model gpt-5 glm-4.6
}
`)

	routes := cfg.Routes("gpt-5")
	if len(routes) != 2 {
		t.Fatalf("候选数 = %d，期望 2", len(routes))
	}
	if routes[0].Provider != "first" || routes[0].Upstream != "gpt-5" {
		t.Errorf("第一候选 = %+v，期望 first 且不改写上游名", routes[0])
	}
	if routes[1].Provider != "second" || routes[1].Upstream != "glm-4.6" {
		t.Errorf("第二候选 = %+v，期望 second 且上游名被改写", routes[1])
	}
	if got := cfg.Routes("不存在的模型"); len(got) != 0 {
		t.Errorf("未声明的模型名应没有候选，得到 %v", got)
	}
}

// 同名模型也可以按端点先后回退：候选顺序是端点级的，不是 provider 级的。
func TestRoutesOrderWithinOneProvider(t *testing.T) {
	cfg := mustParse(t, `
version 1
provider p {
    api_key k

    url https://a.example.com/v1/chat/completions
    model gpt-5

    endpoint https://b.example.com/v1/chat/completions {
        model gpt-5 gpt-5-backup
    }
}
`)

	routes := cfg.Routes("gpt-5")
	if len(routes) != 2 {
		t.Fatalf("候选数 = %d，期望 2", len(routes))
	}
	if routes[0].Upstream != "gpt-5" || routes[1].Upstream != "gpt-5-backup" {
		t.Errorf("候选顺序 = %q / %q，期望按端点声明先后", routes[0].Upstream, routes[1].Upstream)
	}
}

func TestProtocolIsDerivedFromURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want domain.Protocol
	}{
		{"OpenAI Chat", "https://api.openai.com/v1/chat/completions", domain.ProtocolOpenAIChat},
		{"OpenAI Responses", "https://api.openai.com/v1/responses", domain.ProtocolOpenAIResponses},
		{"Anthropic", "https://api.anthropic.com/v1/messages", domain.ProtocolAnthropicMessages},
		// 版本根由各家自定，因此比对的是端点段而不是整条路径。
		{"智谱式版本根", "https://open.bigmodel.cn/api/coding/paas/v4/chat/completions", domain.ProtocolOpenAIChat},
		{"火山方舟式版本根", "https://ark.cn-beijing.volces.com/api/v3/chat/completions", domain.ProtocolOpenAIChat},
		{"地址末尾多一个斜杠", "https://a.example.com/v1/chat/completions/", domain.ProtocolOpenAIChat},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := mustParse(t, "version 1\nprovider p {\n    api_key k\n    url "+tt.url+"\n    model m\n}\n")
			if got := cfg.Providers[0].Endpoints[0].Protocol; got != tt.want {
				t.Errorf("协议 = %q，期望 %q", got, tt.want)
			}
		})
	}
}

func TestProviderConfigErrors(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		wantMsg []string
	}{
		{
			name:    "缺 api_key",
			src:     "provider p {\n    url https://a.example.com/v1/chat/completions\n    model m\n}\n",
			wantMsg: []string{"没有任何凭据", "api_key"},
		},
		{
			name:    "缺 url",
			src:     "provider p {\n    api_key k\n    model m\n}\n",
			wantMsg: []string{"缺少 url"},
		},
		{
			name:    "缺 model",
			src:     "provider p {\n    api_key k\n    url https://a.example.com/v1/chat/completions\n}\n",
			wantMsg: []string{"没有声明任何 model"},
		},
		{
			name:    "空 provider 块（两者都缺时先报凭据）",
			src:     "provider p {\n}\n",
			wantMsg: []string{"没有任何凭据"},
		},
		{
			name:    "有凭据但一个端点都没有",
			src:     "provider p {\n    api_key k\n}\n",
			wantMsg: []string{"没有任何端点"},
		},
		{
			name:    "协议与地址不一致",
			src:     "provider p {\n    api_key k\n    url https://a.example.com/v1/chat/completions\n    protocol anthropic_messages\n    model m\n}\n",
			wantMsg: []string{"推出的是", "没有正确答案"},
		},
		{
			name:    "地址末段认不出协议",
			src:     "provider p {\n    api_key k\n    url https://a.example.com/v1/unknown\n    model m\n}\n",
			wantMsg: []string{"认不出线协议", "/chat/completions"},
		},
		{
			name:    "地址缺主机名",
			src:     "provider p {\n    api_key k\n    url /v1/chat/completions\n    model m\n}\n",
			wantMsg: []string{"不是带主机名的 http(s) 地址"},
		},
		{
			name:    "地址协议不是 http",
			src:     "provider p {\n    api_key k\n    url ftp://a.example.com/v1/chat/completions\n    model m\n}\n",
			wantMsg: []string{"只支持 http 与 https"},
		},
		{
			name:    "provider 重名",
			src:     "provider p {\n    api_key k\n    url https://a.example.com/v1/chat/completions\n    model a\n}\nprovider p {\n    api_key k\n    url https://b.example.com/v1/chat/completions\n    model b\n}\n",
			wantMsg: []string{"已在", "声明过"},
		},
		{
			name:    "同一端点里模型名重复",
			src:     "provider p {\n    api_key k\n    url https://a.example.com/v1/chat/completions\n    model gpt-5\n    model gpt-5 别的上游名\n}\n",
			wantMsg: []string{"已经声明过"},
		},
		{
			name:    "endpoint 不带花括号",
			src:     "provider p {\n    api_key k\n    url https://a.example.com/v1/chat/completions\n    model m\n    endpoint chat\n}\n",
			wantMsg: []string{"endpoint 块写成"},
		},
		{
			name:    "endpoint 块内又写 url",
			src:     "provider p {\n    api_key k\n    endpoint https://a.example.com/v1/chat/completions {\n        url https://a.example.com/v1/messages\n        model m\n    }\n}\n",
			wantMsg: []string{"块内不能再写 url"},
		},
		{
			name:    "endpoint 块里再开块",
			src:     "provider p {\n    api_key k\n    endpoint https://a.example.com/v1/chat/completions {\n        endpoint https://a.example.com/v1/messages {\n            model m\n        }\n    }\n}\n",
			wantMsg: []string{"不能再开块"},
		},
		{
			name:    "provider 块没有收尾",
			src:     "provider p {\n    api_key k\n    url https://a.example.com/v1/chat/completions\n    model m\n",
			wantMsg: []string{"没有收尾的 }"},
		},
		{
			name:    "超时不是正的时长",
			src:     "provider p {\n    api_key k\n    url https://a.example.com/v1/chat/completions\n    timeout -5s\n    model m\n}\n",
			wantMsg: []string{"不是正的时长"},
		},
		{
			name:    "协议名不认识",
			src:     "provider p {\n    api_key k\n    url https://a.example.com/v1/chat/completions\n    protocol openai\n    model m\n}\n",
			wantMsg: []string{"线协议", "不认识"},
		},
		{
			name:    "model 只有对外名之外的多余取值",
			src:     "provider p {\n    api_key k\n    url https://a.example.com/v1/chat/completions\n    model a b c\n}\n",
			wantMsg: []string{"至多接受两个取值"},
		},
		{
			name:    "provider 块里写顶层指令",
			src:     "provider p {\n    api_key k\n    url https://a.example.com/v1/chat/completions\n    model m\n    log_level debug\n}\n",
			wantMsg: []string{"未知指令", "log_level"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := parseErr(t, "version 1\n"+tt.src)
			for _, want := range tt.wantMsg {
				if !strings.Contains(err.Msg, want) {
					t.Errorf("消息 = %q，期望包含 %q", err.Msg, want)
				}
			}
		})
	}
}

// 错误位置要指回真正出问题的那一行，而不是笼统地指向块头。
func TestProviderErrorPointsAtOffendingLine(t *testing.T) {
	err := parseErr(t, `version 1
provider p {
    api_key k
    url https://a.example.com/v1/chat/completions
    model m
    timeout 5秒
}
`)

	if err.Line != 6 {
		t.Errorf("行号 = %d，期望 6（timeout 那一行）", err.Line)
	}
	if err.Col != 13 {
		t.Errorf("列号 = %d，期望 13（timeout 的取值）", err.Col)
	}
}

func TestModelNamesKeepDeclarationOrder(t *testing.T) {
	cfg := mustParse(t, `
version 1
provider a {
    api_key k
    url https://a.example.com/v1/chat/completions
    model zeta
    model alpha
}
provider b {
    api_key k
    url https://b.example.com/v1/chat/completions
    model alpha
    model beta
}
`)

	got := cfg.ModelNames()
	want := []string{"zeta", "alpha", "beta"}
	if len(got) != len(want) {
		t.Fatalf("模型名 = %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("模型名 = %v，期望 %v（按首次声明顺序去重）", got, want)
		}
	}
}

// Routes 返回新切片：调用方改它不影响配置本身。
func TestRoutesReturnFreshSlice(t *testing.T) {
	cfg := mustParse(t, `
version 1
provider p {
    api_key k
    url https://a.example.com/v1/chat/completions
    model gpt-5
}
`)

	first := cfg.Routes("gpt-5")
	first[0].Upstream = "被调用方改掉的"

	if got := cfg.Routes("gpt-5")[0].Upstream; got == "被调用方改掉的" {
		t.Error("两次调用共享了同一个切片")
	}
}
