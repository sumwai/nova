package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/nova/internal/catalog"
	"github.com/sumwai/nova/internal/config"
	"github.com/sumwai/nova/internal/domain"
)

// discoveryConfig 造一份只有一条发现型端点的配置。
//
// listing 是清单地址；静态模型由调用方经 models 传入。端点地址用固定字面量：
// 它只决定协议推导与凭据注入形态，发现请求打的是 listing。
func discoveryConfig(listing string, models ...config.Model) *config.Config {
	return &config.Config{
		LogLevel:  "info",
		LogFormat: "text",
		Providers: []config.Provider{{
			Name:   "relay",
			APIKey: "sk-test",
			Endpoints: []config.Endpoint{{
				URL:      "https://relay.example.com/v1/chat/completions",
				Protocol: domain.ProtocolOpenAIChat,
				Timeout:  5 * time.Second,
				Models:   models,
				Discover: &config.Discover{URL: listing, Declared: true},
			}},
		}},
	}
}

func TestAssembleMergesDiscoveredAndDeclaredModels(t *testing.T) {
	listing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/v1/models" {
			t.Errorf("清单请求路径 = %q，期望 /v1/models", got)
		}
		// 发现请求复用调用上游那一套请求头：OpenAI 系走 Bearer。
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("清单请求的凭据头 = %q，期望 Bearer sk-test", got)
		}
		_, _ = io.WriteString(w, `{"object":"list","data":[
			{"id":"gpt-5","object":"model","created":1700000000,"owned_by":"openai"},
			{"id":"gpt-5-preview","object":"model"},
			{"id":"claude-sonnet-4","object":"model"},
			{"id":"text-embedding-3","object":"model"}
		]}`)
	}))
	defer listing.Close()

	cfg := discoveryConfig(listing.URL+"/v1/models",
		config.Model{Name: "gpt-5", Upstream: "gpt-5-2026"})
	// 白名单收 gpt 与 claude 两族，黑名单再排掉预览版。
	cfg.Providers[0].Endpoints[0].Discover.Allow = []string{"gpt-5*", "claude-*"}
	cfg.Providers[0].Endpoints[0].Discover.Deny = []string{"*-preview"}

	assembled, err := Assemble(context.Background(), cfg, io.Discard)
	if err != nil {
		t.Fatalf("Assemble 意外失败：%v", err)
	}
	defer func() { _ = assembled.Close() }()

	if len(assembled.Models) != 2 {
		t.Fatalf("目录大小 = %d，期望 2：%+v", len(assembled.Models), assembled.Models)
	}
	// 显式声明与发现项同名时，静态那条胜出——它可以改写上游名。
	if assembled.Models[0].Name != "gpt-5" || assembled.Models[0].Source != ModelSourceStatic {
		t.Errorf("第一条 = %+v，期望 gpt-5 且来源是静态声明", assembled.Models[0])
	}
	if assembled.Models[1].Name != "claude-sonnet-4" || assembled.Models[1].Source != ModelSourceDiscovered {
		t.Errorf("第二条 = %+v，期望 claude-sonnet-4 且来源是发现", assembled.Models[1])
	}

	report := assembled.Discoveries[0]
	if report.Found != 4 || len(report.Kept) != 2 || report.Filtered != 2 {
		t.Errorf("发现报告 = %+v，期望发现 4 条、保留 2 条、排除 2 条", report)
	}
	if report.Shape != catalog.ShapeOpenAI {
		t.Errorf("清单风格 = %q，期望 %q", report.Shape, catalog.ShapeOpenAI)
	}
	if assembled.Stats.Models != 2 {
		t.Errorf("横幅用的目录大小 = %d，期望 2", assembled.Stats.Models)
	}
	// 显式声明的模型（含上游名映射）要随报告一起给出：`nova models` 靠它列出别名。
	if len(report.Declared) != 1 {
		t.Fatalf("报告里的显式声明 = %+v，期望 1 条", report.Declared)
	}
	if report.Declared[0].Name != "gpt-5" || report.Declared[0].Upstream != "gpt-5-2026" {
		t.Errorf("显式声明 = %+v，期望 gpt-5 映射到 gpt-5-2026", report.Declared[0])
	}
}

