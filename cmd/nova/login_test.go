package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sumwai/nova/internal/keystore"
)

// loginNovafile 是测试用的最小配置：两条渠道，凭据都声明为从凭据库取。
const loginNovafile = `
version 1
provider commandcode {
    url https://api.commandcode.ai/v1/chat/completions
    model gpt-5
    account default
}
provider relay {
    url https://relay.example.com/v1/chat/completions
    model gpt-5
    account default
}
`

// runCLI 跑一次完整命令行调用，返回输出与退出码。
func runCLI(t *testing.T, stdin string, args ...string) (string, string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := executeWith(args, strings.NewReader(stdin), &stdout, &stderr)
	return stdout.String(), stderr.String(), code
}

// setupCLI 建一份临时配置目录，返回配置目录与配置文件路径。
func setupCLI(t *testing.T, novafile string) (configDir, configPath string) {
	t.Helper()
	configDir = t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configDir)
	configPath = filepath.Join(configDir, "nova", "Novafile")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatalf("创建配置目录失败：%v", err)
	}
	if err := os.WriteFile(configPath, []byte(novafile), 0o600); err != nil {
		t.Fatalf("写入配置失败：%v", err)
	}
	return configDir, configPath
}

// loadStore 读回凭据库，供断言副作用。
func loadStore(t *testing.T, configDir string) *keystore.Store {
	t.Helper()
	store, err := keystore.Load(configDir)
	if err != nil {
		t.Fatalf("读取凭据库失败：%v", err)
	}
	return store
}

// 参数形式：从标准输入取密钥并存进给定 provider 名下。
func TestLoginStoresKeyFromStdin(t *testing.T) {
	configDir, configPath := setupCLI(t, loginNovafile)

	stdout, stderr, code := runCLI(t, "sk-from-stdin\n",
		"login", "-c", configPath, "commandcode", "--key-stdin")
	if code != exitOK {
		t.Fatalf("退出码 = %d，期望 0\nstdout:%s\nstderr:%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "commandcode") {
		t.Errorf("stdout = %q，期望说明保存到哪个 provider", stdout)
	}

	key, ok := loadStore(t, configDir).Lookup("commandcode", "default")
	if !ok || key != "sk-from-stdin" {
		t.Errorf("凭据库里 commandcode/default = %q, %v，期望 sk-from-stdin", key, ok)
	}
}

// 参数形式可以指定账号名，且不读配置：先登录后写配置是合法顺序。
func TestLoginAcceptsAccountNameWithoutConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	_, stderr, code := runCLI(t, "sk-other\n",
		"login", "not-yet-configured", "work", "--key-stdin")
	if code != exitOK {
		t.Fatalf("退出码 = %d，期望 0\nstderr:%s", code, stderr)
	}

	key, ok := loadStore(t, os.Getenv("XDG_CONFIG_HOME")).Lookup("not-yet-configured", "work")
	if !ok || key != "sk-other" {
		t.Errorf("找不到 not-yet-configured/work：%q, %v", key, ok)
	}
}

// 交互形式：省略 provider 时列出配置里的渠道，按序号选择。
func TestLoginInteractiveByIndex(t *testing.T) {
	configDir, configPath := setupCLI(t, loginNovafile)
	t.Setenv("NOVA_TEST_KEY", "sk-interactive")

	stdout, stderr, code := runCLI(t, "2\n\n",
		"login", "-c", configPath, "--key-env", "NOVA_TEST_KEY")
	if code != exitOK {
		t.Fatalf("退出码 = %d，期望 0\nstdout:%s\nstderr:%s", code, stdout, stderr)
	}
	for _, want := range []string{"请选择供应商", "commandcode (api.commandcode.ai)", "relay (relay.example.com)"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout = %q，期望含 %q", stdout, want)
		}
	}
	if _, ok := loadStore(t, configDir).Lookup("relay", "default"); !ok {
		t.Error("序号 2 应存到 relay")
	}
}

