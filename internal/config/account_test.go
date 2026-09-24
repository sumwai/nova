package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestRepeatedAPIKeysBecomeAccounts 守护「一条 api_key 声明一个账号」：
//
// 多账号是同一句指令的重复，不是另一种写法。序号按行序给出，权重缺省 1，
// 因此「加一个账号」等于复制一行。
func TestRepeatedAPIKeysBecomeAccounts(t *testing.T) {
	cfg := mustParse(t, `
version 1
provider openai {
    api_key k1
    api_key k2 3
    url https://a.example.com/v1/chat/completions
    model m
}
`)

	provider := cfg.Providers[0]
	if len(provider.Accounts) != 2 {
		t.Fatalf("账号数 = %d，期望 2", len(provider.Accounts))
	}
	first := provider.Accounts[0]
	if first.APIKey != "k1" || first.Weight != 1 || first.Index != 1 || first.Ref() != "#1" {
		t.Errorf("第一个账号 = %+v，期望 k1、权重 1、序号 1", first)
	}
	second := provider.Accounts[1]
	if second.APIKey != "k2" || second.Weight != 3 || second.Index != 2 || second.Ref() != "#2" {
		t.Errorf("第二个账号 = %+v，期望 k2、权重 3、序号 2", second)
	}
	if provider.Balanced {
		t.Error("未写 balance 时不该标记为分摊")
	}
	if !provider.HasAccountPool() {
		t.Error("两个账号应被认作账号池")
	}
	if !cfg.Providers[0].HasAccountPool() {
		t.Error("HasAccountPool 应报告这条渠道有账号池")
	}
}

// TestSingleAccountIsNotPool 守护「单账号与账号池引入之前没有区别」：
//
// 路由里因此不必带账号引用，日志与凭据表也不必多出一层。
func TestSingleAccountIsNotPool(t *testing.T) {
	cfg := mustParse(t, `
version 1
provider openai {
    api_key k
    url https://a.example.com/v1/chat/completions
    model m
}
`)

	if cfg.Providers[0].HasAccountPool() {
		t.Error("单账号不该被认作账号池")
	}
	if got := len(cfg.Providers[0].Accounts); got != 1 {
		t.Errorf("账号数 = %d，期望 1", got)
	}
}

func TestBalanceMarksPoolAsShared(t *testing.T) {
	cfg := mustParse(t, `
version 1
provider openai {
    api_key k1
    api_key k2
    balance
    url https://a.example.com/v1/chat/completions
    model m
}
`)

	if !cfg.Providers[0].Balanced {
		t.Error("写了 balance 应标记为按权重轮询分摊")
	}
}

func TestAccountErrors(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		wantMsg []string
	}{
		{
			name:    "权重为零",
			src:     "provider p {\n    api_key k 0\n    url https://a.example.com/v1/chat/completions\n    model m\n}\n",
			wantMsg: []string{"不是正整数"},
		},
		{
			name:    "权重为负",
			src:     "provider p {\n    api_key k -2\n    url https://a.example.com/v1/chat/completions\n    model m\n}\n",
			wantMsg: []string{"不是正整数"},
		},
		{
			name:    "权重不是数字",
			src:     "provider p {\n    api_key k 很多\n    url https://a.example.com/v1/chat/completions\n    model m\n}\n",
			wantMsg: []string{"不是正整数"},
		},
		{
			name:    "api_key 取值过多",
			src:     "provider p {\n    api_key k 1 2\n    url https://a.example.com/v1/chat/completions\n    model m\n}\n",
			wantMsg: []string{"至多接受两个取值"},
		},
		{
			name:    "api_key 缺密钥",
			src:     "provider p {\n    api_key\n    url https://a.example.com/v1/chat/completions\n    model m\n}\n",
			wantMsg: []string{"缺少密钥"},
		},
		{
			name:    "balance 带取值",
			src:     "provider p {\n    api_key k\n    balance on\n    url https://a.example.com/v1/chat/completions\n    model m\n}\n",
			wantMsg: []string{"不接受取值"},
		},
		{
			name:    "balance 写两次",
			src:     "provider p {\n    api_key k\n    balance\n    balance\n    url https://a.example.com/v1/chat/completions\n    model m\n}\n",
			wantMsg: []string{"不能写两次"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := parseErr(t, "version 1\n"+tt.src)
			for _, want := range tt.wantMsg {
				if !strings.Contains(err.Msg, want) {
					t.Errorf("消息 = %q，期望包含 %q", err.Msg, want)
				}
			}
		})
	}
}

