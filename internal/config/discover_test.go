package config

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sumwai/nova/internal/domain"
)

func TestDiscoverDerivesListingURLFromEndpointURL(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		protocol string
		want     string
	}{
		{
			name: "OpenAI Chat",
			url:  "https://api.openai.com/v1/chat/completions",
			want: "https://api.openai.com/v1/models",
		},
		{
			name: "OpenAI Responses",
			url:  "https://api.openai.com/v1/responses",
			want: "https://api.openai.com/v1/models",
		},
		{
			// Anthropic 形状的端点同样从 /v1/messages 推出 /v1/models，
			// 不需要额外写一行 discover。
			name: "Anthropic Messages",
			url:  "https://api.anthropic.com/v1/messages",
			want: "https://api.anthropic.com/v1/models",
		},
		{
			// 版本根不是 /v1 时版本根原样保留：推导是「裁端点段再拼 /models」，
			// 不是「换成 /v1/models」。
			name: "智谱的版本根",
			url:  "https://open.bigmodel.cn/api/coding/paas/v4/chat/completions",
			want: "https://open.bigmodel.cn/api/coding/paas/v4/models",
		},
		{
			name: "火山方舟的版本根",
			url:  "https://ark.cn-beijing.volces.com/api/v3/chat/completions",
			want: "https://ark.cn-beijing.volces.com/api/v3/models",
		},
		{
			name: "尾斜杠先被裁掉",
			url:  "https://host/v1/messages/",
			want: "https://host/v1/models",
		},
		{
			name: "地址里带 query 时丢弃",
			url:  "https://host/v1/messages?beta=true",
			want: "https://host/v1/models",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := "version 1\nprovider relay {\n    api_key k\n    url " + tt.url + "\n    discover\n}\n"
			cfg := mustParse(t, src)
			spec := cfg.Providers[0].Endpoints[0].Discover
			if spec == nil {
				t.Fatal("discover 没有被解析出来")
			}
			if spec.URL != tt.want {
				t.Errorf("清单地址 = %q，期望 %q", spec.URL, tt.want)
			}
			if !spec.Derived {
				t.Error("推导出的地址应当被标记为 Derived")
			}
		})
	}
}

func TestDiscoverKeepsExplicitListingURL(t *testing.T) {
	cfg := mustParse(t, `
version 1
provider relay {
    api_key k
    url https://relay.example.com/v1/chat/completions
    discover https://listing.example.com/models
    model gpt-5
}
`)
	spec := cfg.Providers[0].Endpoints[0].Discover
	if spec.URL != "https://listing.example.com/models" {
		t.Errorf("清单地址 = %q，期望保留显式写下的地址", spec.URL)
	}
	if spec.Derived {
		t.Error("显式写下的地址不该被标记为 Derived")
	}
}

func TestDiscoverParsesAllowAndDenyInOrder(t *testing.T) {
	cfg := mustParse(t, `
version 1
provider relay {
    api_key k
    url https://relay.example.com/v1/chat/completions
    discover
    allow gpt-5*
    deny *-preview
    allow claude-*
}
`)
	spec := cfg.Providers[0].Endpoints[0].Discover
	if strings.Join(spec.Allow, ",") != "gpt-5*,claude-*" {
		t.Errorf("allow = %v，期望按声明顺序", spec.Allow)
	}
	if strings.Join(spec.Deny, ",") != "*-preview" {
		t.Errorf("deny = %v", spec.Deny)
	}
	// allow / deny 先于 discover 出现时含义不变：行序不参与语义。
	reordered := mustParse(t, `
version 1
provider relay {
    api_key k
    url https://relay.example.com/v1/chat/completions
    allow gpt-5*
    discover
}
`)
	if got := reordered.Providers[0].Endpoints[0].Discover.Allow; len(got) != 1 || got[0] != "gpt-5*" {
		t.Errorf("allow = %v，期望先出现的过滤规则同样被收下", got)
	}
}