// 交互选择与 --key-stdin 连着用：选择那一行之后紧跟密钥，不被预读吞掉。
func TestLoginInteractiveThenKeyStdin(t *testing.T) {
	configDir, configPath := setupCLI(t, loginNovafile)

	_, stderr, code := runCLI(t, "commandcode\n\nsk-after-choice\n",
		"login", "-c", configPath, "--key-stdin")
	if code != exitOK {
		t.Fatalf("退出码 = %d，期望 0\nstderr:%s", code, stderr)
	}
	key, ok := loadStore(t, configDir).Lookup("commandcode", "default")
	if !ok || key != "sk-after-choice" {
		t.Errorf("密钥 = %q, %v，期望 sk-after-choice", key, ok)
	}
}

// 交互形式也接受直接写名字。
func TestLoginInteractiveByName(t *testing.T) {
	configDir, configPath := setupCLI(t, loginNovafile)
	t.Setenv("NOVA_TEST_KEY", "sk-by-name")

	_, stderr, code := runCLI(t, "commandcode\n\n",
		"login", "-c", configPath, "--key-env", "NOVA_TEST_KEY")
	if code != exitOK {
		t.Fatalf("退出码 = %d，期望 0\nstderr:%s", code, stderr)
	}
	if _, ok := loadStore(t, configDir).Lookup("commandcode", "default"); !ok {
		t.Error("按名字应存到 commandcode")
	}
}

// 已有 default 时，只给 provider 的登录被拒绝，而不是覆盖。
func TestLoginRefusesDefaultOverwriteWithoutName(t *testing.T) {
	configDir, configPath := setupCLI(t, loginNovafile)
	if _, _, code := runCLI(t, "sk-first\n",
		"login", "-c", configPath, "commandcode", "--key-stdin"); code != exitOK {
		t.Fatalf("准备登录失败")
	}

	t.Setenv("NOVA_TEST_KEY", "sk-second")
	_, stderr, code := runCLI(t, "",
		"login", "-c", configPath, "commandcode", "--key-env", "NOVA_TEST_KEY")
	if code != exitError {
		t.Fatalf("退出码 = %d，期望 1\nstderr:%s", code, stderr)
	}
	if !strings.Contains(stderr, "已有 default") {
		t.Errorf("stderr = %q，期望说明 default 已存在", stderr)
	}
	if key, _ := loadStore(t, configDir).Lookup("commandcode", "default"); key != "sk-first" {
		t.Errorf("原密钥 = %q，期望未被覆盖", key)
	}
}

// 交互式登录在 default 已存在时，回车不接受，需要显式给名字。
func TestLoginInteractiveRefusesEmptyNameWhenDefaultExists(t *testing.T) {
	configDir, configPath := setupCLI(t, loginNovafile)
	if _, _, code := runCLI(t, "sk-first\n",
		"login", "-c", configPath, "commandcode", "--key-stdin"); code != exitOK {
		t.Fatalf("准备登录失败")
	}

	t.Setenv("NOVA_TEST_KEY", "sk-second")
	// stdin：选渠道 1、账号名直接回车。
	_, stderr, code := runCLI(t, "1\n\n",
		"login", "-c", configPath, "--key-env", "NOVA_TEST_KEY")
	if code != exitError {
		t.Fatalf("退出码 = %d，期望 1\nstderr:%s", code, stderr)
	}
	if key, _ := loadStore(t, configDir).Lookup("commandcode", "default"); key != "sk-first" {
		t.Errorf("原密钥 = %q，期望未被覆盖", key)
	}
}

