package profileapply

import (
	"strings"
	"testing"

	"github.com/sumwai/nova/internal/credentials"
)

// credentialSeed 是一条要写进临时凭据库的记录；用切片而不是 map 保住写入顺序。
type credentialSeed struct {
	profile string
	name    string
	key     string
}

// credentialsDir 造一个临时配置目录，并往里写入若干账号。
func credentialsDir(t *testing.T, seeds ...credentialSeed) string {
	t.Helper()
	dir := t.TempDir()
	store, err := credentials.Load(dir)
	if err != nil {
		t.Fatalf("加载临时凭据库失败：%v", err)
	}
	for _, seed := range seeds {
		if err := store.Set(seed.profile, seed.name, seed.key); err != nil {
			t.Fatalf("写入临时凭据库失败：%v", err)
		}
	}
	return dir
}

// account 行按顺序从凭据库取密钥，权重与顺序都保留。
//
// 配置就用任务给出的写法：只写 account，不写 profile，渠道名即档案 id。
func TestAccountLinesResolveFromStore(t *testing.T) {
	cfg := applySrc(t, `
version 1
provider opencode-go {
    account work
    account personal weight 2
}
`, Options{ConfigDir: credentialsDir(t,
		credentialSeed{"opencode-go", "work", "sk-work"},
		credentialSeed{"opencode-go", "personal", "sk-personal"},
	)})

	provider := cfg.Providers[0]
	if len(provider.Accounts) != 2 {
		t.Fatalf("账号数 = %d，期望 2：%+v", len(provider.Accounts), provider.Accounts)
	}
	if provider.Accounts[0].Name != "work" || provider.Accounts[0].APIKey != "sk-work" ||
		provider.Accounts[0].Weight != 1 || provider.Accounts[0].Ref() != "#1" {
		t.Errorf("第一个账号 = %+v，期望 work、sk-work、权重 1、序号 1", provider.Accounts[0])
	}
	if provider.Accounts[1].Name != "personal" || provider.Accounts[1].APIKey != "sk-personal" ||
		provider.Accounts[1].Weight != 2 || provider.Accounts[1].Ref() != "#2" {
		t.Errorf("第二个账号 = %+v，期望 personal、sk-personal、权重 2、序号 2", provider.Accounts[1])
	}
	if !provider.HasAccountPool() {
		t.Error("两个账号应被认作账号池")
	}
}

// 不写 account 时，凭据库里存在 default 就只取 default。
func TestDefaultAccountIsPreferred(t *testing.T) {
	cfg := applySrc(t, "version 1\nprovider opencode-go\n",
		Options{ConfigDir: credentialsDir(t,
			credentialSeed{"opencode-go", "default", "sk-default"},
			credentialSeed{"opencode-go", "other", "sk-other"},
		)})

	provider := cfg.Providers[0]
	if len(provider.Accounts) != 1 {
		t.Fatalf("账号数 = %d，期望只取 default：%+v", len(provider.Accounts), provider.Accounts)
	}
	if provider.Accounts[0].APIKey != "sk-default" || provider.Accounts[0].Ref() != "#1" {
		t.Errorf("账号 = %+v，期望 default 的密钥", provider.Accounts[0])
	}
}

// 没有 default 时取凭据库里的全部账号，顺序即加入顺序。
func TestAllStoreAccountsBecomeProviderAccounts(t *testing.T) {
	cfg := applySrc(t, "version 1\nprovider opencode-go\n",
		Options{ConfigDir: credentialsDir(t,
			credentialSeed{"opencode-go", "alpha", "sk-alpha"},
			credentialSeed{"opencode-go", "beta", "sk-beta"},
		)})

	provider := cfg.Providers[0]
	if len(provider.Accounts) != 2 {
		t.Fatalf("账号数 = %d，期望 2：%+v", len(provider.Accounts), provider.Accounts)
	}
	if provider.Accounts[0].APIKey != "sk-alpha" || provider.Accounts[0].Ref() != "#1" {
		t.Errorf("第一个账号 = %+v，期望 alpha 排在前", provider.Accounts[0])
	}
	if provider.Accounts[1].APIKey != "sk-beta" || provider.Accounts[1].Ref() != "#2" {
		t.Errorf("第二个账号 = %+v，期望 beta 排在 #2", provider.Accounts[1])
	}
}

// 凭据库优先于档案 auth.env 的环境变量。
func TestCredentialsWinOverProfileEnv(t *testing.T) {
	cfg := applySrc(t, "version 1\nprovider opencode-go\n", Options{
		ConfigDir: credentialsDir(t, credentialSeed{"opencode-go", "default", "sk-store"}),
		Getenv:    envOf(map[string]string{"OPENCODE_API_KEY": "sk-env"}),
	})

	if got := cfg.Providers[0].Accounts[0].APIKey; got != "sk-store" {
		t.Errorf("凭据 = %q，期望凭据库里的 sk-store", got)
	}
}

// 块内 api_key 优先，并提醒凭据库里同档案的账号被忽略。
func TestExplicitAPIKeyWinsWithWarning(t *testing.T) {
	cfg := applySrc(t, "version 1\nprovider cmd {\n    profile opencode-go\n    api_key manual\n}\n",
		Options{ConfigDir: credentialsDir(t,
			credentialSeed{"opencode-go", "default", "sk-store"},
			credentialSeed{"opencode-go", "work", "sk-work"},
		)})

	if got := cfg.Providers[0].Accounts[0].APIKey; got != "manual" {
		t.Errorf("凭据 = %q，期望块内的 manual", got)
	}
	assertWarning(t, cfg, "被忽略")
	assertWarning(t, cfg, "default")
}

// account 行引用了凭据库里没有的账号时报错，并说清有哪些账号、下一步敲什么。
func TestMissingNamedAccountReportsAvailable(t *testing.T) {
	err := applyErr(t, "version 1\nprovider cmd {\n    profile opencode-go\n    account nope\n}\n",
		Options{ConfigDir: credentialsDir(t, credentialSeed{"opencode-go", "work", "sk-work"})})

	if err == nil {
		t.Fatal("引用不存在的账号应报错")
	}
	for _, want := range []string{"nope", "work", "凭据库", "nova login opencode-go nope"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误 = %v，期望包含 %q", err, want)
		}
	}
}

// 三条来源都没有时报错，并给出 nova login 与 {env.NAME} 两条路。
func TestAllCredentialSourcesMissingMentionsBothPaths(t *testing.T) {
	err := applyErr(t, "version 1\nprovider opencode-go\n",
		Options{ConfigDir: t.TempDir(), Getenv: envOf(nil)})

	if err == nil {
		t.Fatal("没有任何凭据应报错")
	}
	for _, want := range []string{"nova login", "{env.NAME}", "OPENCODE_API_KEY"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误 = %v，期望包含 %q", err, want)
		}
	}
}
