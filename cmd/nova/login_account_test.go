package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/sumwai/nova/internal/keystore"
)

// promptTestCmd 造一个只用于读取输入的命令对象：输出与输入都落在内存里。
func promptTestCmd(stdin string) (*cobra.Command, *bytes.Buffer) {
	out := &bytes.Buffer{}
	cmd := &cobra.Command{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetIn(strings.NewReader(stdin))
	return cmd, out
}

// storeWith 造一个带落点的凭据库，写入给定的账号。
func storeWith(t *testing.T, accounts map[string]map[string]string) *keystore.Store {
	t.Helper()
	store, err := keystore.Load(t.TempDir())
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	for namespace, entries := range accounts {
		for name, key := range entries {
			if err := store.Set(namespace, name, key); err != nil {
				t.Fatalf("写入失败：%v", err)
			}
		}
	}
	return store
}

// 账号名只收字母、数字、下划线、连字符，且有长度上限。
func TestValidateAccountName(t *testing.T) {
	for _, name := range []string{"default", "work", "work-2", "a_b", "A1"} {
		if err := validateAccountName(name); err != nil {
			t.Errorf("validateAccountName(%q) = %v，期望通过", name, err)
		}
	}

	invalid := []struct {
		name string
		want string
	}{
		{"", "不能为空"},
		{"has space", "不允许的字符"},
		{"dot.name", "不允许的字符"},
		{"斜杠/name", "不允许的字符"},
		{strings.Repeat("a", accountNameMaxLen+1), "过长"},
	}
	for _, tt := range invalid {
		err := validateAccountName(tt.name)
		if err == nil {
			t.Errorf("validateAccountName(%q) 期望报错", tt.name)
			continue
		}
		if !strings.Contains(err.Error(), tt.want) {
			t.Errorf("validateAccountName(%q) = %v，期望含 %q", tt.name, err, tt.want)
		}
	}
}

// 账号候选列表列出已有账号（带尾四位），最后一项是「添加新账号」。
func TestAccountChoicesListsEntriesAndNew(t *testing.T) {
	store := storeWith(t, map[string]map[string]string{
		"p": {"default": "sk-1234567890abcdefghijklmnopqrstuvwxyz"},
	})
	choices := accountChoices(store.Entries("p"))
	if len(choices) != 2 {
		t.Fatalf("候选数 = %d，期望 2", len(choices))
	}
	if !strings.Contains(choices[0].label, "default") ||
		!strings.Contains(choices[0].label, "sk-123456789****stuvwxyz") {
		t.Errorf("第一个候选 = %q，期望含名字与脱敏后的密钥", choices[0].label)
	}
	if choices[1].value != newAccountMarker {
		t.Errorf("最后一项 = %q，期望是添加新账号", choices[1].value)
	}

	empty := accountChoices(nil)
	if len(empty) != 1 || empty[0].value != newAccountMarker {
		t.Errorf("空列表的候选 = %+v，期望只有添加新账号", empty)
	}
}

// 新增账号路径：default 可用时回车即用它，已存在时要求不重名。
func TestPromptNewAccountName(t *testing.T) {
	t.Run("没有 default 时空回车用它", func(t *testing.T) {
		cmd, out := promptTestCmd("\n")
		name, err := promptNewAccountName(cmd, storeWith(t, nil), "p")
		if err != nil || name != keystore.DefaultAccount {
			t.Fatalf("名字 = %q, %v，期望 default", name, err)
		}
		if !strings.Contains(out.String(), "账号名 [default]") {
			t.Errorf("提示 = %q，期望带缺省值", out.String())
		}
	})

	t.Run("已有 default 时空回车被拒", func(t *testing.T) {
		store := storeWith(t, map[string]map[string]string{"p": {"default": "k"}})
		cmd, _ := promptTestCmd("\n")
		if _, err := promptNewAccountName(cmd, store, "p"); err == nil {
			t.Fatal("应要求另给一个名字")
		}
	})

	t.Run("已有账号重名被拒", func(t *testing.T) {
		store := storeWith(t, map[string]map[string]string{"p": {"work": "k"}})
		cmd, _ := promptTestCmd("work\n")
		if _, err := promptNewAccountName(cmd, store, "p"); err == nil {
			t.Fatal("重名应被拒")
		}
	})

	t.Run("新名字通过", func(t *testing.T) {
		store := storeWith(t, map[string]map[string]string{"p": {"default": "k"}})
		cmd, _ := promptTestCmd("work-2\n")
		name, err := promptNewAccountName(cmd, store, "p")
		if err != nil || name != "work-2" {
			t.Fatalf("名字 = %q, %v，期望 work-2", name, err)
		}
	})

	t.Run("非法名字被拒", func(t *testing.T) {
		cmd, _ := promptTestCmd("bad name\n")
		if _, err := promptNewAccountName(cmd, storeWith(t, nil), "p"); err == nil {
			t.Fatal("含空白的名字应被拒")
		}
	})
}

// 账号选择：选已有账号就是去更新它，选最后一项才问新名字。
func TestPromptAccountInteractive(t *testing.T) {
	t.Run("选已有账号", func(t *testing.T) {
		store := storeWith(t, map[string]map[string]string{"p": {"default": "k1"}})
		cmd, out := promptTestCmd("1\n")
		name, err := promptAccountInteractive(cmd, store, "p")
		if err != nil || name != "default" {
			t.Fatalf("名字 = %q, %v，期望 default", name, err)
		}
		if !strings.Contains(out.String(), "渠道 p 的账号") {
			t.Errorf("输出 = %q，期望列出账号", out.String())
		}
	})

	t.Run("选添加新账号", func(t *testing.T) {
		store := storeWith(t, map[string]map[string]string{"p": {"default": "k1"}})
		cmd, _ := promptTestCmd("2\nwork\n")
		name, err := promptAccountInteractive(cmd, store, "p")
		if err != nil || name != "work" {
			t.Fatalf("名字 = %q, %v，期望 work", name, err)
		}
	})

	t.Run("没有账号时直接问名字", func(t *testing.T) {
		cmd, out := promptTestCmd("\n")
		name, err := promptAccountInteractive(cmd, storeWith(t, nil), "q")
		if err != nil || name != keystore.DefaultAccount {
			t.Fatalf("名字 = %q, %v，期望 default", name, err)
		}
		if strings.Contains(out.String(), "添加新账号") {
			t.Errorf("没有账号时不该出现选择列表：%q", out.String())
		}
	})
}
