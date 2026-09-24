package config

import (
	"strings"
	"testing"
)

// routeProvider 造一段最小 provider，供 route 用例引用。
func routeProvider(name string) string {
	return "provider " + name + " {\n" +
		"    api_key k\n" +
		"    url https://" + name + ".example.com/v1/chat/completions\n" +
		"    model deepseek-chat\n" +
		"}\n"
}

// TestParseRouteMultipleCandidatesPerLine 守护一行多个渠道名与可选的逐项权重。
//
// 权重写在它所属渠道名之后，省略即 1：这与 model、api_key 的双记号是同一条惯例。
func TestParseRouteMultipleCandidatesPerLine(t *testing.T) {
	names := []string{"a", "b", "c", "d", "e", "f"}
	src := "version 1\n"
	for _, name := range names {
		src += routeProvider(name)
	}
	src += `route m {
    provider a b 2 c
    fallback d e f
}
`
	cfg := mustParse(t, src)

	route := cfg.ModelRoutes[0]
	want := []struct {
		provider string
		weight   int
		fallback bool
	}{
		{"a", 1, false},
		{"b", 2, false},
		{"c", 1, false},
		{"d", 1, true},
		{"e", 1, true},
		{"f", 1, true},
	}
	if len(route.Candidates) != len(want) {
		t.Fatalf("候选数 = %d，期望 %d", len(route.Candidates), len(want))
	}
	for i, expected := range want {
		got := route.Candidates[i]
		if got.Provider != expected.provider || got.Weight != expected.weight || got.Fallback != expected.fallback {
			t.Errorf("第 %d 条候选 = %+v，期望 %s（权重 %d、fallback %v）",
				i+1, got, expected.provider, expected.weight, expected.fallback)
		}
	}
}

func TestParseModelRoute(t *testing.T) {
	cfg := mustParse(t, "version 1\n"+
		routeProvider("a")+routeProvider("b")+
		`route deepseek* {
    balance
    provider a 3
    provider b 1
}
`)

	if len(cfg.ModelRoutes) != 1 {
		t.Fatalf("规则数 = %d，期望 1", len(cfg.ModelRoutes))
	}
	route := cfg.ModelRoutes[0]
	if route.Pattern != "deepseek*" {
		t.Errorf("模式 = %q，期望 deepseek*", route.Pattern)
	}
	if !route.Balanced {
		t.Error("写了 balance 应标记为按权重轮询分摊")
	}
	if len(route.Candidates) != 2 {
		t.Fatalf("候选数 = %d，期望 2", len(route.Candidates))
	}
	if route.Candidates[0].Provider != "a" || route.Candidates[0].Weight != 3 {
		t.Errorf("第一个候选 = %+v，期望 a 且权重 3", route.Candidates[0])
	}
	// 权重省略即 1。
	if route.Candidates[1].Provider != "b" || route.Candidates[1].Weight != 1 {
		t.Errorf("第二个候选 = %+v，期望 b 且权重 1", route.Candidates[1])
	}
}

// 规则可以写在 provider 之前：引用关系在整份配置解析完之后才校验。
func TestModelRouteMayPrecedeProviders(t *testing.T) {
	cfg := mustParse(t, "version 1\n"+
		`route deepseek* {
    provider a
}
`+routeProvider("a"))

	if len(cfg.ModelRoutes) != 1 {
		t.Fatalf("规则数 = %d，期望 1", len(cfg.ModelRoutes))
	}
}

func TestMatchModelPattern(t *testing.T) {
	tests := []struct {
		pattern string
		name    string
		want    bool
	}{
		{"deepseek-chat", "deepseek-chat", true},
		{"deepseek-chat", "deepseek-chat-mini", false}, // 两端隐式锚定
		{"deepseek*", "deepseek-chat", true},
		{"deepseek*", "deepseek-reasoner", true},
		{"deepseek*", "DeepSeek-chat", false}, // 对外名是精确匹配的，模式因此大小写敏感
		{"*", "任何名字", true},
		{"*", "", true},
		{"gpt-?", "gpt-5", true},
		{"gpt-?", "gpt-55", false},
		{"gpt-?5", "gpt-15", true},
		{"openai/gpt-*", "openai/gpt-4o", true}, // * 可以跨 /
		{"gpt*5", "gpt-4", false},
		{"gpt*5", "gpt-3.5", true},
	}
	for _, tt := range tests {
		t.Run(tt.pattern+"/"+tt.name, func(t *testing.T) {
			if got := matchModelPattern(tt.pattern, tt.name); got != tt.want {
				t.Errorf("matchModelPattern(%q, %q) = %v，期望 %v", tt.pattern, tt.name, got, tt.want)
			}
		})
	}
}

