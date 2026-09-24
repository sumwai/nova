package gateway

import (
	"strings"
	"testing"
	"time"

	"github.com/sumwai/nova/internal/config"
	"github.com/sumwai/nova/internal/domain"
)

// routeProvider 造一条只提供一个模型的渠道，账号唯一。
func routeProvider(name, model string) config.Provider {
	return config.Provider{
		Name:     name,
		Accounts: []config.Account{{APIKey: "k-" + name, Index: 1, Weight: 1}},
		Endpoints: []config.Endpoint{{
			URL:      "https://" + name + ".example.com/v1/chat/completions",
			Protocol: domain.ProtocolOpenAIChat,
			Timeout:  time.Second,
			Models:   []config.Model{{Name: model, Upstream: name + "-upstream"}},
		}},
	}
}

// routeConfig 造三个渠道，都提供同一个对外名。
func routeConfig() *config.Config {
	return &config.Config{
		Providers: []config.Provider{
			routeProvider("a", "m"),
			routeProvider("b", "m"),
			routeProvider("c", "m"),
		},
	}
}

func routeResolver(t *testing.T, cfg *config.Config) (*modelRouteResolver, []config.Warning) {
	t.Helper()
	return newModelRouteResolver(testEndpoints(cfg), cfg)
}

func providerOrder(routes []domain.Route) []string {
	order := make([]string, 0, len(routes))
	for _, route := range routes {
		order = append(order, route.CredentialRef)
	}
	return order
}

// TestModelRouteReordersCandidates 守护「列出的渠道排在前、未列出的接在后」。
//
// 不写 balance 时块内顺序就是优先级，未列出的渠道按声明顺序排在其后作回退。
func TestModelRouteReordersCandidates(t *testing.T) {
	cfg := routeConfig()
	cfg.ModelRoutes = []config.ModelRoute{{
		Pattern: "m",
		Candidates: []config.RouteCandidate{
			{Provider: "c", Weight: 1},
			{Provider: "a", Weight: 1},
		},
	}}

	resolver, _ := routeResolver(t, cfg)
	got := candidatesOf(t, resolver, "m")
	if want := []string{"c", "a", "b"}; !equalStrings(providerOrder(got), want) {
		t.Errorf("候选顺序 = %v，期望 %v", providerOrder(got), want)
	}
}

// TestModelRouteBalancesListedCandidates 守护分摊只改起点、链尾保留回退段。
//
// 列出的渠道之间轮转；未列出的渠道永远在最后，只有前面都失败才会被用到。
func TestModelRouteBalancesListedCandidates(t *testing.T) {
	cfg := routeConfig()
	cfg.ModelRoutes = []config.ModelRoute{{
		Pattern:  "m",
		Balanced: true,
		Candidates: []config.RouteCandidate{
			{Provider: "a", Weight: 1},
			{Provider: "b", Weight: 1},
		},
	}}

	resolver, _ := routeResolver(t, cfg)
	var firsts []string
	for round := 0; round < 4; round++ {
		got := candidatesOf(t, resolver, "m")
		if len(got) != 3 {
			t.Fatalf("候选数 = %d，期望三条都保留", len(got))
		}
		firsts = append(firsts, got[0].CredentialRef)
		// 未列出的 c 不参与分摊，始终排在最后。
		if last := got[len(got)-1].CredentialRef; last != "c" {
			t.Fatalf("第 %d 次链尾 = %s，期望未列出的 c", round+1, last)
		}
	}
	if want := []string{"a", "b", "a", "b"}; !equalStrings(firsts, want) {
		t.Errorf("首选渠道序列 = %v，期望 %v", firsts, want)
	}
}

// TestModelRouteBalancesWholeProvider 守护同一渠道的多条端点候选作为一段整体移动。
//
// 段内保持声明顺序：先试完这条渠道的全部端点，再换下一条渠道。
func TestModelRouteBalancesWholeProvider(t *testing.T) {
	cfg := &config.Config{Providers: []config.Provider{
		{
			Name:     "a",
			Accounts: []config.Account{{APIKey: "k-a", Index: 1, Weight: 1}},
			Endpoints: []config.Endpoint{
				{
					URL:      "https://a.example.com/v1/chat/completions",
					Protocol: domain.ProtocolOpenAIChat,
					Timeout:  time.Second,
					Models:   []config.Model{{Name: "m", Upstream: "a-first"}},
				},
				{
					URL:      "https://a.example.com/v1/responses",
					Protocol: domain.ProtocolOpenAIResponses,
					Timeout:  time.Second,
					Models:   []config.Model{{Name: "m", Upstream: "a-second"}},
				},
			},
		},
		routeProvider("b", "m"),
	}}
	cfg.ModelRoutes = []config.ModelRoute{{
		Pattern:  "m",
		Balanced: true,
		Candidates: []config.RouteCandidate{
			{Provider: "a", Weight: 1},
			{Provider: "b", Weight: 1},
		},
	}}

	resolver, _ := routeResolver(t, cfg)
	for round := 0; round < 4; round++ {
		got := candidatesOf(t, resolver, "m")
		if len(got) != 3 {
			t.Fatalf("候选数 = %d，期望三条", len(got))
		}
		// a 的两条端点必须相邻，且相对顺序不变。
		first := indexOfProvider(got, "a")
		if first < 0 || first+1 >= len(got) {
			t.Fatalf("第 %d 次里 a 的两条端点不相邻：%v", round+1, providerOrder(got))
		}
		if got[first].UpstreamModel != "a-first" || got[first+1].UpstreamModel != "a-second" {
			t.Fatalf("第 %d 次 a 的端点顺序 = %q / %q，期望保持声明顺序",
				round+1, got[first].UpstreamModel, got[first+1].UpstreamModel)
		}
	}
}

