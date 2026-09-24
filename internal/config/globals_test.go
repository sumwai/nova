package config

import (
	"errors"
	"strings"
	"testing"
)

// configError 把一次解析失败的结果取成 *Error，便于断言位置。
func configError(t *testing.T, err error) *Error {
	t.Helper()
	if err == nil {
		t.Fatal("期望报错，但它成功了")
	}
	var cerr *Error
	if !errors.As(err, &cerr) {
		t.Fatalf("错误类型是 %T，期望 *Error", err)
	}
	return cerr
}

func parseWithEnv(t *testing.T, src string, env map[string]string) (*Config, error) {
	t.Helper()
	return ParseWith([]byte(src), "Novafile", Options{
		Getenv: func(name string) string { return env[name] },
	})
}

func mustParseWithEnv(t *testing.T, src string, env map[string]string) *Config {
	t.Helper()
	cfg, err := parseWithEnv(t, src, env)
	if err != nil {
		t.Fatalf("Parse 意外失败：%v", err)
	}
	return cfg
}

// 日志格式与日志级别一样是配置指令，缺省 text。
func TestParseLogFormat(t *testing.T) {
	if got := mustParse(t, "version 1\n").LogFormat; got != "text" {
		t.Errorf("缺省日志格式 = %q，期望 text", got)
	}
	if got := mustParse(t, "version 1\nlog_format json\n").LogFormat; got != "json" {
		t.Errorf("显式指定的日志格式 = %q，期望 json", got)
	}
}

// 取值不认识时报错并报出位置，而不是静默退回缺省：
// 少打一个字母就发现日志“没变化”，比当场报错难查得多。
func TestParseRejectsUnknownLogFormat(t *testing.T) {
	err := parseErr(t, "version 1\nlog_format xml\n")

	if !strings.Contains(err.Msg, "text / json") {
		t.Errorf("消息 = %q，期望列出可取的值", err.Msg)
	}
	if err.Line != 2 {
		t.Errorf("错误行号 = %d，期望 2", err.Line)
	}
}

// client_key 可写多条：一份配置里给不同调用方各发一个 key 是正常用法。
func TestParseClientKeysAcceptMultiple(t *testing.T) {
	cfg := mustParse(t, `
version 1
client_key first-key
client_key second-key
`)

	if len(cfg.ClientKeys) != 2 {
		t.Fatalf("凭据数 = %d，期望 2（client_key 可以写多条）", len(cfg.ClientKeys))
	}
	if cfg.ClientKeys[0] != "first-key" || cfg.ClientKeys[1] != "second-key" {
		t.Errorf("凭据 = %v，期望按声明顺序保留", cfg.ClientKeys)
	}
}

// 其余顶层指令在同一份配置里只能出现一次。
func TestParseRejectsRepeatedGlobalDirective(t *testing.T) {
	err := parseErr(t, "version 1\nlog_level info\nlog_level debug\n")

	if !strings.Contains(err.Msg, "不能写两次") {
		t.Errorf("消息 = %q，期望提到重复声明", err.Msg)
	}
	if err.Line != 3 {
		t.Errorf("行号 = %d，期望 3（指向后一次）", err.Line)
	}
}

func TestPlaceholderExpandsFromEnvironment(t *testing.T) {
	cfg := mustParseWithEnv(t, `
version 1
provider p {
    api_key {env.TEST_KEY}
    url https://a.example.com/v1/chat/completions
    model m
}
`, map[string]string{"TEST_KEY": "sk-from-env"})

	if got := cfg.Providers[0].APIKey; got != "sk-from-env" {
		t.Errorf("凭据 = %q，期望从环境变量展开", got)
	}
}

// 占位符也用在顶层指令的取值上。
func TestPlaceholderExpandsInGlobalValue(t *testing.T) {
	cfg := mustParseWithEnv(t, "version 1\nclient_key {env.K}\n", map[string]string{"K": "abc"})

	if len(cfg.ClientKeys) != 1 || cfg.ClientKeys[0] != "abc" {
		t.Errorf("凭据 = %v，期望展开成 abc", cfg.ClientKeys)
	}
}