// 交互式登录在 default 已存在时，给一个新名字就存到新名字下。
func TestLoginInteractiveAcceptsNewAccountName(t *testing.T) {
	configDir, configPath := setupCLI(t, loginNovafile)
	if _, _, code := runCLI(t, "sk-first\n",
		"login", "-c", configPath, "commandcode", "--key-stdin"); code != exitOK {
		t.Fatalf("准备登录失败")
	}

	t.Setenv("NOVA_TEST_KEY", "sk-second")
	// stdin：选渠道 1、账号名 work。
	stdout, stderr, code := runCLI(t, "1\nwork\n",
		"login", "-c", configPath, "--key-env", "NOVA_TEST_KEY")
	if code != exitOK {
		t.Fatalf("退出码 = %d，期望 0\nstdout:%s\nstderr:%s", code, stdout, stderr)
	}
	store := loadStore(t, configDir)
	if _, ok := store.Lookup("commandcode", "work"); !ok {
		t.Error("新名字 work 应已存入")
	}
	if key, _ := store.Lookup("commandcode", "default"); key != "sk-first" {
		t.Errorf("default = %q，期望未被覆盖", key)
	}
}

// 交互式登录里显式输入 default 就是声明要覆盖它。
func TestLoginInteractiveExplicitDefaultOverwrites(t *testing.T) {
	configDir, configPath := setupCLI(t, loginNovafile)
	if _, _, code := runCLI(t, "sk-first\n",
		"login", "-c", configPath, "commandcode", "--key-stdin"); code != exitOK {
		t.Fatalf("准备登录失败")
	}

	t.Setenv("NOVA_TEST_KEY", "sk-second")
	_, stderr, code := runCLI(t, "1\ndefault\n",
		"login", "-c", configPath, "--key-env", "NOVA_TEST_KEY")
	if code != exitOK {
		t.Fatalf("退出码 = %d，期望 0\nstderr:%s", code, stderr)
	}
	if key, _ := loadStore(t, configDir).Lookup("commandcode", "default"); key != "sk-second" {
		t.Errorf("密钥 = %q，期望被显式覆盖为 sk-second", key)
	}
}

// 配置里的 {env.NAME} 未导出时，nova login 仍能列出渠道并完成登录。
func TestLoginToleratesUnsetEnv(t *testing.T) {
	configDir, configPath := setupCLI(t, `
version 1
client_key {env.NOVA_TEST_UNSET_9F3A2B}
provider commandcode {
    url https://api.commandcode.ai/v1/chat/completions
    model gpt-5
    account default
}
`)
	t.Setenv("NOVA_TEST_KEY", "sk-env")
	_, stderr, code := runCLI(t, "1\n\n", "login", "-c", configPath, "--key-env", "NOVA_TEST_KEY")
	if code != exitOK {
		t.Fatalf("退出码 = %d，期望 0\nstderr:%s", code, stderr)
	}
	if _, ok := loadStore(t, configDir).Lookup("commandcode", "default"); !ok {
		t.Error("应完成登录")
	}
}

// 序号越界时报错，不静默取第一个。
func TestLoginInteractiveRejectsBadIndex(t *testing.T) {
	_, configPath := setupCLI(t, loginNovafile)

	_, stderr, code := runCLI(t, "9\n", "login", "-c", configPath, "--key-stdin")
	if code != exitError {
		t.Fatalf("退出码 = %d，期望 1\nstderr:%s", code, stderr)
	}
	if !strings.Contains(stderr, "超出范围") {
		t.Errorf("stderr = %q，期望说明序号越界", stderr)
	}
}

// 没有输入时报错，而不是把空选择当成第一个。
func TestLoginInteractiveWithoutInput(t *testing.T) {
	_, configPath := setupCLI(t, loginNovafile)

	_, stderr, code := runCLI(t, "", "login", "-c", configPath, "--key-stdin")
	if code != exitError {
		t.Fatalf("退出码 = %d，期望 1\nstderr:%s", code, stderr)
	}
}

// 配置里一条渠道都没有时，交互形式给出明确提示。
func TestLoginInteractiveWithoutProviders(t *testing.T) {
	_, configPath := setupCLI(t, "version 1\n")

	_, stderr, code := runCLI(t, "1\n", "login", "-c", configPath, "--key-stdin")
	if code != exitError {
		t.Fatalf("退出码 = %d，期望 1\nstderr:%s", code, stderr)
	}
	if !strings.Contains(stderr, "没有任何 provider") {
		t.Errorf("stderr = %q，期望说明配置里没有渠道", stderr)
	}
}

