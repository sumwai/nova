package catalog

import (
	"context"
	"strings"
	"testing"
)

func TestExposeRewritesUpstreamIDs(t *testing.T) {
	tests := []struct {
		name string
		from string
		to   string
		id   string
		want string
		ok   bool
	}{
		{name: "加前缀", from: "*", to: "sensenova/*", id: "kimi-k3", want: "sensenova/kimi-k3", ok: true},
		{name: "去前缀", from: "sensenova/*", to: "*", id: "sensenova/kimi-k3", want: "kimi-k3", ok: true},
		{name: "换前缀", from: "openai/*", to: "gpt/*", id: "openai/gpt-4o", want: "gpt/gpt-4o", ok: true},
		{name: "去后缀", from: "*-latest", to: "*", id: "glm-latest", want: "glm", ok: true},
		// 不加 * 的对外名模式：命中几条就全落到同一个名字上，命中两条会在 Discover 里报错。
		{name: "常量对外名", from: "*", to: "统一名", id: "任意", want: "统一名", ok: true},
		// 前缀对不上就不命中。
		{name: "前缀不匹配", from: "sensenova/*", to: "*", id: "kimi-k3"},
		// 待匹配的 id 比前后缀加起来还短，不可能命中。
		{name: "长度不够", from: "aaa/*", to: "*", id: "aa/"},
		// 匹配不区分大小写，但捕获值取自原文：拼出来的名字不跟着被折叠。
		{name: "大小写不敏感", from: "SENSENOVA/*", to: "x/*", id: "sensenova/Kimi-K3", want: "x/Kimi-K3", ok: true},
		// 捕获可以为空：前缀之外什么都不剩时给出空串，由对外名模式自己决定怎么用。
		{name: "捕获为空", from: "sensenova/*", to: "p/*", id: "sensenova/", want: "p/", ok: true},
		// 含两个 * 的模式没有定义的捕获区间，按未命中处理。
		{name: "两个星号不匹配", from: "a*b*", to: "*", id: "aXbY"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := Alias{From: tt.from, To: tt.to}.expose(tt.id)
			if ok != tt.ok || got != tt.want {
				t.Errorf("expose(%q, %q) 于 %q = %q, %v；期望 %q, %v",
					tt.from, tt.to, tt.id, got, ok, tt.want, tt.ok)
			}
		})
	}
}

// 多条规则时第一条命中即生效：先特例、后通配的分层因此能表达。
func TestApplyAliasesTakesFirstMatch(t *testing.T) {
	aliases := []Alias{
		{From: "sensenova/*", To: "sns/*"},
		{From: "*", To: "sensenova/*"},
	}
	if got, index := applyAliases(aliases, "sensenova/kimi-k3"); got != "sns/kimi-k3" || index != 0 {
		t.Errorf("第一条规则应当生效，实际 = %q（下标 %d）", got, index)
	}
	if got, index := applyAliases(aliases, "glm-5.2"); got != "sensenova/glm-5.2" || index != 1 {
		t.Errorf("未命中第一条时应落到第二条，实际 = %q（下标 %d）", got, index)
	}
	if got, index := applyAliases(nil, "glm-5.2"); got != "glm-5.2" || index != -1 {
		t.Errorf("没有规则时对外名应保持上游 id，实际 = %q（下标 %d）", got, index)
	}
}

func TestDiscoverAppliesExpose(t *testing.T) {
	fetcher := &stubFetcher{body: []byte(`{"data":[
		{"id":"sensenova/kimi-k3"},
		{"id":"glm-5.2"}
	]}`)}
	ep := testEndpoint()
	ep.Expose = []Alias{
		{From: "sensenova/*", To: "*"},
		{From: "nope/*", To: "x/*", File: "Novafile", Line: 9},
	}

	result, err := Discover(context.Background(), ep, fetcher)
	if err != nil {
		t.Fatalf("Discover 意外失败：%v", err)
	}
	if len(result.Models) != 2 {
		t.Fatalf("保留数 = %d，期望 2", len(result.Models))
	}
	// 命中的项：对外名去掉了前缀，上游 id 原样保留。
	if result.Models[0].ID != "sensenova/kimi-k3" || result.Models[0].Name != "kimi-k3" {
		t.Errorf("第一条 = %+v，期望上游 id 保留、对外名去前缀", result.Models[0])
	}
	// 没命中的项：对外名即上游 id。
	if result.Models[1].Name != "glm-5.2" {
		t.Errorf("第二条对外名 = %q，期望等于上游 id", result.Models[1].Name)
	}
	// 没命中任何清单项的规则要报回装配层，由它记一条提醒。
	if len(result.UnusedAliases) != 1 || result.UnusedAliases[0].From != "nope/*" {
		t.Errorf("未命中的规则 = %+v，期望只有 nope/*", result.UnusedAliases)
	}
	if result.UnusedAliases[0].Line != 9 {
		t.Errorf("未命中规则的位置 = %d 行，期望 9（提醒要指回配置行）", result.UnusedAliases[0].Line)
	}
}