// 未设置与「已设置但为空」在 Go 里无法区分，两者都按「没有可用取值」报错：
// 把空凭据放行只会让上游回一个 401，而那时看不出问题出在环境变量没导出上。
func TestPlaceholderUnsetIsRejected(t *testing.T) {
	for _, tt := range []struct {
		name string
		env  map[string]string
	}{
		{"变量完全没设置", map[string]string{}},
		{"变量设为空串", map[string]string{"TEST_KEY": ""}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseWithEnv(t, `
version 1
provider p {
    api_key {env.TEST_KEY}
    url https://a.example.com/v1/chat/completions
    model m
}
`, tt.env)
			cerr := configError(t, err)
			if !strings.Contains(cerr.Msg, "未设置或为空") {
				t.Errorf("消息 = %q，期望说明变量没有可用取值", cerr.Msg)
			}
			if cerr.Line != 4 {
				t.Errorf("行号 = %d，期望 4（指向占位符那一行）", cerr.Line)
			}
		})
	}
}

// 命名空间写错时不会被当成「变量未设置」而放行，也不会被静默展开成空值：
// 判据只认 env. 前缀，因此 {secret.K} 被当成块开启，报的是这一行形态不对。
func TestPlaceholderRejectsUnknownNamespace(t *testing.T) {
	_, err := parseWithEnv(t, "version 1\nclient_key {secret.K}\n", map[string]string{"K": "abc"})
	cerr := configError(t, err)
	if cerr.Line != 2 {
		t.Errorf("行号 = %d，期望 2", cerr.Line)
	}
}

func TestPlaceholderRequiresClosingBrace(t *testing.T) {
	_, err := parseWithEnv(t, "version 1\nclient_key {env.K\n", map[string]string{"K": "abc"})

	if err == nil {
		t.Fatal("期望报错，但加载成功了")
	}
	if !strings.Contains(err.Error(), "缺少收尾的 }") {
		t.Errorf("消息 = %q，期望说明占位符没有收尾", err.Error())
	}
}

func TestClientKeyRejectsBlank(t *testing.T) {
	for _, tt := range []struct {
		name string
		src  string
	}{
		{"含空白的取值", "version 1\nclient_key \"a b\"\n"},
		{"含制表符的取值", "version 1\nclient_key \"a\tb\"\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseWithEnv(t, tt.src, nil)
			if err == nil {
				t.Fatal("期望报错，但加载成功了")
			}
			if !strings.Contains(err.Error(), "不能为空、也不能含空白") {
				t.Errorf("消息 = %q，期望说明取值形态不对", err.Error())
			}
		})
	}
}

// 绑到非回环地址又没有客户端凭据时给出警告：那是把上游凭据敞开的配置。
func TestNonLoopbackListenWithoutClientKeyWarns(t *testing.T) {
	cfg := mustParse(t, "version 1\nlisten :8080\n")

	if !hasWarningAbout(cfg, "client_key") {
		t.Errorf("缺少「有监听无鉴权」的提醒：%v", cfg.Warnings)
	}
}

// 只绑回环时不警告：那是本机自用的默认形态。
func TestLoopbackListenWithoutClientKeyDoesNotWarn(t *testing.T) {
	cfg := mustParse(t, "version 1\nlisten 127.0.0.1:8080\n")

	if hasWarningAbout(cfg, "client_key") {
		t.Errorf("绑回环时不该报鉴权警告：%v", cfg.Warnings)
	}
}

// 声明的代数、日志级别与监听地址都会被保留。
func TestParseKeepsGlobalValues(t *testing.T) {
	cfg := mustParse(t, `
version 1
log_level warn
listen 0.0.0.0:9000
admin 127.0.0.1:9001
client_key k
`)

	if cfg.LogLevel != "warn" {
		t.Errorf("日志级别 = %q", cfg.LogLevel)
	}
	if cfg.Listen != "0.0.0.0:9000" {
		t.Errorf("监听 = %q", cfg.Listen)
	}
	if cfg.Admin != "127.0.0.1:9001" {
		t.Errorf("管理端点 = %q", cfg.Admin)
	}
}