// 只有 discover、没有 model 的端点必须能通过校验：那正是「模型全部来自上游清单」的写法。
func TestEndpointWithOnlyDiscoverProvidesNoStaticModels(t *testing.T) {
	cfg := mustParse(t, `
version 1
provider relay {
    api_key k
    url https://relay.example.com/v1/chat/completions
    discover
}
`)
	endpoint := cfg.Providers[0].Endpoints[0]
	if len(endpoint.Models) != 0 {
		t.Errorf("静态模型 = %v，期望为空", endpoint.Models)
	}
	// 发现来的模型不进配置层的对外名清单：它们是运行期事实。
	if names := cfg.ModelNames(); len(names) != 0 {
		t.Errorf("ModelNames = %v，期望不含发现模型", names)
	}
	if count := cfg.DiscoveryCount(); count != 1 {
		t.Errorf("DiscoveryCount = %d，期望 1", count)
	}
}

func TestDiscoverRejectsFilterWithoutDiscover(t *testing.T) {
	err := parseErr(t, `
version 1
provider relay {
    api_key k
    url https://relay.example.com/v1/chat/completions
    model gpt-5
    deny *-preview
}
`)
	if !strings.Contains(err.Msg, "却没有 discover") {
		t.Errorf("消息 = %q，期望指出过滤规则缺少 discover", err.Msg)
	}
}

func TestDiscoverRejectsSecondDiscoverLine(t *testing.T) {
	err := parseErr(t, `
version 1
provider relay {
    api_key k
    url https://relay.example.com/v1/chat/completions
    discover
    discover https://elsewhere.example.com/models
}
`)
	if !strings.Contains(err.Msg, "不能写两次") {
		t.Errorf("消息 = %q，期望拒绝重复的 discover", err.Msg)
	}
}

func TestDiscoverRejectsBadListingURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{name: "不是 http", url: "ftp://host/models", want: "只支持 http 与 https"},
		{name: "没有主机名", url: "/v1/models", want: "不是带主机名的 http(s) 地址"},
		{name: "取值超出一个", url: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.url == "" {
				err := parseErr(t, `
version 1
provider relay {
    api_key k
    url https://relay.example.com/v1/chat/completions
    discover a b
}
`)
				if !strings.Contains(err.Msg, "至多接受一个取值") {
					t.Errorf("消息 = %q，期望拒绝多余的取值", err.Msg)
				}
				return
			}
			err := parseErr(t, `
version 1
provider relay {
    api_key k
    url https://relay.example.com/v1/chat/completions
    discover `+tt.url+`
}
`)
			if !strings.Contains(err.Msg, tt.want) {
				t.Errorf("消息 = %q，期望含 %q", err.Msg, tt.want)
			}
		})
	}
}

func TestDiscoverRejectsBlankPattern(t *testing.T) {
	err := parseErr(t, `
version 1
provider relay {
    api_key k
    url https://relay.example.com/v1/chat/completions
    discover
    allow "a b"
}
`)
	if !strings.Contains(err.Msg, "不能含空白") {
		t.Errorf("消息 = %q，期望拒绝含空白的模式", err.Msg)
	}
}

// 端点既没有 model 也没有 discover 时仍然报错：那时它没有任何选路依据。
func TestEndpointWithoutAnyModelSourceStillFails(t *testing.T) {
	err := parseErr(t, `
version 1
provider relay {
    api_key k
    url https://relay.example.com/v1/chat/completions
}
`)
	if !strings.Contains(err.Msg, "没有声明任何 model") {
		t.Errorf("消息 = %q，期望指出端点没有模型来源", err.Msg)
	}
}

// 推导函数只接受「能裁出协议端点段」的地址，其余一律交回 false，
// 由调用方要求显式写出清单地址，而不是猜一个。
func TestDeriveListingURLIsConservative(t *testing.T) {
	tests := []struct {
		url      string
		protocol domain.Protocol
	}{
		// 没有端点段。
		{url: "https://host/v1", protocol: domain.ProtocolOpenAIChat},
		// 端点段不完整。
		{url: "https://host/v1/chat/complete", protocol: domain.ProtocolOpenAIChat},
		// 协议与地址末段对不上：配置层会在更早的地方报错，推导函数不因此猜测。
		{url: "https://host/v1/messages", protocol: domain.ProtocolOpenAIChat},
	}
	for _, tt := range tests {
		if _, ok := deriveListingURL(tt.url, tt.protocol); ok {
			t.Errorf("deriveListingURL(%q, %q) 应当失败", tt.url, tt.protocol)
		}
	}
}