// 发现失败时，有显式模型的端点降级为只用那些模型，装配继续。
func TestAssembleDegradesToDeclaredModelsWhenDiscoveryFails(t *testing.T) {
	listing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"维护中"}}`)
	}))
	defer listing.Close()

	cfg := discoveryConfig(listing.URL+"/v1/models", config.Model{Name: "gpt-5"})
	assembled, err := Assemble(context.Background(), cfg, io.Discard)
	if err != nil {
		t.Fatalf("有显式模型可退回时不该装配失败，实际：%v", err)
	}
	defer func() { _ = assembled.Close() }()

	if len(assembled.Models) != 1 || assembled.Models[0].Name != "gpt-5" {
		t.Errorf("降级后目录 = %+v，期望只剩显式声明的 gpt-5", assembled.Models)
	}
	if report := assembled.Discoveries[0]; !report.Degraded {
		t.Error("发现失败时应标记为降级")
	}
	if assembled.Stats.Degraded != 1 {
		t.Errorf("降级端点数 = %d，期望 1", assembled.Stats.Degraded)
	}
}

// 发现失败且端点没有显式模型时装配整体失败：那种端点没有任何可路由的模型。
func TestAssembleFailsWhenDiscoveryFailsWithoutDeclaredModels(t *testing.T) {
	listing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"invalid api key"}`)
	}))
	defer listing.Close()

	cfg := discoveryConfig(listing.URL + "/v1/models")
	_, err := Assemble(context.Background(), cfg, io.Discard)
	if err == nil {
		t.Fatal("没有显式模型可退回时应当装配失败")
	}
	// 错误里要说清是哪条渠道的哪份清单，以及为什么失败。
	for _, want := range []string{"模型发现失败", "provider relay", "401"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误 = %q，期望含 %q", err.Error(), want)
		}
	}
}

// Anthropic 形状的端点取清单时要带协议内置头，与转发路径同一套规则。
func TestDiscoveryReusesProtocolHeaders(t *testing.T) {
	var apiKey, version string
	listing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiKey = r.Header.Get("x-api-key")
		version = r.Header.Get("anthropic-version")
		_, _ = io.WriteString(w, `{"data":[{"type":"model","id":"claude-sonnet-4"}]}`)
	}))
	defer listing.Close()

	cfg := &config.Config{
		LogLevel: "info", LogFormat: "text",
		Providers: []config.Provider{{
			Name:   "anthropic",
			APIKey: "sk-ant",
			Endpoints: []config.Endpoint{{
				URL:      "https://api.anthropic.com/v1/messages",
				Protocol: domain.ProtocolAnthropicMessages,
				Timeout:  5 * time.Second,
				Discover: &config.Discover{URL: listing.URL + "/v1/models", Declared: true},
			}},
		}},
	}
	assembled, err := Assemble(context.Background(), cfg, io.Discard)
	if err != nil {
		t.Fatalf("Assemble 意外失败：%v", err)
	}
	defer func() { _ = assembled.Close() }()

	if apiKey != "sk-ant" {
		t.Errorf("清单请求的 x-api-key = %q，期望 sk-ant", apiKey)
	}
	if version == "" {
		t.Error("清单请求缺少 anthropic-version：协议内置头没有复用转发那一套")
	}
	if len(assembled.Models) != 1 || assembled.Models[0].Source != ModelSourceDiscovered {
		t.Errorf("目录 = %+v，期望一条发现来的模型", assembled.Models)
	}
	if assembled.Discoveries[0].Shape != catalog.ShapeAnthropic {
		t.Errorf("清单风格 = %q，期望 %q", assembled.Discoveries[0].Shape, catalog.ShapeAnthropic)
	}
}

// 发现来的模型与显式声明的模型走同一条选路路径；同名时显式那条改写上游名。
func TestDiscoveredModelsJoinRouting(t *testing.T) {
	cfg := discoveryConfig("https://relay.example.com/v1/models",
		config.Model{Name: "gpt-5", Upstream: "gpt-5-2026"})
	endpoint := &cfg.Providers[0].Endpoints[0]
	routes := routesByModel(assembleEndpoints(cfg, map[*config.Endpoint][]catalog.Model{
		endpoint: {{ID: "gpt-5", Name: "gpt-5"}, {ID: "claude-sonnet-4", Name: "claude-sonnet-4"}},
	}))

	declared := routes["gpt-5"]
	if len(declared) != 1 {
		t.Fatalf("gpt-5 的候选数 = %d，期望 1", len(declared))
	}
	// 同名时静态那条胜出，上游名用显式声明的改写值。
	if declared[0].UpstreamModel != "gpt-5-2026" {
		t.Errorf("gpt-5 的上游名 = %q，期望 gpt-5-2026", declared[0].UpstreamModel)
	}

	discovered := routes["claude-sonnet-4"]
	if len(discovered) != 1 {
		t.Fatalf("claude-sonnet-4 的候选数 = %d，期望 1", len(discovered))
	}
	// 发现项没有独立的上游名，对外名与上游名相同。
	if discovered[0].UpstreamModel != "claude-sonnet-4" {
		t.Errorf("发现模型的上游名 = %q，期望与对外名相同", discovered[0].UpstreamModel)
	}
	if discovered[0].CredentialRef != "relay" {
		t.Errorf("发现模型的凭据引用 = %q，期望 relay", discovered[0].CredentialRef)
	}
}

