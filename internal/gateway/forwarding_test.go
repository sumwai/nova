package gateway

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/nova/internal/config"
	"github.com/sumwai/nova/internal/domain"
)

func TestProtocolForPathMapsOnlyRegisteredPaths(t *testing.T) {
	tests := []struct {
		path string
		want domain.Protocol
		ok   bool
	}{
		{path: "/v1/chat/completions", want: domain.ProtocolOpenAIChat, ok: true},
		{path: "/v1/responses", want: domain.ProtocolOpenAIResponses, ok: true},
		{path: "/v1/messages", want: domain.ProtocolAnthropicMessages, ok: true},
		// 尾斜杠、子路径与「差不多的别的路径」都不算命中：网关只暴露约定的三个端点，
		// 对「看起来像」的路径放行，会让一个拼错的 URL 直到转发那一步才暴露。
		{path: "/v1/messages/"},
		{path: "/v1/messages/extra"},
		{path: "/v1/completions"},
		{path: "/healthz"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got, ok := protocolForPath(tt.path)
			if ok != tt.ok || got != tt.want {
				t.Errorf("protocolForPath(%q) = %q, %v；期望 %q, %v", tt.path, got, ok, tt.want, tt.ok)
			}
		})
	}
}

// testConfig 造两个渠道各一条端点、且共享同一个对外模型名的配置。
//
// 两条端点的协议刻意不同：同一个对外名下的候选因此跨协议，正好用来检查
// 「同协议优先」那一层排序有没有生效。
func testConfig() *config.Config {
	return &config.Config{
		Providers: []config.Provider{
			{
				Name:   "primary",
				APIKey: "primary-key",
				Endpoints: []config.Endpoint{{
					URL:      "https://primary.example.com/v1/chat/completions",
					Protocol: domain.ProtocolOpenAIChat,
					Timeout:  5 * time.Second,
					Models:   []config.Model{{Name: "shared", Upstream: "primary-upstream"}},
				}},
			},
			{
				Name:   "backup",
				APIKey: "backup-key",
				Endpoints: []config.Endpoint{{
					URL:      "https://backup.example.com/v1/messages",
					Protocol: domain.ProtocolAnthropicMessages,
					Timeout:  5 * time.Second,
					Models:   []config.Model{{Name: "shared", Upstream: "backup-upstream"}},
				}},
			},
		},
	}
}

func TestRoutesByModelKeepsCandidateOrder(t *testing.T) {
	shared := routesByModel(testConfig())["shared"]
	if len(shared) != 2 {
		t.Fatalf("shared 的候选数 = %d，期望 2", len(shared))
	}
	// 顺序即回退顺序：声明在前的渠道先被尝试。
	if shared[0].CredentialRef != "primary" || shared[1].CredentialRef != "backup" {
		t.Errorf("候选顺序 = %q, %q；期望 primary, backup", shared[0].CredentialRef, shared[1].CredentialRef)
	}
	if got := shared[0].BaseURL; got != "https://primary.example.com/v1/chat/completions" {
		t.Errorf("BaseURL = %q，期望取端点的完整地址", got)
	}
	if got := shared[1].UpstreamModel; got != "backup-upstream" {
		t.Errorf("UpstreamModel = %q，期望取该 model 的 upstream", got)
	}
	if got := shared[1].Protocol; got != domain.ProtocolAnthropicMessages {
		t.Errorf("Protocol = %q，期望照搬端点的协议", got)
	}
	if got := shared[0].Timeout; got != 5*time.Second {
		t.Errorf("Timeout = %v，期望照搬端点的超时", got)
	}
	// 本版没有能配置这两项的指令，它们必须留零值，而不是被某个默认值悄悄填上。
	if shared[0].OutputLimit != nil {
		t.Errorf("OutputLimit = %v，期望 nil", *shared[0].OutputLimit)
	}
	if got := shared[0].CredentialHeaderStyle; got != domain.CredentialHeaderAuto {
		t.Errorf("CredentialHeaderStyle = %q，期望零值", got)
	}
}

