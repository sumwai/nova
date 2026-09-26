package config

import (
	"strings"
	"testing"
)

// TestProviderShorthandReferencesProfile 守护 `provider <名>` 单行形态：它等价于
// 引用同名档案，不要求配置里写出凭据、端点或模型。
func TestProviderShorthandReferencesProfile(t *testing.T) {
	cfg := mustParse(t, "version 1\nprovider opencode-go\nprovider deepseek-official\n")

	if len(cfg.Providers) != 2 {
		t.Fatalf("provider 数 = %d，期望 2", len(cfg.Providers))
	}
	for i, want := range []string{"opencode-go", "deepseek-official"} {
		provider := cfg.Providers[i]
		if provider.Name != want {
			t.Errorf("第 %d 条名字 = %q，期望 %q", i, provider.Name, want)
		}
		if provider.Profile != want {
			t.Errorf("provider %s 的档案 id = %q，期望同名", provider.Name, provider.Profile)
		}
		if !provider.ProfileRef {
			t.Errorf("provider %s 应处于未展开的引用形态", provider.Name)
		}
		if len(provider.Accounts) != 0 || len(provider.Endpoints) != 0 {
			t.Errorf("provider %s 不该携带从档案展开的账号或端点：%+v",
				provider.Name, provider)
		}
	}
}

// TestProviderBlockProfileDirective 守护块内 `profile <id>`：块名与档案 id 不同时，
// 由 profile 决定引用哪份档案，块名只作渠道标识。
func TestProviderBlockProfileDirective(t *testing.T) {
	cfg := mustParse(t, `
version 1
provider cmd {
    profile commandcode
    api_key k
}
`)

	if len(cfg.Providers) != 1 {
		t.Fatalf("provider 数 = %d，期望 1", len(cfg.Providers))
	}
	provider := cfg.Providers[0]
	if provider.Name != "cmd" {
		t.Errorf("块名 = %q，期望 cmd", provider.Name)
	}
	if provider.Profile != "commandcode" {
		t.Errorf("档案 id = %q，期望 commandcode（来自 profile 指令，而非块名）", provider.Profile)
	}
	if !provider.ProfileRef {
		t.Error("写了 profile 的渠道应处于未展开的引用形态")
	}
	// 手写的 api_key 是展开阶段的输入，解析层保留它。
	if len(provider.Accounts) != 1 || provider.Accounts[0].APIKey != "k" {
		t.Errorf("账号 = %+v，期望保留手写的 api_key", provider.Accounts)
	}
}

// 块内同时写手写声明与 profile：引用形态成立，手写内容保留，缺端点不报错。
// 展开阶段的合并口径以档案为准，解析层只负责把两边的声明都交出去。
func TestProviderProfileWithManualDeclarations(t *testing.T) {
	cfg := mustParse(t, `
version 1
provider cmd {
    profile commandcode
    api_key k
    url https://a.example.com/v1/chat/completions
    model m
}
`)

	provider := cfg.Providers[0]
	if !provider.ProfileRef || provider.Profile != "commandcode" {
		t.Errorf("引用形态 = %+v，期望 ProfileRef 且 Profile=commandcode", provider)
	}
	if len(provider.Endpoints) != 1 || len(provider.Endpoints[0].Models) != 1 {
		t.Errorf("手写的端点与模型应保留：%+v", provider.Endpoints)
	}
}

// 引用形态缺 url / api_key / model 都不再触发三条必填断言。
func TestProviderReferenceSkipsRequiredFields(t *testing.T) {
	for _, src := range []string{
		"version 1\nprovider opencode-go\n",
		"version 1\nprovider cmd {\n    profile commandcode\n}\n",
	} {
		if _, err := Parse([]byte(src), "Novafile"); err != nil {
			t.Errorf("引用形态不该报必填字段：%v\n源码：%s", err, src)
		}
	}
}

// 引用形态下重名仍然拒绝：名字出现在日志与报错里，重名会指不准。
func TestProviderReferenceRejectsDuplicateName(t *testing.T) {
	err := parseErr(t, `
version 1
provider opencode-go
provider opencode-go {
    profile commandcode
}
`)

	for _, want := range []string{"已在", "声明过"} {
		if !strings.Contains(err.Msg, want) {
			t.Errorf("消息 = %q，期望包含 %q", err.Msg, want)
		}
	}
}

// TestProviderReferenceMissingBraceReportsBlockSyntax 守护「漏写 {」的定位不被丢失。
//
// `provider <名>` 单独成行是档案引用，后面紧跟 provider 级指令说明这一行漏了 `{`。
// 不在这里报，错误会落到下一行变成「未知指令 api_key」，指向的不是要改的那一行。
func TestProviderReferenceMissingBraceReportsBlockSyntax(t *testing.T) {
	tests := []struct {
		name string
		src  string
	}{
		{name: "紧跟 api_key", src: "version 1\nprovider cmd\n    api_key k\n}\n"},
		{name: "紧跟 profile", src: "version 1\nprovider cmd\n    profile local\n}\n"},
		{
			name: "紧跟 endpoint 块",
			src: "version 1\nprovider cmd\n" +
				"    endpoint https://a.example.com/v1/chat/completions {\n        model m\n    }\n}\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := parseErr(t, tt.src)
			if !strings.Contains(err.Msg, "花括号") {
				t.Errorf("消息 = %q，期望指出缺少块头花括号", err.Msg)
			}
			if err.Line != 2 {
				t.Errorf("定位 = %s:%d，期望定位到 provider 那一行第 2 行", err.File, err.Line)
			}
		})
	}
}

// profile 在同一渠道里只能写一次，走 rejectRepeat 口径并指出前一次的位置。
func TestProviderProfileWrittenTwice(t *testing.T) {
	err := parseErr(t, `
version 1
provider cmd {
    profile first
    profile second
}
`)

	for _, want := range []string{"profile", "已在", "不能写两次"} {
		if !strings.Contains(err.Msg, want) {
			t.Errorf("消息 = %q，期望包含 %q", err.Msg, want)
		}
	}
}

// profile 的取值是档案 id，必须是词记号；占位符在这里没有可展开的对象。
func TestProviderProfileRejectsPlaceholder(t *testing.T) {
	err := parseErr(t, `
version 1
provider cmd {
    profile {env.CMD_PROFILE}
}
`)

	if !strings.Contains(err.Msg, "档案 id") {
		t.Errorf("消息 = %q，期望指出取值应是档案 id", err.Msg)
	}
}