// 不写任何过滤规则时记一条提醒：准入集合完全由上游决定。这不是错误，
// 但启动时应当能看出「暴露什么由上游说了算」。
func TestDiscoverWithoutFilterRulesWarns(t *testing.T) {
	cfg := mustParse(t, `
version 1
provider relay {
    api_key k
    url https://relay.example.com/v1/chat/completions
    discover
    model gpt-5
}
`)
	if !hasWarningAbout(cfg, "没有 allow / deny") {
		t.Errorf("提醒 = %v，期望提示准入集合完全由上游决定", cfg.Warnings)
	}

	// 写了任一过滤规则就不再提醒：准入集合已经由配置说了话。
	withRule := mustParse(t, `
version 1
provider relay {
    api_key k
    url https://relay.example.com/v1/chat/completions
    discover
    allow gpt-5*
}
`)
	if hasWarningAbout(withRule, "没有 allow / deny") {
		t.Errorf("提醒 = %v，期望不再提示", withRule.Warnings)
	}
}

// model 的两个记号按字面量使用：名字里出现通配符时提醒一句，
// 免得把它当成过滤模式写。
func TestModelWithWildcardWarns(t *testing.T) {
	cfg := mustParse(t, `
version 1
provider relay {
    api_key k
    url https://relay.example.com/v1/chat/completions
    discover
    model sensenova/* *
}
`)
	if !hasWarningAbout(cfg, "不做通配匹配") {
		t.Errorf("提醒 = %v，期望指出 model 不做通配匹配", cfg.Warnings)
	}

	// 名字里没有通配符时不提示。
	plain := mustParse(t, `
version 1
provider relay {
    api_key k
    url https://relay.example.com/v1/chat/completions
    model gpt-4o openai/gpt-4o
}
`)
	if hasWarningAbout(plain, "不做通配匹配") {
		t.Errorf("提醒 = %v，期望不提示", plain.Warnings)
	}
}

// expose 的解析与校验：两个取值、上游模式恰一个 *、两个模式都不收 ?。
func TestExposeParsesAndValidates(t *testing.T) {
	cfg := mustParse(t, `
version 1
provider relay {
    api_key k
    url https://relay.example.com/v1/chat/completions
    discover
    allow *
    expose sensenova/* *
    expose * sns/*
}
`)
	spec := cfg.Providers[0].Endpoints[0].Discover
	if len(spec.Expose) != 2 {
		t.Fatalf("改名规则 = %+v，期望两条且按声明顺序", spec.Expose)
	}
	if spec.Expose[0].From != "sensenova/*" || spec.Expose[0].To != "*" {
		t.Errorf("第一条 = %+v，期望去前缀", spec.Expose[0])
	}
	if spec.Expose[1].From != "*" || spec.Expose[1].To != "sns/*" {
		t.Errorf("第二条 = %+v，期望加前缀", spec.Expose[1])
	}
	if spec.Expose[0].Line == 0 {
		t.Error("规则要带位置：未命中的提醒靠它指回配置行")
	}

	tests := []struct {
		name string
		line string
		want string
	}{
		{name: "缺取值", line: "expose a*", want: "需要两个取值"},
		{name: "多取值", line: "expose a* b* c", want: "至多接受两个取值"},
		{name: "上游模式没有星号", line: "expose aa bb", want: "必须含恰好一个 *"},
		{name: "上游模式两个星号", line: "expose a*b* c", want: "必须含恰好一个 *"},
		{name: "上游模式带问号", line: "expose a?* c", want: "不收 ?"},
		{name: "对外名模式两个星号", line: "expose a* b*c*", want: "至多含一个 *"},
		{name: "对外名模式带问号", line: "expose a* b?", want: "不收 ?"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := fmt.Sprintf("version 1\nprovider relay {\n    api_key k\n"+
				"    url https://relay.example.com/v1/chat/completions\n    discover\n    %s\n}\n", tt.line)
			err := parseErr(t, src)
			if !strings.Contains(err.Msg, tt.want) {
				t.Errorf("消息 = %q，期望含 %q", err.Msg, tt.want)
			}
		})
	}

	// 改名规则作用于清单项，没有 discover 就没有作用对象。
	noDiscover := parseErr(t, `
version 1
provider relay {
    api_key k
    url https://relay.example.com/v1/chat/completions
    model gpt-5
    expose a* b*
}
`)
	if !strings.Contains(noDiscover.Msg, "却没有 discover") {
		t.Errorf("消息 = %q，期望指出缺 discover", noDiscover.Msg)
	}
}
