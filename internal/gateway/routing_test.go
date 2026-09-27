package gateway

import (
	"strings"
	"testing"
	"time"

	"github.com/sumwai/nova/internal/config"
	"github.com/sumwai/nova/internal/domain"
	"github.com/sumwai/nova/internal/limits"
	"github.com/sumwai/nova/internal/price"
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
	return newModelRouteResolver(testEndpoints(cfg), cfg, testPriceTable(t, cfg))
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

// pricedProvider 造一条提供单个模型的渠道，并带上一条单价声明。
//
// price 是每百万输出 token 的单价：排序用的固定用量估计只含输出，
// 因此把价格放在输出上才能观察排序。currency 为空表示不写单价（模拟查不到价格的模型）。
func pricedProvider(name, model, currency string, rate float64) config.Provider {
	provider := routeProvider(name, model)
	declared := price.Declared{Key: "p/" + name, Currency: currency}
	if currency != "" {
		declared.Unit = &price.Unit{Currency: currency, OutputMTok: rate}
	}
	provider.Endpoints[0].Models[0].Price = declared
	return provider
}

// freeProvider 造一条显式免费的渠道。
func freeProvider(name, model string) config.Provider {
	provider := routeProvider(name, model)
	provider.Endpoints[0].Models[0].Price = price.Declared{Key: "p/" + name, Free: true, Currency: "USD"}
	return provider
}

// poolProvider 造一条声明了计划额度的渠道，即 prefer price 里的「池类」。
func poolProvider(name, model, currency string, rate float64) config.Provider {
	provider := pricedProvider(name, model, currency, rate)
	provider.Accounts[0].Limits = []limits.Declared{{
		Kind:   "quota",
		Metric: "requests",
		Window: "1m",
		Limit:  floatPtr(100),
	}}
	return provider
}

// TestPreferPriceKeepsPoolAheadOfCheaperPayg 守护池类按声明顺序在前，不按单价。
//
// 设计第七节：`price` 是「先花已付费的，再按单价挑按量」。池类若按单价排，
// 一条贵的订阅渠道会排到便宜很多的按量渠道之后，与「先花已付费的」相反。
func TestPreferPriceKeepsPoolAheadOfCheaperPayg(t *testing.T) {
	cfg := &config.Config{Providers: []config.Provider{
		poolProvider("pool", "m", "USD", 100),
		pricedProvider("cheap", "m", "USD", 1),
	}}
	cfg.ModelRoutes = []config.ModelRoute{{
		Pattern: "m",
		Prefer:  config.PreferPrice,
		Candidates: []config.RouteCandidate{
			{Provider: "pool", Weight: 1},
			{Provider: "cheap", Weight: 1},
		},
	}}

	resolver, _ := routeResolver(t, cfg)
	if got := providerOrder(candidatesOf(t, resolver, "m")); !equalStrings(got, []string{"pool", "cheap"}) {
		t.Errorf("price 顺序 = %v，期望池类 pool 在按量 cheap 之前", got)
	}
}

// TestPreferPricePoolKeepsDeclarationOrder 守护多个池类之间保持声明顺序。
func TestPreferPricePoolKeepsDeclarationOrder(t *testing.T) {
	cfg := &config.Config{Providers: []config.Provider{
		poolProvider("p2", "m", "USD", 50),
		poolProvider("p1", "m", "USD", 1),
		pricedProvider("payg", "m", "USD", 0.1),
	}}
	cfg.ModelRoutes = []config.ModelRoute{{
		Pattern: "m",
		Prefer:  config.PreferPrice,
		Candidates: []config.RouteCandidate{
			{Provider: "p2", Weight: 1},
			{Provider: "p1", Weight: 1},
			{Provider: "payg", Weight: 1},
		},
	}}

	resolver, _ := routeResolver(t, cfg)
	if got := providerOrder(candidatesOf(t, resolver, "m")); !equalStrings(got, []string{"p2", "p1", "payg"}) {
		t.Errorf("price 顺序 = %v，期望池类按声明顺序 p2 p1，按量 payg 在后", got)
	}
}

// TestPreferPricePoolSkipsNominalWarning 守护池类不因价格未知而被报「名义价参与排序」。
//
// 池类不按单价排序，那条提醒对它不成立；报了会让人以为顺序来自名义价。
func TestPreferPricePoolSkipsNominalWarning(t *testing.T) {
	cfg := &config.Config{Providers: []config.Provider{
		poolProvider("pool", "m", "", 0),
		pricedProvider("cheap", "m", "USD", 1),
	}}
	cfg.ModelRoutes = []config.ModelRoute{{
		Pattern: "m",
		Prefer:  config.PreferPrice,
		Candidates: []config.RouteCandidate{
			{Provider: "pool", Weight: 1},
			{Provider: "cheap", Weight: 1},
		},
	}}

	_, warnings := routeResolver(t, cfg)
	for _, warning := range warnings {
		if strings.Contains(warning.Msg, "名义价") {
			t.Errorf("提醒 = %q，池类不应产生名义价提醒", warning.Msg)
		}
	}
}

// TestPreferOrderIgnoresPrices 守护 order 不受价格影响：即使带上单价，顺序仍是声明顺序。
func TestPreferOrderIgnoresPrices(t *testing.T) {
	cfg := &config.Config{Providers: []config.Provider{
		pricedProvider("a", "m", "USD", 10),
		pricedProvider("b", "m", "USD", 1),
		pricedProvider("c", "m", "USD", 5),
	}}
	cfg.ModelRoutes = []config.ModelRoute{{
		Pattern: "m",
		Prefer:  config.PreferOrder,
		Candidates: []config.RouteCandidate{
			{Provider: "a", Weight: 1},
			{Provider: "b", Weight: 1},
			{Provider: "c", Weight: 1},
		},
	}}

	resolver, _ := routeResolver(t, cfg)
	if got := providerOrder(candidatesOf(t, resolver, "m")); !equalStrings(got, []string{"a", "b", "c"}) {
		t.Errorf("order 顺序 = %v，期望声明顺序", got)
	}
}

// TestPreferPriceSortsByUnitCost 守护同币种内按单价升序。
func TestPreferPriceSortsByUnitCost(t *testing.T) {
	cfg := &config.Config{Providers: []config.Provider{
		pricedProvider("a", "m", "USD", 3),
		pricedProvider("b", "m", "USD", 1),
		pricedProvider("c", "m", "USD", 2),
	}}
	cfg.ModelRoutes = []config.ModelRoute{{
		Pattern: "m",
		Prefer:  config.PreferPrice,
		Candidates: []config.RouteCandidate{
			{Provider: "a", Weight: 1},
			{Provider: "b", Weight: 1},
			{Provider: "c", Weight: 1},
		},
	}}

	resolver, _ := routeResolver(t, cfg)
	if got := providerOrder(candidatesOf(t, resolver, "m")); !equalStrings(got, []string{"b", "c", "a"}) {
		t.Errorf("price 顺序 = %v，期望按单价升序 b c a", got)
	}
}

// TestPreferPriceKeepsFreeFirst 守护显式免费排在所有按量之前，且组内保持声明顺序。
func TestPreferPriceKeepsFreeFirst(t *testing.T) {
	cfg := &config.Config{Providers: []config.Provider{
		pricedProvider("a", "m", "USD", 0.5),
		freeProvider("b", "m"),
		freeProvider("c", "m"),
	}}
	cfg.ModelRoutes = []config.ModelRoute{{
		Pattern: "m",
		Prefer:  config.PreferPrice,
		Candidates: []config.RouteCandidate{
			{Provider: "a", Weight: 1},
			{Provider: "b", Weight: 1},
			{Provider: "c", Weight: 1},
		},
	}}

	resolver, _ := routeResolver(t, cfg)
	if got := providerOrder(candidatesOf(t, resolver, "m")); !equalStrings(got, []string{"b", "c", "a"}) {
		t.Errorf("price 顺序 = %v，期望免费 b c 在前、按量 a 在后", got)
	}
}

// TestPreferPriceUnknownJoinsByNominal 守护未知价用名义价参与排序，不被排到最后。
//
// 已知输出单价 10 与 2，中位数是 6；未知条目按 6 排，落在两者之间。
func TestPreferPriceUnknownJoinsByNominal(t *testing.T) {
	cfg := &config.Config{Providers: []config.Provider{
		pricedProvider("a", "m", "USD", 10),
		pricedProvider("b", "m", "USD", 2),
		pricedProvider("c", "m", "", 0),
	}}
	cfg.ModelRoutes = []config.ModelRoute{{
		Pattern: "m",
		Prefer:  config.PreferPrice,
		Candidates: []config.RouteCandidate{
			{Provider: "a", Weight: 1},
			{Provider: "b", Weight: 1},
			{Provider: "c", Weight: 1},
		},
	}}

	resolver, warnings := routeResolver(t, cfg)
	got := providerOrder(candidatesOf(t, resolver, "m"))
	if !equalStrings(got, []string{"b", "c", "a"}) {
		t.Errorf("price 顺序 = %v，期望未知价 c 按名义价落在中间", got)
	}
	// 名义价参与排序时必须被标 assumed，否则「这个顺序不是按真实单价得出的」无从看见。
	found := false
	for _, warning := range warnings {
		if strings.Contains(warning.Msg, "价格未知") && strings.Contains(warning.Msg, "assumed") {
			found = true
		}
	}
	if !found {
		t.Errorf("期望一条「价格未知…assumed」提醒，实际：%v", warnings)
	}
}

// TestPreferPriceDoesNotCompareCurrencies 守护跨币种不比较：各自分组、组内排序。
//
// 声明顺序是 a(USD 10) b(EUR 100) c(USD 1) d(EUR 1)。USD 组内应为 c a，EUR 组内应为 d b，
// 组按首次出现排成 USD 组在前。EUR 的 1 小于 USD 的 1 也不会跳进 USD 组，因为两者不可比。
func TestPreferPriceDoesNotCompareCurrencies(t *testing.T) {
	cfg := &config.Config{Providers: []config.Provider{
		pricedProvider("a", "m", "USD", 10),
		pricedProvider("b", "m", "EUR", 100),
		pricedProvider("c", "m", "USD", 1),
		pricedProvider("d", "m", "EUR", 1),
	}}
	cfg.ModelRoutes = []config.ModelRoute{{
		Pattern: "m",
		Prefer:  config.PreferPrice,
		Candidates: []config.RouteCandidate{
			{Provider: "a", Weight: 1},
			{Provider: "b", Weight: 1},
			{Provider: "c", Weight: 1},
			{Provider: "d", Weight: 1},
		},
	}}

	resolver, _ := routeResolver(t, cfg)
	got := providerOrder(candidatesOf(t, resolver, "m"))
	if !equalStrings(got, []string{"c", "a", "d", "b"}) {
		t.Errorf("price 顺序 = %v，期望 USD 组 c a 在前、EUR 组 d b 在后", got)
	}
}

// TestPreferPriceThenBalanceRotates 守护 prefer 与 balance 的关系：先按价格排序，再轮转起点。
func TestPreferPriceThenBalanceRotates(t *testing.T) {
	cfg := &config.Config{Providers: []config.Provider{
		pricedProvider("a", "m", "USD", 3),
		pricedProvider("b", "m", "USD", 1),
	}}
	cfg.ModelRoutes = []config.ModelRoute{{
		Pattern:  "m",
		Prefer:   config.PreferPrice,
		Balanced: true,
		Candidates: []config.RouteCandidate{
			{Provider: "a", Weight: 1},
			{Provider: "b", Weight: 1},
		},
	}}

	resolver, _ := routeResolver(t, cfg)
	var firsts []string
	for round := 0; round < 4; round++ {
		got := providerOrder(candidatesOf(t, resolver, "m"))
		firsts = append(firsts, got[0])
	}
	// 排序后 b 在前，轮转只改起点：b,a,b,a。
	if want := []string{"b", "a", "b", "a"}; !equalStrings(firsts, want) {
		t.Errorf("首选渠道序列 = %v，期望 %v", firsts, want)
	}
}

func indexOfProvider(routes []domain.Route, provider string) int {
	for i, route := range routes {
		if route.CredentialRef == provider {
			return i
		}
	}
	return -1
}
