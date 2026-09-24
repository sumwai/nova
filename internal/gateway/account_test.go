package gateway

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/sumwai/nova/internal/config"
	"github.com/sumwai/nova/internal/domain"
)

// acct 造一个账号，序号与权重都要显式给：序号就是它的引用，权重缺省在解析期填 1，
// 测试直接构造 config 结构时不会走那条路。
func acct(index, weight int) config.Account {
	return config.Account{APIKey: "k" + strconv.Itoa(index), Index: index, Weight: weight}
}

// accountConfig 造一条渠道，一个端点一个对外模型，账号池由调用方给出。
func accountConfig(balanced bool, accounts ...config.Account) *config.Config {
	return &config.Config{
		Providers: []config.Provider{{
			Name:     "relay",
			Accounts: accounts,
			Balanced: balanced,
			Endpoints: []config.Endpoint{{
				URL:      "https://relay.example.com/v1/chat/completions",
				Protocol: domain.ProtocolOpenAIChat,
				Timeout:  5 * time.Second,
				Models:   []config.Model{{Name: "shared", Upstream: "shared-upstream"}},
			}},
		}},
	}
}

// resolverOf 造一次装配的选路器。轮转状态挂在它上面，因此需要看「多次请求之间的轮转」
// 的用例必须复用同一个实例，而不能每次重新装配。
func resolverOf(t *testing.T, cfg *config.Config) *modelRouteResolver {
	t.Helper()
	resolver, _ := newModelRouteResolver(testEndpoints(cfg), cfg)
	return resolver
}

func candidatesOf(t *testing.T, resolver *modelRouteResolver, model string) []domain.Route {
	t.Helper()
	got, err := resolver.Candidates(context.Background(), &domain.Request{
		Model:    model,
		Protocol: domain.ProtocolOpenAIChat,
	})
	if err != nil {
		t.Fatalf("Candidates 报错：%v", err)
	}
	return got
}

// TestCandidatesExpandAccountsInsideEndpoint 守护账号在端点内层展开。
//
// 账号是同一渠道内的等价副本，账号级故障比端点级故障常见，因此 E1 的两个账号
// 先被依次尝试，再轮到 E2；顺序里先出现的账号也是回退链上的首选。
func TestCandidatesExpandAccountsInsideEndpoint(t *testing.T) {
	cfg := accountConfig(false, acct(1, 1), acct(2, 1))
	cfg.Providers = append(cfg.Providers, config.Provider{
		Name:     "backup",
		Accounts: []config.Account{acct(1, 1)},
		Endpoints: []config.Endpoint{{
			URL:      "https://backup.example.com/v1/chat/completions",
			Protocol: domain.ProtocolOpenAIChat,
			Timeout:  5 * time.Second,
			Models:   []config.Model{{Name: "shared", Upstream: "backup-upstream"}},
		}},
	})

	got := candidatesOf(t, resolverOf(t, cfg), "shared")
	if len(got) != 3 {
		t.Fatalf("候选数 = %d，期望 3（relay 两个账号 + backup 一个）", len(got))
	}
	want := []struct{ channel, account string }{
		{"relay", "#1"},
		{"relay", "#2"},
		{"backup", ""},
	}
	for i, w := range want {
		if got[i].CredentialRef != w.channel || got[i].AccountRef != w.account {
			t.Errorf("第 %d 条候选 = %s/%s，期望 %s/%s",
				i+1, got[i].CredentialRef, got[i].AccountRef, w.channel, w.account)
		}
	}
}

// TestSingleAccountLeavesAccountRefEmpty 守护单账号渠道不产生账号引用。
//
// 凭据表对空引用取该渠道的默认账号，日志与折叠键因此与账号池引入之前逐字一致。
func TestSingleAccountLeavesAccountRefEmpty(t *testing.T) {
	got := candidatesOf(t, resolverOf(t, accountConfig(false, acct(1, 1))), "shared")
	if len(got) != 1 {
		t.Fatalf("候选数 = %d，期望 1", len(got))
	}
	if got[0].AccountRef != "" {
		t.Errorf("AccountRef = %q，期望空串", got[0].AccountRef)
	}
}

