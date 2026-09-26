package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sumwai/nova/internal/credentials"
)

// seedStore 往 XDG 配置目录下的凭据库写入两个账号，返回该配置目录。
func seedStore(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	store, err := credentials.Load(dir)
	if err != nil {
		t.Fatalf("加载凭据库失败：%v", err)
	}
	if err := store.Set("opencode-go", "default", "sk-1234abcd"); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	if err := store.Set("opencode-go", "work", "sk-5678efgh"); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	return dir
}

// list 缺省只给尾四位，--show 才打印完整密钥。
func TestAccountListHidesKeyByDefault(t *testing.T) {
	seedStore(t)

	var stdout, stderr bytes.Buffer
	if got := execute([]string{"account", "list"}, &stdout, &stderr); got != exitOK {
		t.Fatalf("退出码 = %d，期望 %d\n--- stderr ---\n%s", got, exitOK, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"opencode-go  default", "opencode-go  work", "…abcd", "…efgh"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout = %q，期望含 %q", out, want)
		}
	}
	if strings.Contains(out, "sk-1234abcd") {
		t.Errorf("stdout = %q，缺省不应回显完整密钥", out)
	}

	stdout.Reset()
	if got := execute([]string{"account", "list", "--show"}, &stdout, &stderr); got != exitOK {
		t.Fatalf("退出码 = %d，期望 %d\n--- stderr ---\n%s", got, exitOK, stderr.String())
	}
	if !strings.Contains(stdout.String(), "sk-1234abcd") {
		t.Errorf("stdout = %q，--show 应打印完整密钥", stdout.String())
	}
}

// 空凭据库给出可执行的下一步，而不是一行空白。
func TestAccountListEmpty(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	if got := execute([]string{"account", "list"}, &stdout, &stderr); got != exitOK {
		t.Fatalf("退出码 = %d，期望 %d\n--- stderr ---\n%s", got, exitOK, stderr.String())
	}
	if !strings.Contains(stdout.String(), "没有账号") {
		t.Errorf("stdout = %q，期望说明凭据库是空的", stdout.String())
	}
}

// remove 省略账号名时删除整个档案并报出删了几个；指定账号名时只删一个。
func TestAccountRemove(t *testing.T) {
	dir := seedStore(t)

	var stdout, stderr bytes.Buffer
	if got := execute([]string{"account", "remove", "opencode-go", "work"}, &stdout, &stderr); got != exitOK {
		t.Fatalf("退出码 = %d，期望 %d\n--- stderr ---\n%s", got, exitOK, stderr.String())
	}
	if !strings.Contains(stdout.String(), "已删除档案 opencode-go 的账号 work") {
		t.Errorf("stdout = %q，期望说明删了哪个账号", stdout.String())
	}
	reloaded, err := credentials.Load(dir)
	if err != nil {
		t.Fatalf("重新加载失败：%v", err)
	}
	if _, ok := reloaded.Lookup("opencode-go", "work"); ok {
		t.Error("账号 work 应已删除")
	}

	stdout.Reset()
	if got := execute([]string{"account", "remove", "opencode-go"}, &stdout, &stderr); got != exitOK {
		t.Fatalf("退出码 = %d，期望 %d\n--- stderr ---\n%s", got, exitOK, stderr.String())
	}
	if !strings.Contains(stdout.String(), "已删除档案 opencode-go 的 1 个账号") {
		t.Errorf("stdout = %q，期望报出删除个数", stdout.String())
	}
}

// 删除不存在的东西给出可读的错误，而不是静默成功。
func TestAccountRemoveMissing(t *testing.T) {
	seedStore(t)

	tests := []struct {
		name string
		args []string
		want string
	}{
		{"档案不存在", []string{"account", "remove", "nope"}, "没有档案 nope 的账号"},
		{"账号不存在", []string{"account", "remove", "opencode-go", "missing"}, "有的账号"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if got := execute(tt.args, &stdout, &stderr); got != exitError {
				t.Fatalf("退出码 = %d，期望 %d\n--- stderr ---\n%s", got, exitError, stderr.String())
			}
			if !strings.Contains(stderr.String(), tt.want) {
				t.Errorf("stderr = %q，期望含 %q", stderr.String(), tt.want)
			}
		})
	}
}