// 两条取密钥的来源标志互斥。
func TestLoginRejectsTwoKeySources(t *testing.T) {
	_, _, code := runCLI(t, "", "login", "p", "--key-stdin", "--key-env", "X")
	if code != exitError {
		t.Fatalf("退出码 = %d，期望 1", code)
	}
}

// 参数形式给的账号名也要过校验，不能把任意文字写进凭据库。
func TestLoginRejectsInvalidAccountName(t *testing.T) {
	configDir, configPath := setupCLI(t, loginNovafile)

	_, stderr, code := runCLI(t, "sk-1\n",
		"login", "-c", configPath, "commandcode", "bad name", "--key-stdin")
	if code != exitError {
		t.Fatalf("退出码 = %d，期望 1\nstderr:%s", code, stderr)
	}
	if !strings.Contains(stderr, "不允许的字符") {
		t.Errorf("stderr = %q，期望指出非法字符", stderr)
	}
	if store := loadStore(t, configDir); len(store.Namespaces()) != 0 {
		t.Errorf("非法名字不该落盘：%v", store.Namespaces())
	}
}

// 位置参数太多按用法错误处理（退出码 2）。
func TestLoginRejectsExtraArgs(t *testing.T) {
	_, _, code := runCLI(t, "", "login", "a", "b", "c")
	if code != exitUsage {
		t.Fatalf("退出码 = %d，期望 2", code)
	}
}

// account 列出凭据库的账号，缺省只给尾四位，--show 才给完整密钥。
func TestAccountListsStoredKeys(t *testing.T) {
	const (
		longKey = "sk-1234567890abcdefghijklmnopqrstuvwxyz"
		masked  = "sk-123456789****stuvwxyz"
	)
	configDir, configPath := setupCLI(t, loginNovafile)
	if _, _, code := runCLI(t, longKey+"\n",
		"login", "-c", configPath, "commandcode", "--key-stdin"); code != exitOK {
		t.Fatalf("准备登录失败，退出码 %d", code)
	}

	stdout, stderr, code := runCLI(t, "", "account")
	if code != exitOK {
		t.Fatalf("退出码 = %d，期望 0\nstderr:%s", code, stderr)
	}
	if !strings.Contains(stdout, "commandcode") || !strings.Contains(stdout, masked) {
		t.Errorf("stdout = %q，期望含 provider 与脱敏后的密钥 %q", stdout, masked)
	}
	if strings.Contains(stdout, longKey) {
		t.Errorf("缺省不该回显完整密钥：%q", stdout)
	}

	shown, _, code := runCLI(t, "", "account", "--show")
	if code != exitOK {
		t.Fatalf("退出码 = %d，期望 0", code)
	}
	if !strings.Contains(shown, longKey) {
		t.Errorf("--show 应打印完整密钥：%q", shown)
	}

	if store := loadStore(t, configDir); store.Path() == "" {
		t.Error("凭据库应有落点")
	}
}

