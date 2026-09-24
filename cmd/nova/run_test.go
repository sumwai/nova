package main

import (
	"path/filepath"
	"testing"
)

func TestResolveConfigPathPrecedence(t *testing.T) {
	env := map[string]string{configEnvVar: "/from/env/Novafile"}
	getenv := func(key string) string { return env[key] }

	tests := []struct {
		name      string
		flagValue string
		want      string
	}{
		{"命令行优先于环境变量", "/from/flag/Novafile", "/from/flag/Novafile"},
		{"没有命令行时取环境变量", "", "/from/env/Novafile"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveConfigPath(tt.flagValue, getenv)
			if err != nil {
				t.Fatalf("意外失败：%v", err)
			}
			if got != tt.want {
				t.Errorf("路径 = %q，期望 %q", got, tt.want)
			}
		})
	}
}

// getenv 以入参注入，正是为了让「两边都空时退回缺省」这条分支
// 不依赖真实的进程环境就能被检查。
func TestResolveConfigPathFallsBackToDefault(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	got, err := resolveConfigPath("", func(string) string { return "" })
	if err != nil {
		t.Fatalf("意外失败：%v", err)
	}
	want := filepath.Join(dir, "nova", "Novafile")
	if got != want {
		t.Errorf("路径 = %q，期望 %q", got, want)
	}
}

func TestDefaultConfigPathFollowsXDG(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	got, err := defaultConfigPath()
	if err != nil {
		t.Fatalf("意外失败：%v", err)
	}
	if want := filepath.Join(dir, "nova", "Novafile"); got != want {
		t.Errorf("路径 = %q，期望 %q", got, want)
	}
}

// 定位不到用户配置目录时报错，而不是退回当前目录的相对路径：
// 相对路径会让「nova 读的是哪份配置」随工作目录漂移，这是排障时最不该有歧义的一件事。
func TestDefaultConfigPathFailsLoudlyWithoutHome(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")

	if _, err := defaultConfigPath(); err == nil {
		t.Fatal("期望报错，但它返回了一个路径")
	}
}

func TestDialAddress(t *testing.T) {
	tests := []struct {
		name   string
		listen string
		want   string
	}{
		{"空 host 换成回环", ":2026", "127.0.0.1:2026"},
		{"0.0.0.0 换成回环", "0.0.0.0:2026", "127.0.0.1:2026"},
		{"IPv6 任意地址换成回环", "[::]:2026", "127.0.0.1:2026"},
		{"回环原样", "127.0.0.1:2026", "127.0.0.1:2026"},
		{"主机名原样", "localhost:2026", "localhost:2026"},
		{"内网地址原样", "10.0.0.5:2026", "10.0.0.5:2026"},
		{"IPv6 回环原样", "[::1]:2026", "[::1]:2026"},
		{"不成 host:port 形态时原样返回", "localhost", "localhost"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dialAddress(tt.listen); got != tt.want {
				t.Errorf("dialAddress(%q) = %q，期望 %q", tt.listen, got, tt.want)
			}
		})
	}
}
