package config

import (
	"strings"
	"testing"
)

// account 指令可写多条，顺序即账号顺序，权重走现有 balance 口径。
func TestAccountLinesPreserveOrderAndWeight(t *testing.T) {
	cfg := mustParse(t, `
version 1
provider cmd {
    profile opencode-go
    balance
    account work
    account personal weight 2
}
`)

	provider := cfg.Providers[0]
	if !provider.ProfileRef || provider.Profile != "opencode-go" {
		t.Fatalf("渠道 = %+v，期望保留档案引用", provider)
	}
	if len(provider.Accounts) != 2 {
		t.Fatalf("账号数 = %d，期望 2：%+v", len(provider.Accounts), provider.Accounts)
	}
	first := provider.Accounts[0]
	if first.Name != "work" || first.APIKey != "" || first.Weight != 1 || first.Index != 1 || first.Ref() != "#1" {
		t.Errorf("第一个账号 = %+v，期望 work、权重 1、序号 1、密钥待展开", first)
	}
	second := provider.Accounts[1]
	if second.Name != "personal" || second.Weight != 2 || second.Index != 2 || second.Ref() != "#2" {
		t.Errorf("第二个账号 = %+v，期望 personal、权重 2、序号 2", second)
	}
	if !provider.HasAccountPool() {
		t.Error("两个账号应被认作账号池")
	}
}

// 块内写了 account 但没有写 profile 时，按渠道名引用同名档案。
//
// 这是任务给出的写法：`provider opencode-go { account work }` 与单行
// `provider opencode-go` 表达同一条引用，加一个块只是为了写账号。
func TestAccountLineWithoutProfileReferencesSameName(t *testing.T) {
	cfg := mustParse(t, `
version 1
provider opencode-go {
    account work
    account personal weight 2
}
`)

	provider := cfg.Providers[0]
	if !provider.ProfileRef || provider.Profile != "opencode-go" {
		t.Fatalf("渠道 = %+v，期望按渠道名引用同名档案", provider)
	}
	if len(provider.Accounts) != 2 {
		t.Fatalf("账号数 = %d，期望 2", len(provider.Accounts))
	}
}

// 写了 account 又写了 profile 时，以显式写下的档案 id 为准。
func TestAccountLineKeepsExplicitProfile(t *testing.T) {
	cfg := mustParse(t, `
version 1
provider cmd {
    profile opencode-go
    account work
}
`)

	provider := cfg.Providers[0]
	if !provider.ProfileRef || provider.Profile != "opencode-go" {
		t.Fatalf("渠道 = %+v，期望保留显式档案 id", provider)
	}
}

// account 的权重只有 weight 一种写法，权重值必须是正整数。
func TestAccountLineSyntaxErrors(t *testing.T) {
	tests := []struct {
		name string
		line string
		want string
	}{
		{"缺账号名", "account", "缺少账号名"},
		{"第二个取值不是 weight", "account work 2", "只有 weight"},
		{"权重不是正整数", "account work weight 0", "不是正整数"},
		{"取值过多", "account work weight 2 extra", "多出来"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cerr := parseErr(t, "version 1\nprovider cmd {\n    profile opencode-go\n    "+tt.line+"\n}\n")
			if !strings.Contains(cerr.Msg, tt.want) {
				t.Errorf("消息 = %q，期望包含 %q", cerr.Msg, tt.want)
			}
		})
	}
}