// 凭据缺失时 nova models 仍给出报告，并把「尚未登录」作为提醒打在 stderr。
func TestModelsWorksWithoutCredentials(t *testing.T) {
	_, configPath := setupCLI(t, loginNovafile)

	stdout, stderr, code := runCLI(t, "", "models", "-c", configPath, "--provider", "commandcode")
	if code != exitOK {
		t.Fatalf("退出码 = %d，期望 0\nstdout:%s\nstderr:%s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "尚未登录") {
		t.Errorf("stderr = %q，期望含「尚未登录」提醒", stderr)
	}
	if !strings.Contains(stdout, "gpt-5") {
		t.Errorf("stdout = %q，期望列出显式声明的模型", stdout)
	}
}

// 取值函数每次都重新读凭据库：nova login 之后的重载要能看到新账号。
func TestCredentialLookupSeesLaterLogin(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	lookup, err := credentialLookup()
	if err != nil {
		t.Fatalf("构造取值函数失败：%v", err)
	}
	if _, ok := lookup("p", "a"); ok {
		t.Fatal("空凭据库里不该查到账号")
	}

	store, err := keystore.Load(dir)
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	if err := store.Set("p", "a", "k-after"); err != nil {
		t.Fatalf("写入失败：%v", err)
	}

	key, ok := lookup("p", "a")
	if !ok || key != "k-after" {
		t.Errorf("取值 = %q, %v，期望看到后写入的账号", key, ok)
	}
}

// 凭据库为空时 account 给出下一步提示。
func TestAccountEmptyStore(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	stdout, _, code := runCLI(t, "", "account")
	if code != exitOK {
		t.Fatalf("退出码 = %d，期望 0", code)
	}
	if !strings.Contains(stdout, "没有任何账号") && !strings.Contains(stdout, "没有账号") {
		t.Errorf("stdout = %q，期望提示库为空", stdout)
	}
}

// logout 参数形式删除指定账号。
func TestLogoutRemovesNamedAccount(t *testing.T) {
	configDir, configPath := setupCLI(t, loginNovafile)
	if _, _, code := runCLI(t, "sk-1\n",
		"login", "-c", configPath, "commandcode", "--key-stdin"); code != exitOK {
		t.Fatalf("准备登录失败，退出码 %d", code)
	}

	stdout, stderr, code := runCLI(t, "", "logout", "commandcode")
	if code != exitOK {
		t.Fatalf("退出码 = %d，期望 0\nstdout:%s\nstderr:%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "已删除") {
		t.Errorf("stdout = %q，期望说明删除结果", stdout)
	}
	if _, ok := loadStore(t, configDir).Lookup("commandcode", "default"); ok {
		t.Error("账号应已删除")
	}
}

// logout 交互形式列出已有账号供选择。
func TestLogoutInteractive(t *testing.T) {
	configDir, configPath := setupCLI(t, loginNovafile)
	for _, target := range []string{"commandcode", "relay"} {
		if _, _, code := runCLI(t, "sk-"+target+"\n",
			"login", "-c", configPath, target, "--key-stdin"); code != exitOK {
			t.Fatalf("准备登录 %s 失败", target)
		}
	}

	stdout, stderr, code := runCLI(t, "2\n", "logout")
	if code != exitOK {
		t.Fatalf("退出码 = %d，期望 0\nstdout:%s\nstderr:%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "请选择要删除的账号") {
		t.Errorf("stdout = %q，期望列出候选", stdout)
	}

	store := loadStore(t, configDir)
	if _, ok := store.Lookup("relay", "default"); ok {
		t.Error("序号 2 对应的 relay 应已删除")
	}
	if _, ok := store.Lookup("commandcode", "default"); !ok {
		t.Error("未选中的 commandcode 应保留")
	}
}

// logout 引用不存在的账号时报错，并列出可选项。
func TestLogoutUnknownAccount(t *testing.T) {
	_, configPath := setupCLI(t, loginNovafile)
	if _, _, code := runCLI(t, "sk-1\n",
		"login", "-c", configPath, "commandcode", "work", "--key-stdin"); code != exitOK {
		t.Fatalf("准备登录失败")
	}

	_, stderr, code := runCLI(t, "", "logout", "commandcode", "missing")
	if code != exitError {
		t.Fatalf("退出码 = %d，期望 1", code)
	}
	if !strings.Contains(stderr, "work") {
		t.Errorf("stderr = %q，期望列出已有账号 work", stderr)
	}
}

// 凭据库为空时交互 logout 给出提示而不是空列表。
func TestLogoutInteractiveEmptyStore(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	_, stderr, code := runCLI(t, "", "logout")
	if code != exitError {
		t.Fatalf("退出码 = %d，期望 1", code)
	}
	if !strings.Contains(stderr, "没有账号") {
		t.Errorf("stderr = %q，期望提示库为空", stderr)
	}
}