// 过滤先于改名：被 allow / deny 拦下的项不会走到改名，因此也不会命中规则。
func TestDiscoverFiltersBeforeExpose(t *testing.T) {
	fetcher := &stubFetcher{body: []byte(`{"data":[
		{"id":"sensenova/kimi-k3"},
		{"id":"sensenova/glm-5.2"}
	]}`)}
	ep := testEndpoint()
	ep.Allow = []string{"sensenova/kimi-*"}
	ep.Expose = []Alias{{From: "sensenova/*", To: "*"}}

	result, err := Discover(context.Background(), ep, fetcher)
	if err != nil {
		t.Fatalf("Discover 意外失败：%v", err)
	}
	if len(result.Models) != 1 || result.Models[0].Name != "kimi-k3" {
		t.Errorf("保留的模型 = %+v，期望只有去前缀后的 kimi-k3", result.Models)
	}
	if len(result.Excluded) != 1 || result.Excluded[0] != "sensenova/glm-5.2" {
		t.Errorf("排除的模型 = %v，期望按上游 id 记录", result.Excluded)
	}
}

// 两条清单项改名后撞同一个对外名时报错：谁该生效在配置里没有表达。
func TestDiscoverRejectsCollidingExpose(t *testing.T) {
	fetcher := &stubFetcher{body: []byte(`{"data":[{"id":"a/x"},{"id":"b/x"}]}`)}
	ep := testEndpoint()
	ep.Expose = []Alias{
		{From: "a/*", To: "same"},
		{From: "b/*", To: "same"},
	}

	_, err := Discover(context.Background(), ep, fetcher)
	if err == nil {
		t.Fatal("撞名时应当报错")
	}
	for _, want := range []string{"a/x", "b/x", "改名后都是"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误 = %q，期望含 %q", err.Error(), want)
		}
	}
}

// 撞名检测在改名之后：两个不同的上游 id 本来不冲突，改名后仍不同名时不该报错。
func TestDiscoverAllowsDistinctNamesAfterExpose(t *testing.T) {
	fetcher := &stubFetcher{body: []byte(`{"data":[{"id":"a/x"},{"id":"b/y"}]}`)}
	ep := testEndpoint()
	ep.Expose = []Alias{{From: "*", To: "p/*"}}

	result, err := Discover(context.Background(), ep, fetcher)
	if err != nil {
		t.Fatalf("Discover 意外失败：%v", err)
	}
	// 同一个规则加同样的前缀，两个不同的上游 id 仍得到两个不同的对外名。
	if len(result.Models) != 2 || result.Models[0].Name != "p/a/x" || result.Models[1].Name != "p/b/y" {
		t.Errorf("保留的模型 = %+v，期望改名为 p/a/x 与 p/b/y", result.Models)
	}
}

func TestDiscoverWithoutExposeKeepsUpstreamIDs(t *testing.T) {
	fetcher := &stubFetcher{body: []byte(`{"data":[{"id":"openai/gpt-4o"}]}`)}
	result, err := Discover(context.Background(), testEndpoint(), fetcher)
	if err != nil {
		t.Fatalf("Discover 意外失败：%v", err)
	}
	if result.Models[0].Name != "openai/gpt-4o" {
		t.Errorf("对外名 = %q，期望等于上游 id", result.Models[0].Name)
	}
	if len(result.UnusedAliases) != 0 {
		t.Errorf("没有规则时不该有未命中项：%+v", result.UnusedAliases)
	}
}