// 第一条命中的规则生效：更具体的写在前面即可，不需要按具体程度排序。
func TestModelRouteFirstMatchWins(t *testing.T) {
	cfg := mustParse(t, "version 1\n"+routeProvider("a")+
		`route deepseek-chat {
    provider a
}
route deepseek* {
    provider a
}
`)

	tests := []struct {
		name  string
		want  int
		found bool
	}{
		{"deepseek-chat", 0, true},
		{"deepseek-reasoner", 1, true},
		{"gpt-5", -1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cfg.ModelRouteIndexFor(tt.name)
			if tt.found && got != tt.want {
				t.Errorf("规则下标 = %d，期望 %d", got, tt.want)
			}
			if !tt.found {
				if got != -1 {
					t.Errorf("未命中应返回 -1，实际 %d", got)
				}
				if cfg.ModelRouteFor(tt.name) != nil {
					t.Error("未命中时 ModelRouteFor 应返回 nil")
				}
			}
		})
	}
}

func TestModelRouteErrors(t *testing.T) {
	providers := routeProvider("a") + routeProvider("b")
	tests := []struct {
		name    string
		src     string
		wantMsg []string
	}{
		{
			name:    "块头缺少花括号",
			src:     "route deepseek*\n",
			wantMsg: []string{"route 块写成"},
		},
		{
			name:    "块头缺模式",
			src:     "route {\n    provider a\n}\n",
			wantMsg: []string{"route 块写成"},
		},
		{
			name:    "模式含空白",
			src:     "route \"a b\" {\n    provider a\n}\n",
			wantMsg: []string{"不能含空白"},
		},
		{
			name:    "没有列出候选",
			src:     "route deepseek* {\n    balance\n}\n",
			wantMsg: []string{"没有列出任何候选"},
		},
		{
			name:    "块没有收尾",
			src:     "route deepseek* {\n    provider a\n",
			wantMsg: []string{"没有收尾的 }"},
		},
		{
			name:    "块内不能再开块",
			src:     "route deepseek* {\n    provider a {\n    }\n}\n",
			wantMsg: []string{"不能再开块"},
		},
		{
			name:    "balance 带取值",
			src:     "route deepseek* {\n    provider a\n    balance on\n}\n",
			wantMsg: []string{"不接受取值"},
		},
		{
			name:    "balance 写两次",
			src:     "route deepseek* {\n    provider a\n    balance\n    balance\n}\n",
			wantMsg: []string{"不能写两次"},
		},
		{
			name:    "provider 缺渠道名",
			src:     "route deepseek* {\n    provider\n}\n",
			wantMsg: []string{"缺少渠道名"},
		},
		{
			name:    "fallback 缺渠道名",
			src:     "route deepseek* {\n    provider a\n    fallback\n}\n",
			wantMsg: []string{"缺少渠道名"},
		},
		{
			name:    "纯数字不能作渠道名",
			src:     "route deepseek* {\n    provider 3\n}\n",
			wantMsg: []string{"纯数字"},
		},
		{
			name:    "权重为零",
			src:     "route deepseek* {\n    provider a 0\n}\n",
			wantMsg: []string{"不是正整数"},
		},
		{
			name:    "渠道重复",
			src:     "route deepseek* {\n    provider a\n    provider a 2\n}\n",
			wantMsg: []string{"已经在这条规则里列过"},
		},
		{
			name:    "块头重复",
			src:     "route deepseek* {\n    provider a\n}\nroute deepseek* {\n    provider b\n}\n",
			wantMsg: []string{"已在", "第二条永远不会被用到"},
		},
		{
			name:    "引用不存在的渠道",
			src:     "route deepseek* {\n    provider ghost\n}\n",
			wantMsg: []string{"没有对应的 provider 块"},
		},
		{
			name:    "块内写未知指令",
			src:     "route deepseek* {\n    provider a\n    timeout 5s\n}\n",
			wantMsg: []string{"未知指令"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := parseErr(t, "version 1\n"+providers+tt.src)
			for _, want := range tt.wantMsg {
				if !strings.Contains(err.Msg, want) {
					t.Errorf("消息 = %q，期望包含 %q", err.Msg, want)
				}
			}
		})
	}
}

// route 与 provider 在指令清单里都在：二进制自己回答「认哪些写法」。
func TestRouteIsListedAsInstruction(t *testing.T) {
	found := false
	for _, name := range InstructionNames() {
		if name == directiveRoute {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("指令清单里没有 %s：%v", directiveRoute, InstructionNames())
	}
}
