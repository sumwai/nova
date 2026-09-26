package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sumwai/nova/internal/credentials"
)

// swapStdin 把进程标准输入换成一根写入了 input 的管道，读完即挂上清理。
//
// 无回显读入与 --key-stdin 都读标准输入；把它换成管道后，两条分支都能在测试里
// 被完整走到，不必真的去开一个伪终端。
func swapStdin(t *testing.T, input string) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("创建管道失败：%v", err)
	}
	if _, err := writer.WriteString(input); err != nil {
		t.Fatalf("写入管道失败：%v", err)
	}
	writer.Close()

	original := os.Stdin
	os.Stdin = reader
	t.Cleanup(func() {
		os.Stdin = original
		reader.Close()
	})
}

// --key-env 把密钥存进凭据库，且不在 stdout 回显密钥。
func TestLoginKeyEnvStoresKey(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("NOVA_TEST_KEY", "sk-abcdefgh")

	var stdout, stderr bytes.Buffer
	if got := execute([]string{"login", "opencode-go", "work", "--key-env", "NOVA_TEST_KEY"},
		&stdout, &stderr); got != exitOK {
		t.Fatalf("退出码 = %d，期望 %d\n--- stderr ---\n%s", got, exitOK, stderr.String())
	}

	store, err := credentials.Load(dir)
	if err != nil {
		t.Fatalf("读取凭据库失败：%v", err)
	}
	key, ok := store.Lookup("opencode-go", "work")
	if !ok || key != "sk-abcdefgh" {
		t.Errorf("存下的密钥 = %q, %v，期望 sk-abcdefgh", key, ok)
	}
	out := stdout.String()
	if strings.Contains(out, "sk-abcdefgh") {
		t.Errorf("stdout = %q，不应回显密钥", out)
	}
	for _, want := range []string{"opencode-go", "work"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout = %q，期望含 %q", out, want)
		}
	}
	if !strings.Contains(stderr.String(), "未在登录时校验密钥") {
		t.Errorf("stderr = %q，期望说明没有校验密钥", stderr.String())
	}
}

// 省略账号名时用 default。
func TestLoginDefaultsAccountName(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("NOVA_TEST_KEY", "sk-default")

	var stdout, stderr bytes.Buffer
	if got := execute([]string{"login", "opencode-go", "--key-env", "NOVA_TEST_KEY"},
		&stdout, &stderr); got != exitOK {
		t.Fatalf("退出码 = %d，期望 %d\n--- stderr ---\n%s", got, exitOK, stderr.String())
	}
	store, err := credentials.Load(dir)
	if err != nil {
		t.Fatalf("读取凭据库失败：%v", err)
	}
	if _, ok := store.Lookup("opencode-go", "default"); !ok {
		t.Error("省略账号名时应存成 default")
	}
}

// --no-store 只把密钥写到 stdout，不落盘。
func TestLoginNoStoreWritesOnlyStdout(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("NOVA_TEST_KEY", "sk-abcdefgh")

	var stdout, stderr bytes.Buffer
	if got := execute([]string{"login", "opencode-go", "--key-env", "NOVA_TEST_KEY", "--no-store"},
		&stdout, &stderr); got != exitOK {
		t.Fatalf("退出码 = %d，期望 %d\n--- stderr ---\n%s", got, exitOK, stderr.String())
	}
	if stdout.String() != "sk-abcdefgh\n" {
		t.Errorf("stdout = %q，期望只写出密钥一行", stdout.String())
	}
	path := filepath.Join(dir, "nova", "credentials.json")
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("--no-store 不应落盘，实际 err = %v", err)
	}
	if !strings.Contains(stderr.String(), "未在登录时校验密钥") {
		t.Errorf("stderr = %q，期望说明没有校验密钥", stderr.String())
	}
}

// --key-stdin 从标准输入读密钥。
func TestLoginKeyStdin(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	swapStdin(t, "sk-from-stdin\n")

	var stdout, stderr bytes.Buffer
	if got := execute([]string{"login", "opencode-go", "--key-stdin"}, &stdout, &stderr); got != exitOK {
		t.Fatalf("退出码 = %d，期望 %d\n--- stderr ---\n%s", got, exitOK, stderr.String())
	}
	store, err := credentials.Load(dir)
	if err != nil {
		t.Fatalf("读取凭据库失败：%v", err)
	}
	if key, ok := store.Lookup("opencode-go", "default"); !ok || key != "sk-from-stdin" {
		t.Errorf("存下的密钥 = %q, %v，期望 sk-from-stdin", key, ok)
	}
}

// 标准输入不是终端且没有来源标志时报错，并说明可用的标志。
func TestLoginNonTTYWithoutSourceFails(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	swapStdin(t, "")

	var stdout, stderr bytes.Buffer
	got := execute([]string{"login", "opencode-go"}, &stdout, &stderr)
	if got != exitError {
		t.Fatalf("退出码 = %d，期望 %d\n--- stderr ---\n%s", got, exitError, stderr.String())
	}
	for _, want := range []string{"--key-stdin", "--key-env"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr = %q，期望提到 %q", stderr.String(), want)
		}
	}
}

// --key-env 指向的环境变量为空时报错。
func TestLoginKeyEnvEmptyFails(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("NOVA_TEST_EMPTY_KEY", "")

	var stdout, stderr bytes.Buffer
	got := execute([]string{"login", "opencode-go", "--key-env", "NOVA_TEST_EMPTY_KEY"}, &stdout, &stderr)
	if got != exitError {
		t.Fatalf("退出码 = %d，期望 %d\n--- stderr ---\n%s", got, exitError, stderr.String())
	}
	if !strings.Contains(stderr.String(), "NOVA_TEST_EMPTY_KEY") {
		t.Errorf("stderr = %q，期望指出环境变量名", stderr.String())
	}
}

// 省略档案 id 时列出内置档案的 id，并在 stdout 上给出可选集合。
func TestLoginWithoutProfileListsBuiltin(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("NOVA_TEST_KEY", "sk-x")

	var stdout, stderr bytes.Buffer
	got := execute([]string{"login", "--key-env", "NOVA_TEST_KEY"}, &stdout, &stderr)
	if got == exitOK {
		t.Fatal("未给出档案 id 应非 0 退出")
	}
	for _, want := range []string{"opencode-go", "deepseek-official", "sensenova"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout = %q，期望列出内置档案 %q", stdout.String(), want)
		}
	}
}
