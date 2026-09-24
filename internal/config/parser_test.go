package config

import (
	"errors"
	"strings"
	"testing"
)

func mustParse(t *testing.T, src string) *Config {
	t.Helper()
	cfg, err := Parse([]byte(src), "Novafile")
	if err != nil {
		t.Fatalf("Parse 意外失败：%v", err)
	}
	return cfg
}

func parseErr(t *testing.T, src string) *Error {
	t.Helper()
	_, err := Parse([]byte(src), "Novafile")
	if err == nil {
		t.Fatal("期望 Parse 报错，但它成功了")
	}
	var cerr *Error
	if !errors.As(err, &cerr) {
		t.Fatalf("错误类型是 %T，期望 *Error", err)
	}
	return cerr
}

// hasWarningAbout 报告提醒里有没有提到某个关键词。
//
// 用它而不是断言提醒条数：后续新增的提醒（没有 provider、绑了非回环却没鉴权）
// 与当前断言无关，把条数写死会让每加一条提醒都要来改一个不相干的测试。
func hasWarningAbout(cfg *Config, keyword string) bool {
	for _, warn := range cfg.Warnings {
		if strings.Contains(warn.Msg, keyword) {
			return true
		}
	}
	return false
}

func TestParseAppliesDefaultsWhenNothingIsWritten(t *testing.T) {
	cfg := mustParse(t, "")

	if cfg.LogLevel != defaultLogLevel {
		t.Errorf("日志级别 = %q，期望缺省 %q", cfg.LogLevel, defaultLogLevel)
	}
	if cfg.Listen != defaultListen {
		t.Errorf("监听地址 = %q，期望缺省 %q", cfg.Listen, defaultListen)
	}
	if cfg.Admin != defaultAdmin {
		t.Errorf("管理端点 = %q，期望缺省 %q", cfg.Admin, defaultAdmin)
	}
	if cfg.Schema != CurrentSchema {
		t.Errorf("配置代数 = %d，期望 %d", cfg.Schema, CurrentSchema)
	}
	if cfg.SchemaDeclared {
		t.Error("SchemaDeclared = true，但文件里没有 version")
	}
	// 未声明代数要有一条提醒：否则「这份配置按哪一代在解析」只能靠读源码回答。
	if !hasWarningAbout(cfg, "未声明 "+directiveVersion) {
		t.Errorf("缺少「未声明 %s」的提醒：%v", directiveVersion, cfg.Warnings)
	}
}

func TestParseAcceptsDeclaredSchemaWithoutWarning(t *testing.T) {
	cfg := mustParse(t, "version 1\nlog_level debug\n")

	if !cfg.SchemaDeclared {
		t.Error("SchemaDeclared = false，但文件里写了 version")
	}
	if cfg.Schema != 1 {
		t.Errorf("配置代数 = %d，期望 1", cfg.Schema)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("日志级别 = %q，期望 debug", cfg.LogLevel)
	}
	if hasWarningAbout(cfg, "未声明 "+directiveVersion) {
		t.Errorf("已经写了 version 却仍报「未声明」：%v", cfg.Warnings)
	}
}

// 这条是「配置语法代数」这个机制的核心断言：
// 代数超出支持区间时报出的必须是版本不匹配，而不是先撞上某条指令的语法错误。
// 若顺序反了，使用者会看到「未知指令 log_levle」而去翻拼写，永远想不到是版本问题。
func TestSchemaIsCheckedBeforeAnyDirective(t *testing.T) {
	err := parseErr(t, "version 2\nlog_levle info\n")

	if !strings.Contains(err.Msg, "配置语法代数 2 不被本二进制支持") {
		t.Fatalf("消息 = %q，期望先报版本不匹配", err.Msg)
	}
	if strings.Contains(err.Msg, "未知指令") {
		t.Errorf("消息 = %q，不该把版本问题报成指令问题", err.Msg)
	}
	// 位置指向 version 的取值，也就是该被改的那一处。
	if err.Line != 1 || err.Col != 9 {
		t.Errorf("位置 = %d:%d，期望 1:9（指向 version 的取值）", err.Line, err.Col)
	}
}

func TestParseRejectsBadSchemaDeclarations(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		wantMsg string
	}{
		{"代数偏高", "version 99\n", "不被本二进制支持"},
		{"代数不是整数", "version abc\n", "不是整数"},
		{"代数写两次", "version 1\nversion 1\n", "只能声明一次"},
		{"代数缺取值", "version\n", "缺少代数取值"},
		{"代数多取值", "version 1 2\n", "只接受一个代数取值"},
		{"代数零", "version 0\n", "不被本二进制支持"},
		{"代数负数", "version -1\n", "不被本二进制支持"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := parseErr(t, tt.src)
			if !strings.Contains(err.Msg, tt.wantMsg) {
				t.Errorf("消息 = %q，期望包含 %q", err.Msg, tt.wantMsg)
			}
		})
	}
}