// TestAccountPolicyWarnings 覆盖两种「写下的配置没有作用对象」的提醒。
//
// 它们不阻止加载：单账号加 balance 是一份准备扩到多账号的配置。
// 静默才是问题——使用者会以为写下的那个数已经生效了。
func TestAccountPolicyWarnings(t *testing.T) {
	cfg := mustParse(t, `
version 1
provider one {
    api_key k
    balance
    url https://a.example.com/v1/chat/completions
    model m
}
provider two {
    api_key k1
    api_key k2 5
    url https://b.example.com/v1/chat/completions
    model m
}
`)

	var texts []string
	for _, warn := range cfg.Warnings {
		texts = append(texts, warn.String())
	}
	joined := strings.Join(texts, "\n")
	if !strings.Contains(joined, "没有可分摊的对象") {
		t.Errorf("期望提醒「单账号的 balance 没有对象」，实际提醒：\n%s", joined)
	}
	if !strings.Contains(joined, "权重没有作用") {
		t.Errorf("期望提醒「未写 balance 时权重无作用」，实际提醒：\n%s", joined)
	}
}

// TestImportAccountPoolFile 守护账号池文件：它就是一段 provider 块体，用 import 拼进来。
//
// 拼接后每行仍带自己的文件与行号，因此池文件里写错的一行会指回池文件，
// 而不是主配置里某个不相干的位置。
func TestImportAccountPoolFile(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "Novafile")
	pool := filepath.Join(dir, "accounts.nova")
	writeFile(t, main, `version 1
provider openai {
    import accounts.nova
    url https://a.example.com/v1/chat/completions
    model m
}
`)
	writeFile(t, pool, "# 账号池\napi_key k1\napi_key k2 3\n")

	cfg, err := Load(main)
	if err != nil {
		t.Fatalf("Load 意外失败：%v", err)
	}
	accounts := cfg.Providers[0].Accounts
	if len(accounts) != 2 {
		t.Fatalf("账号数 = %d，期望 2", len(accounts))
	}
	if accounts[1].APIKey != "k2" || accounts[1].Weight != 3 {
		t.Errorf("第二个账号 = %+v，期望从池文件读入 k2 与权重 3", accounts[1])
	}
	if accounts[1].File != pool {
		t.Errorf("账号位置 = %q，期望指向池文件 %q", accounts[1].File, pool)
	}

	// 池文件里写错的一行，报错必须指回池文件的那一行。
	writeFile(t, pool, "# 账号池\napi_key k1\napi_key k2 0\n")
	_, err = Load(main)
	cerr := configError(t, err)
	if cerr.File != pool {
		t.Errorf("报错文件 = %q，期望 %q", cerr.File, pool)
	}
	if cerr.Line != 3 {
		t.Errorf("报错行号 = %d，期望 3", cerr.Line)
	}
}

// TestBalanceIsListedAsInstruction 守护指令清单自动包含 balance：
// 二进制自己回答「认哪些写法」是排障的入口，漏掉一条就等于骗人。
func TestBalanceIsListedAsInstruction(t *testing.T) {
	found := false
	for _, name := range InstructionNames() {
		if name == directiveBalance {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("指令清单里没有 %s：%v", directiveBalance, InstructionNames())
	}
}