// TestSequentialPoolKeepsDeclarationOrder 守护未写 balance 时的声明顺序。
//
// 每次请求的候选顺序完全一样：前面的账号失败（且可重试）才用后面的。
func TestSequentialPoolKeepsDeclarationOrder(t *testing.T) {
	resolver := resolverOf(t, accountConfig(false, acct(1, 1), acct(2, 1)))
	for round := 0; round < 3; round++ {
		got := candidatesOf(t, resolver, "shared")
		if got[0].AccountRef != "#1" || got[1].AccountRef != "#2" {
			t.Fatalf("第 %d 次候选顺序 = %s/%s，期望固定为 #1/#2",
				round+1, got[0].AccountRef, got[1].AccountRef)
		}
	}
}

// TestBalancedPoolRotatesStart 守护按权重分摊时起点逐次前移、链尾保留全部账号。
//
// 分摊只改起点，不改「失败换下一个」：候选数恒等于账号数，
// 因此正常时轮流用，某个账号失败时仍有后备。
func TestBalancedPoolRotatesStart(t *testing.T) {
	resolver := resolverOf(t, accountConfig(true, acct(1, 1), acct(2, 1)))

	var firsts []string
	for round := 0; round < 4; round++ {
		got := candidatesOf(t, resolver, "shared")
		if len(got) != 2 {
			t.Fatalf("候选数 = %d，期望两条都保留", len(got))
		}
		firsts = append(firsts, got[0].AccountRef)
	}
	want := []string{"#1", "#2", "#1", "#2"}
	for i := range want {
		if firsts[i] != want[i] {
			t.Fatalf("首选账号序列 = %v，期望 %v", firsts, want)
		}
	}
}

// TestBalancedPoolSharesOrderAcrossEndpoints 守护同一渠道的多条端点共用一次轮转起点。
//
// 一个请求在两个端点上各长出一组账号候选；两组必须从同一个账号开始，
// 否则一个请求会在 E1 上用 #2、在 E2 上又从 #1 开始，分摊比例随之走样。
func TestBalancedPoolSharesOrderAcrossEndpoints(t *testing.T) {
	cfg := accountConfig(true, acct(1, 1), acct(2, 1))
	cfg.Providers[0].Endpoints = append(cfg.Providers[0].Endpoints, config.Endpoint{
		URL:      "https://relay.example.com/v1/messages",
		Protocol: domain.ProtocolAnthropicMessages,
		Timeout:  5 * time.Second,
		Models:   []config.Model{{Name: "shared", Upstream: "shared-anthropic"}},
	})

	got := candidatesOf(t, resolverOf(t, cfg), "shared")
	if len(got) != 4 {
		t.Fatalf("候选数 = %d，期望 4（两条端点各两个账号）", len(got))
	}
	// 同协议优先会把 openai 端点排到前面，两条各占一格。
	if got[0].AccountRef != got[2].AccountRef || got[1].AccountRef != got[3].AccountRef {
		t.Errorf("两组账号顺序 = [%s %s] / [%s %s]，期望两组一致",
			got[0].AccountRef, got[1].AccountRef, got[2].AccountRef, got[3].AccountRef)
	}
}

// TestAccountPoolOrderAppliesWeights 守护加权轮转：按累计权重决定这次的起点。
//
// 3:1 的池在四个请求里轮到第一个账号三次、第二个账号一次，
// 且每次链上都保留两个账号。
func TestAccountPoolOrderAppliesWeights(t *testing.T) {
	pool := &accountPool{
		refs:     []string{"#1", "#2"},
		weights:  []int{3, 1},
		total:    4,
		balanced: true,
	}

	var firsts []string
	for round := 0; round < 4; round++ {
		order := pool.order()
		if len(order) != 2 {
			t.Fatalf("第 %d 次顺序 = %v，期望两个账号都在", round+1, order)
		}
		firsts = append(firsts, order[0])
	}
	want := []string{"#1", "#1", "#1", "#2"}
	for i := range want {
		if firsts[i] != want[i] {
			t.Fatalf("首选账号序列 = %v，期望 %v", firsts, want)
		}
	}
}