func TestParseRejectsUnknownDirectiveWithSuggestion(t *testing.T) {
	tests := []struct {
		name      string
		src       string
		wantInMsg []string
	}{
		{
			name:      "拼写接近时给出候选",
			src:       "log_levle info\n",
			wantInMsg: []string{`未知指令 "log_levle"`, `"log_level"`},
		},
		{
			name:      "离得太远时不给候选",
			src:       "zzzzzzzzzz info\n",
			wantInMsg: []string{`未知指令 "zzzzzzzzzz"`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := parseErr(t, tt.src)
			for _, want := range tt.wantInMsg {
				if !strings.Contains(err.Msg, want) {
					t.Errorf("消息 = %q，期望包含 %q", err.Msg, want)
				}
			}
			if tt.name == "离得太远时不给候选" && strings.Contains(err.Msg, "最接近") {
				t.Errorf("消息 = %q，不该给一个离题的候选", err.Msg)
			}
		})
	}
}

func TestParseRejectsBadLogLevel(t *testing.T) {
	err := parseErr(t, "log_level verbose\n")

	if !strings.Contains(err.Msg, "日志级别") {
		t.Errorf("消息 = %q，期望提到日志级别", err.Msg)
	}
	if err.Line != 1 || err.Col != 11 {
		t.Errorf("位置 = %d:%d，期望 1:11（指向取值）", err.Line, err.Col)
	}
}

func TestParseValidatesAddresses(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		wantMsg string
	}{
		{"缺端口", "listen localhost\n", "不是 host:port 形态"},
		{"端口非数字", "listen :abc\n", "端口必须是 1 到 65535"},
		{"端口为零", "listen :0\n", "端口必须是 1 到 65535"},
		{"端口越界", "admin localhost:70000\n", "端口必须是 1 到 65535"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := parseErr(t, tt.src)
			if !strings.Contains(err.Msg, tt.wantMsg) {
				t.Errorf("消息 = %q，期望包含 %q", err.Msg, tt.wantMsg)
			}
		})
	}
}

func TestParseAcceptsAddressForms(t *testing.T) {
	tests := []struct {
		name string
		src  string
	}{
		{"仅端口", "listen :8080\n"},
		{"主机加端口", "listen 127.0.0.1:8080\n"},
		{"主机名", "listen localhost:8080\n"},
		{"IPv6", "listen [::1]:8080\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mustParse(t, tt.src)
		})
	}
}

func TestParseRejectsRepeatedDirective(t *testing.T) {
	err := parseErr(t, "log_level info\nlisten :8080\nlog_level debug\n")

	if !strings.Contains(err.Msg, "不能写两次") {
		t.Errorf("消息 = %q，期望提到重复声明", err.Msg)
	}
	// 指出前一次在哪一行，是这条报错唯一能帮人省下的时间。
	if !strings.Contains(err.Msg, "第 1 行") {
		t.Errorf("消息 = %q，期望指出前一次在第 1 行", err.Msg)
	}
	if err.Line != 3 {
		t.Errorf("位置行号 = %d，期望 3（指向后一次）", err.Line)
	}
}

func TestParseWarnsOnNonLoopbackAdmin(t *testing.T) {
	tests := []struct {
		name     string
		addr     string
		wantWarn bool
	}{
		{"回环主机名", "localhost:2026", false},
		{"回环 IP", "127.0.0.1:2026", false},
		{"回环 IPv6", "[::1]:2026", false},
		{"绑所有接口", ":2026", true},
		{"任意接口", "0.0.0.0:2026", true},
		{"内网地址", "10.0.0.5:2026", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := mustParse(t, "version 1\nadmin "+tt.addr+"\n")

			warned := false
			for _, warn := range cfg.Warnings {
				if strings.Contains(warn.Msg, "管理端点") {
					warned = true
				}
			}
			if warned != tt.wantWarn {
				t.Errorf("警告 = %v，期望 %v（地址 %s，全部提醒 %v）",
					warned, tt.wantWarn, tt.addr, cfg.Warnings)
			}
		})
	}
}

func TestParseUnquotesValues(t *testing.T) {
	cfg := mustParse(t, `version 1`+"\n"+`admin "localhost:2026"`+"\n")

	if cfg.Admin != "localhost:2026" {
		t.Errorf("管理端点 = %q，期望去掉引号后的取值", cfg.Admin)
	}
	if hasWarningAbout(cfg, "管理端点") {
		t.Errorf("带引号的回环地址仍应算回环，却报了警告：%v", cfg.Warnings)
	}
}

// 空文件不是错误：没有任何指令的配置落在全套缺省值上，是一份合法的配置。
func TestParseAcceptsEmptyInput(t *testing.T) {
	cfg := mustParse(t, "\n\n# 只有注释\n\n")

	if cfg.Listen != defaultListen || cfg.Admin != defaultAdmin {
		t.Errorf("缺省值未生效：listen=%q admin=%q", cfg.Listen, cfg.Admin)
	}
}

// 一行以花括号开头时，报错要说清「这里只能是指令名」，
// 而不是把它当成一个取值为 "{" 的怪指令。
func TestParseReportsStrayBrace(t *testing.T) {
	err := parseErr(t, "{\n")

	if !strings.Contains(err.Msg, "只能是一条指令名") {
		t.Errorf("消息 = %q，期望说明这一行只能是指令名", err.Msg)
	}
}