func TestModelRouteResolverPrefersSameProtocol(t *testing.T) {
	resolver := modelRouteResolver{routes: routesByModel(testConfig())}

	got, err := resolver.Candidates(context.Background(), &domain.Request{
		Model:    "shared",
		Protocol: domain.ProtocolAnthropicMessages,
	})
	if err != nil {
		t.Fatalf("Candidates 报错：%v", err)
	}
	if len(got) != 2 {
		t.Fatalf("候选数 = %d，期望两条都保留", len(got))
	}
	// 同协议的候选排到最前：省掉一次重建，把请求按原协议的字段原样上发。
	// 跨协议的候选留在链尾，因此另一个协议仍是前一条失败后的后备。
	if got[0].Protocol != domain.ProtocolAnthropicMessages {
		t.Errorf("首选协议 = %q，期望与客户端协议一致", got[0].Protocol)
	}
	if got[1].Protocol != domain.ProtocolOpenAIChat {
		t.Errorf("后备协议 = %q，期望是另一条", got[1].Protocol)
	}
}

func TestModelRouteResolverMissReturnsNoCandidates(t *testing.T) {
	resolver := modelRouteResolver{routes: routesByModel(testConfig())}
	for _, req := range []*domain.Request{
		nil,
		{Model: "unknown", Protocol: domain.ProtocolOpenAIChat},
	} {
		got, err := resolver.Candidates(context.Background(), req)
		if err != nil || got != nil {
			t.Errorf("未命中时 = %v, %v；期望空候选与 nil 错误（把错误码留给流水线判定）", got, err)
		}
	}
}

func TestUpstreamIDUsesProviderAndHost(t *testing.T) {
	tests := []struct {
		name    string
		address string
		want    string
	}{
		{
			name:    "只留主机名，不写完整地址",
			address: "https://api.example.com/v1/chat/completions",
			want:    "relay api.example.com",
		},
		{
			name:    "带端口时端口跟着主机名走",
			address: "http://127.0.0.1:18080/v1/messages",
			want:    "relay 127.0.0.1:18080",
		},
		{
			name:    "带 userinfo 时口令不进标识",
			address: "https://user:secret@api.example.com/v1/chat/completions",
			want:    "relay api.example.com",
		},
		{
			name:    "解析不出主机名时退回抹掉口令的原文",
			address: "user:secret@not-a-url",
			want:    "relay user:xxxxx@not-a-url",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := upstreamID("relay", &config.Endpoint{URL: tt.address})
			if got != tt.want {
				t.Errorf("upstreamID = %q，期望 %q", got, tt.want)
			}
			// 这个标识会进日志，口令绝不能跟着去。
			if strings.Contains(got, "secret") {
				t.Errorf("upstreamID 泄露了口令：%q", got)
			}
		})
	}
}

func TestMaxUpstreamAttemptsCoversLongestChain(t *testing.T) {
	tests := []struct {
		name   string
		routes map[string][]domain.Route
		want   int
	}{
		{name: "空表退到 1", routes: nil, want: 1},
		{name: "单候选", routes: map[string][]domain.Route{"a": {{}}}, want: 1},
		{
			name:   "取最长的那条链",
			routes: map[string][]domain.Route{"a": {{}, {}}, "b": {{}, {}, {}}},
			want:   3,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := maxUpstreamAttempts(tt.routes); got != tt.want {
				t.Errorf("maxUpstreamAttempts = %d，期望 %d", got, tt.want)
			}
		})
	}
}

func TestCredentialsByRefUsesProviderName(t *testing.T) {
	table := credentialsByRef(testConfig().Providers)
	if len(table) != 2 {
		t.Fatalf("凭据表条目数 = %d，期望 2", len(table))
	}
	// 引用名必须与 Route.CredentialRef 同源，否则运行期取不到凭据。
	if table["primary"].APIKey != "primary-key" {
		t.Errorf("primary 的凭据 = %q，期望 primary-key", table["primary"].APIKey)
	}
	if table["backup"].APIKey != "backup-key" {
		t.Errorf("backup 的凭据 = %q，期望 backup-key", table["backup"].APIKey)
	}
}