// TestModelRouteFallbackGroupRunsLast 守护三段顺序：主用候选 → fallback → 未列出的渠道。
//
// fallback 不参与轮转，也不与主用候选混在一起：主用候选全部失败之后才轮到它，
// 未列出的渠道又在它之后。这样「失败了才切换」是结构上的事实，不靠权重或运气。
func TestModelRouteFallbackGroupRunsLast(t *testing.T) {
	cfg := &config.Config{Providers: []config.Provider{
		routeProvider("a", "m"),
		routeProvider("b", "m"),
		routeProvider("c", "m"),
		routeProvider("d", "m"),
	}}
	cfg.ModelRoutes = []config.ModelRoute{{
		Pattern:  "m",
		Balanced: true,
		Candidates: []config.RouteCandidate{
			{Provider: "a", Weight: 1},
			{Provider: "b", Weight: 1},
			{Provider: "c", Weight: 1, Fallback: true},
		},
	}}

	resolver, _ := routeResolver(t, cfg)
	var firsts []string
	for round := 0; round < 4; round++ {
		got := candidatesOf(t, resolver, "m")
		if len(got) != 4 {
			t.Fatalf("候选数 = %d，期望四条都保留", len(got))
		}
		order := providerOrder(got)
		if order[2] != "c" || order[3] != "d" {
			t.Fatalf("第 %d 次链尾 = %v，期望 fallback 的 c 与未列出的 d 固定在最后",
				round+1, order)
		}
		firsts = append(firsts, order[0])
	}
	if want := []string{"a", "b", "a", "b"}; !equalStrings(firsts, want) {
		t.Errorf("主用段首选渠道序列 = %v，期望 %v", firsts, want)
	}
}

// TestModelRouteWarnsWhenRuleHasNoTarget 守护装配期的两条提醒。
func TestModelRouteWarnsWhenRuleHasNoTarget(t *testing.T) {
	// c 提供的是另一个模型，因此第二条规则里的 c 没有作用对象。
	cfg := &config.Config{Providers: []config.Provider{
		routeProvider("a", "m"),
		routeProvider("b", "m"),
		routeProvider("c", "other"),
	}}
	cfg.ModelRoutes = []config.ModelRoute{
		{
			Pattern:    "ghost*",
			Candidates: []config.RouteCandidate{{Provider: "a", Weight: 1}},
		},
		{
			Pattern: "m",
			Candidates: []config.RouteCandidate{
				{Provider: "a", Weight: 1},
				{Provider: "c", Weight: 1},
			},
		},
	}

	_, warnings := routeResolver(t, cfg)
	var texts []string
	for _, warning := range warnings {
		texts = append(texts, warning.String())
	}
	joined := strings.Join(texts, "\n")
	if !strings.Contains(joined, "没有命中任何对外名") {
		t.Errorf("期望提醒「规则没有命中任何对外名」，实际：\n%s", joined)
	}
	if !strings.Contains(joined, "没有提供任何匹配该模式的模型") {
		t.Errorf("期望提醒「候选渠道没有作用对象」，实际：\n%s", joined)
	}
}

// TestNoModelRouteKeepsDeclarationOrder 守护「不写 route 块时行为不变」。
func TestNoModelRouteKeepsDeclarationOrder(t *testing.T) {
	resolver, warnings := routeResolver(t, routeConfig())
	if len(warnings) != 0 {
		t.Fatalf("没有规则时不该有提醒：%v", warnings)
	}
	got := candidatesOf(t, resolver, "m")
	if want := []string{"a", "b", "c"}; !equalStrings(providerOrder(got), want) {
		t.Errorf("候选顺序 = %v，期望按声明顺序 %v", providerOrder(got), want)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func indexOfProvider(routes []domain.Route, provider string) int {
	for i, route := range routes {
		if route.CredentialRef == provider {
			return i
		}
	}
	return -1
}
