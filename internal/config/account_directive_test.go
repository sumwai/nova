package config

import (
	"strings"
	"testing"
)

const accountDirectiveNovafile = `
version 1
provider commandcode {
    url https://api.commandcode.ai/v1/chat/completions
    model gpt-5
    account default
}
`

// account 指令的取值来自注入的 lookup，命名空间省略时取渠道名。
func TestParseAccountDirectiveResolvesFromLookup(t *testing.T) {
	lookup := func(namespace, name string) (string, bool) {
		keys := map[string]string{
			"commandcode.default": "sk-cc-default",
			"commandcode.work":    "sk-cc-work",
			"shared.work":         "sk-shared-work",
		}
		key, ok := keys[namespace+"."+name]
		return key, ok
	}
	cfg, err := ParseWith([]byte(`
version 1
provider commandcode {
    url https://api.commandcode.ai/v1/chat/completions
    model gpt-5
    account default
    account work 3
    account shared.work
}
`), "Novafile", Options{LookupAccount: lookup})
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}

	accounts := cfg.Providers[0].Accounts
	if len(accounts) != 3 {
		t.Fatalf("账号数 = %d，期望 3", len(accounts))
	}
	if accounts[0].Namespace != "commandcode" || accounts[0].Name != "default" ||
		accounts[0].APIKey != "sk-cc-default" {
		t.Errorf("第 1 个账号 = %+v", accounts[0])
	}
	if accounts[1].APIKey != "sk-cc-work" || accounts[1].Weight != 3 {
		t.Errorf("第 2 个账号 = %+v", accounts[1])
	}
	if accounts[2].Namespace != "shared" || accounts[2].Name != "work" ||
		accounts[2].APIKey != "sk-shared-work" {
		t.Errorf("第 3 个账号 = %+v", accounts[2])
	}
	// Index 按声明顺序，api_key 与 account 混排时也如此。
	for i, account := range accounts {
		if account.Index != i+1 {
			t.Errorf("第 %d 个账号的 Index = %d，期望 %d", i+1, account.Index, i+1)
		}
	}
}

// api_key 与 account 可以混排：两种来源的账号同属一个账号池。
func TestParseAccountDirectiveMixedWithAPIKey(t *testing.T) {
	lookup := func(namespace, name string) (string, bool) {
		if namespace == "relay" && name == "second" {
			return "sk-second", true
		}
		return "", false
	}
	cfg, err := ParseWith([]byte(`
version 1
provider relay {
    url https://relay.example.com/v1/chat/completions
    api_key sk-first
    account second
    model gpt-5
}
`), "Novafile", Options{LookupAccount: lookup})
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}

	accounts := cfg.Providers[0].Accounts
	if len(accounts) != 2 {
		t.Fatalf("账号数 = %d，期望 2", len(accounts))
	}
	if accounts[0].APIKey != "sk-first" || accounts[0].Name != "" {
		t.Errorf("第 1 个账号 = %+v，期望来自 api_key", accounts[0])
	}
	if accounts[1].APIKey != "sk-second" || accounts[1].Name != "second" {
		t.Errorf("第 2 个账号 = %+v，期望来自 account", accounts[1])
	}
}

// 引用的账号不在凭据库里时报错，并给出下一步命令。
func TestParseAccountDirectiveMissingAccount(t *testing.T) {
	_, err := ParseWith([]byte(accountDirectiveNovafile), "Novafile",
		Options{LookupAccount: func(string, string) (string, bool) { return "", false }})
	if err == nil {
		t.Fatal("账号缺失应报错")
	}
	for _, want := range []string{"不在凭据库里", "commandcode", "nova login"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误 = %q，期望含 %q", err, want)
		}
	}
}

// 没有凭据来源（lookup 为零值）时报错，而不是静默留空。
func TestParseAccountDirectiveWithoutLookup(t *testing.T) {
	_, err := ParseWith([]byte(accountDirectiveNovafile), "Novafile", Options{})
	if err == nil {
		t.Fatal("没有 lookup 时 account 指令应报错")
	}
	if !strings.Contains(err.Error(), "不在凭据库里") {
		t.Errorf("错误 = %q，期望说明凭据取不到", err)
	}
}

