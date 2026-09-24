package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeArgs(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"无参数", nil, []string{}},
		{"-? 归一成 --help", []string{"-?"}, []string{"--help"}},
		{"子命令之后的 -? 同样归一", []string{"run", "-?"}, []string{"run", "--help"}},
		{"常规参数原样保留", []string{"run", "-c", "Novafile"}, []string{"run", "-c", "Novafile"}},
		// `--` 之后是位置参数：那里的 -? 是一个真实取值，
		// 归一它会让一个本来能用的调用突然变成帮助请求。
		{"-- 之后的 -? 不改写", []string{"run", "--", "-?"}, []string{"run", "--", "-?"}},
		{"-- 之后的其它参数也原样", []string{"--", "a", "b"}, []string{"--", "a", "b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeArgs(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("normalizeArgs(%v) = %v，期望 %v", tt.in, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("normalizeArgs(%v)[%d] = %q，期望 %q", tt.in, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestExecuteExitCodes(t *testing.T) {
	missingConfig := filepath.Join(t.TempDir(), "并不存在的配置")

	tests := []struct {
		name string
		args []string
		want int
	}{
		{"无参数时打帮助", nil, exitOK},
		{"version 子命令", []string{"version"}, exitOK},
		{"--version", []string{"--version"}, exitOK},
		{"-v", []string{"-v"}, exitOK},
		{"--help", []string{"--help"}, exitOK},
		{"-h", []string{"-h"}, exitOK},
		{"-?", []string{"-?"}, exitOK},
		{"help 子命令", []string{"help"}, exitOK},
		{"run --help 不启动服务", []string{"run", "--help"}, exitOK},
		{"config 不带子命令时打帮助", []string{"config"}, exitOK},

		// 用法错误取 2：脚本据此知道该改命令行，而不是去重试。
		{"未知子命令", []string{"nope"}, exitUsage},
		{"未知标志", []string{"--nope"}, exitUsage},
		{"叶子命令多给参数", []string{"version", "extra"}, exitUsage},
		{"父命令给错子命令名", []string{"config", "nope"}, exitUsage},
		{"标志写错", []string{"run", "--cnofig", "x"}, exitUsage},

		// 运行期失败取 1：命令行本身没问题，是环境或配置的问题。
		{"run 读不到配置", []string{"run", "-c", missingConfig}, exitError},
		{"config check 读不到配置", []string{"config", "check", "-c", missingConfig}, exitError},
		{"reload 读不到配置", []string{"reload", "-c", missingConfig}, exitError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			if got := execute(tt.args, &stdout, &stderr); got != tt.want {
				t.Errorf("execute(%v) = %d，期望 %d\n--- stdout ---\n%s--- stderr ---\n%s",
					tt.args, got, tt.want, stdout.String(), stderr.String())
			}
		})
	}
}

// 结果进 stdout、诊断进 stderr：混在一条流上，
// `nova version --json | jq` 之类的管道就会被日志行打断。
func TestExecuteKeepsStreamsSeparate(t *testing.T) {
	var stdout, stderr bytes.Buffer

	if got := execute([]string{"version"}, &stdout, &stderr); got != exitOK {
		t.Fatalf("退出码 = %d，期望 %d", got, exitOK)
	}
	if !strings.Contains(stdout.String(), "nova ") {
		t.Errorf("stdout = %q，期望含版本首行", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q，期望干净", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()

	if got := execute([]string{"--nope"}, &stdout, &stderr); got != exitUsage {
		t.Fatalf("退出码 = %d，期望 %d", got, exitUsage)
	}
	if stderr.Len() == 0 {
		t.Error("用法错误没有写进 stderr")
	}
}

// 用法错误要连用法一起给出：只说「未知标志」而不说有哪些标志，
// 使用者还得再敲一次 --help。
func TestUsageErrorsPrintUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer

	if got := execute([]string{"--nope"}, &stdout, &stderr); got != exitUsage {
		t.Fatalf("退出码 = %d，期望 %d", got, exitUsage)
	}
	if !strings.Contains(stderr.String(), "Usage") && !strings.Contains(stderr.String(), "用法") {
		t.Errorf("stderr = %q，期望含用法说明", stderr.String())
	}
}