// 发现项改名后：目录里是对外名，发往上游的仍是上游 id。
func TestAssembleExposesRenamedModels(t *testing.T) {
	listing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"id":"sensenova/kimi-k3"},{"id":"glm-5.2"}]}`)
	}))
	defer listing.Close()

	cfg := discoveryConfig(listing.URL + "/v1/models")
	cfg.Providers[0].Endpoints[0].Discover.Expose = []config.Alias{
		{From: "sensenova/*", To: "*"},
	}

	assembled, err := Assemble(context.Background(), cfg, io.Discard)
	if err != nil {
		t.Fatalf("Assemble 意外失败：%v", err)
	}
	defer func() { _ = assembled.Close() }()

	if len(assembled.Models) != 2 {
		t.Fatalf("目录 = %+v，期望两条", assembled.Models)
	}
	// 命中的项去掉了前缀；未命中的项保持上游 id。
	if assembled.Models[0].Name != "kimi-k3" || assembled.Models[1].Name != "glm-5.2" {
		t.Errorf("目录 = %+v，期望 kimi-k3 与 glm-5.2", assembled.Models)
	}

	kept := assembled.Discoveries[0].Kept
	if len(kept) != 2 || kept[0].Name != "kimi-k3" || kept[0].Upstream != "sensenova/kimi-k3" {
		t.Fatalf("报告里的保留项 = %+v，期望对外名与上游名分开记录", kept)
	}

	// 选路用的是上游 id：客户端请求 kimi-k3，网关发给上游 sensenova/kimi-k3。
	endpoint := &cfg.Providers[0].Endpoints[0]
	routes := routesByModel(assembleEndpoints(cfg, map[*config.Endpoint][]catalog.Model{
		endpoint: {{ID: "sensenova/kimi-k3", Name: "kimi-k3"}},
	}))
	route := routes["kimi-k3"]
	if len(route) != 1 || route[0].UpstreamModel != "sensenova/kimi-k3" {
		t.Fatalf("kimi-k3 的路由 = %+v，期望上游名为 sensenova/kimi-k3", route)
	}
}

// 显式声明的对外名与改名后的发现项同名时，显式那条胜出（与未改名时的规则一致）。
func TestAssemblePrefersDeclaredOverExposedName(t *testing.T) {
	listing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"id":"sensenova/kimi-k3"}]}`)
	}))
	defer listing.Close()

	cfg := discoveryConfig(listing.URL+"/v1/models",
		config.Model{Name: "kimi-k3", Upstream: "固定的上游名"})
	cfg.Providers[0].Endpoints[0].Discover.Expose = []config.Alias{{From: "sensenova/*", To: "*"}}

	assembled, err := Assemble(context.Background(), cfg, io.Discard)
	if err != nil {
		t.Fatalf("Assemble 意外失败：%v", err)
	}
	defer func() { _ = assembled.Close() }()

	if len(assembled.Models) != 1 || assembled.Models[0].Source != ModelSourceStatic {
		t.Fatalf("目录 = %+v，期望只剩显式声明的那条", assembled.Models)
	}
	endpoint := &cfg.Providers[0].Endpoints[0]
	routes := routesByModel(assembleEndpoints(cfg, map[*config.Endpoint][]catalog.Model{
		endpoint: {{ID: "sensenova/kimi-k3", Name: "kimi-k3"}},
	}))
	if routes["kimi-k3"][0].UpstreamModel != "固定的上游名" {
		t.Errorf("上游名 = %q，期望用显式声明的那个", routes["kimi-k3"][0].UpstreamModel)
	}
}

// 发现型端点在目录里的顺序是「显式声明在前、发现项在后」。
func TestModelEntriesPutDeclaredModelsFirst(t *testing.T) {
	cfg := discoveryConfig("https://relay.example.com/v1/models",
		config.Model{Name: "z-declared"})
	endpoint := &cfg.Providers[0].Endpoints[0]
	entries := modelEntries(assembleEndpoints(cfg, map[*config.Endpoint][]catalog.Model{
		endpoint: {{ID: "a-discovered", Name: "a-discovered"}, {ID: "z-declared", Name: "z-declared"}},
	}), map[*config.Endpoint][]catalog.Model{
		endpoint: {{ID: "a-discovered", Name: "a-discovered"}, {ID: "z-declared", Name: "z-declared"}},
	})

	if len(entries) != 2 {
		t.Fatalf("目录大小 = %d，期望 2", len(entries))
	}
	if entries[0].Name != "z-declared" || entries[0].Source != ModelSourceStatic {
		t.Errorf("第一条 = %+v，期望显式声明的模型排在最前", entries[0])
	}
	if entries[1].Name != "a-discovered" || entries[1].Source != ModelSourceDiscovered {
		t.Errorf("第二条 = %+v，期望发现项排在后面", entries[1])
	}
}