// 宽松模式下取不到值不报错，归类留给真正使用它的调用方。
func TestParseAccountDirectiveLenient(t *testing.T) {
	cfg, err := ParseWith([]byte(accountDirectiveNovafile), "Novafile",
		Options{IgnoreMissingAccount: true})
	if err != nil {
		t.Fatalf("宽松模式不该报错：%v", err)
	}
	if key := cfg.Providers[0].Accounts[0].APIKey; key != "" {
		t.Errorf("取值 = %q，期望留空", key)
	}
	if len(cfg.Warnings) == 0 {
		t.Fatal("宽松模式应记一条「尚未登录」提醒，不能无声留空")
	}
	if !strings.Contains(cfg.Warnings[0].Msg, "尚未登录") {
		t.Errorf("提醒 = %q，期望说明尚未登录", cfg.Warnings[0].Msg)
	}
}

// account 的语法错误各有明确的报错，不做静默兜底。
func TestParseAccountDirectiveSyntaxErrors(t *testing.T) {
	tests := []struct {
		name string
		line string
		want string
	}{
		{"缺少取值", "    account", "缺少账号引用"},
		{"取值过多", "    account a b c", "至多接受两个取值"},
		{"命名空间为空", "    account .work", "格式不对"},
		{"账号名为空", "    account work.", "格式不对"},
		{"权重非正整数", "    account work 0", "不是正整数"},
		{"权重不是数字", "    account work many", "不是正整数"},
		{"取值是占位符", "    account {env.NAME}", "不能是花括号"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseWith([]byte(`
version 1
provider p {
    url https://api.example.com/v1/chat/completions
    model gpt-5
`+tt.line+`
}
`), "Novafile", Options{IgnoreMissingAccount: true})
			if err == nil {
				t.Fatalf("%s 应报错", tt.name)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("错误 = %q，期望含 %q", err, tt.want)
			}
		})
	}
}

// IgnoreMissingAccount 不覆盖 {env.NAME}：只校验写法的命令仍要求进程环境齐备。
func TestIgnoreMissingAccountDoesNotRelaxEnv(t *testing.T) {
	_, err := ParseWith([]byte(`
version 1
provider p {
    url https://api.example.com/v1/chat/completions
    api_key {env.NOVA_TEST_MISSING}
    model gpt-5
}
`), "Novafile", Options{
		IgnoreMissingAccount: true,
		Getenv:               func(string) string { return "" },
	})
	if err == nil {
		t.Fatal("{env.NAME} 缺失仍应报错")
	}
}

// IgnoreMissingEnv 只放宽 {env.NAME}，account 仍要求取到值。
func TestIgnoreMissingEnvDoesNotRelaxAccount(t *testing.T) {
	_, err := ParseWith([]byte(accountDirectiveNovafile), "Novafile",
		Options{IgnoreMissingEnv: true})
	if err == nil {
		t.Fatal("account 缺失仍应报错")
	}
}

// 宽松口径下未导出的 {env.NAME} 不拦住 client_key；严格口径下仍报错。
func TestIgnoreMissingEnvSkipsClientKey(t *testing.T) {
	novafile := []byte(`
version 1
client_key {env.NOVA_TEST_UNSET}
provider p {
    url https://api.example.com/v1/chat/completions
    model gpt-5
    account default
}
`)
	setting := Options{
		IgnoreMissingEnv:     true,
		IgnoreMissingAccount: true,
		Getenv:               func(string) string { return "" },
	}
	if _, err := ParseWith(novafile, "Novafile", setting); err != nil {
		t.Fatalf("宽松口径不该因 client_key 报错：%v", err)
	}

	strict := setting
	strict.IgnoreMissingEnv = false
	if _, err := ParseWith(novafile, "Novafile", strict); err == nil {
		t.Fatal("严格口径下 client_key 展开为空仍应报错")
	}
}

// account 可以写多次，与 api_key 一样是「一句一行」的账号声明。
func TestParseAccountDirectiveCanRepeat(t *testing.T) {
	lookup := func(namespace, name string) (string, bool) { return "k-" + name, true }
	cfg, err := ParseWith([]byte(`
version 1
provider p {
    url https://api.example.com/v1/chat/completions
    model gpt-5
    account a
    account b
    account c
}
`), "Novafile", Options{LookupAccount: lookup})
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if got := len(cfg.Providers[0].Accounts); got != 3 {
		t.Errorf("账号数 = %d，期望 3", got)
	}
}
