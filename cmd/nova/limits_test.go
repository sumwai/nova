package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// usageProbeScript 是一份能产出合法额度文档的探测脚本。
const usageProbeScript = `#!/bin/sh
printf '%s\n' \
  'schema: 1' \
  'source: exec' \
  'observed_at: 2026-09-27T00:00:00Z' \
  'account:' \
  '  - { kind: quota, metric: usd, window: 5h, limit: 14, remaining: 5.2 }'
`

// usageProfile 造一份带 usage 声明的本地档案内容。
func usageProfile(usage string) string {
	return "schema: 1\nid: relay\n" +
		"auth: { header: authorization, scheme: Bearer, env: RELAY_KEY }\n" +
		"endpoints:\n  openai_chat: { url: https://relay.example.com/v1/chat/completions, protocol: openai_chat }\n" +
		"models:\n  - { id: m }\n" +
		usage + "\n"
}

// writeUsageConfig 在配置目录旁放一份本地档案，返回配置路径。
func writeUsageConfig(t *testing.T, configSource, profileContent string) string {
	t.Helper()
	configPath := writeConfig(t, configSource)
	profileDir := filepath.Join(filepath.Dir(configPath), "profiles")
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		t.Fatalf("创建档案目录失败：%v", err)
	}
	if err := os.WriteFile(filepath.Join(profileDir, "relay.yaml"), []byte(profileContent), 0o600); err != nil {
		t.Fatalf("写入档案失败：%v", err)
	}
	return configPath
}

// TestLimitsCheckRunsExecProbe 守护 limits check 跑通 exec 探测并回显解析结果。
func TestLimitsCheckRunsExecProbe(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("RELAY_KEY", "k")

	probeDir := filepath.Join(dataHome, "nova", "probes")
	if err := os.MkdirAll(probeDir, 0o700); err != nil {
		t.Fatalf("创建 probes 目录失败：%v", err)
	}
	script := filepath.Join(probeDir, "usage.sh")
	if err := os.WriteFile(script, []byte(usageProbeScript), 0o700); err != nil {
		t.Fatalf("写入探测脚本失败：%v", err)
	}

	configPath := writeUsageConfig(t,
		"version 1\nprofiles {\n    source ./profiles\n}\nprovider relay\n",
		usageProfile(`usage: { exec: "`+script+`", interval: 60s }`))

	var stdout, stderr bytes.Buffer
	if got := execute([]string{"limits", "check", "relay", "-c", configPath}, &stdout, &stderr); got != exitOK {
		t.Fatalf("退出码 = %d，期望 %d\n--- stdout ---\n%s--- stderr ---\n%s",
			got, exitOK, stdout.String(), stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"provider relay", "source      exec", "账号级", "remaining=5.2"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout = %q，期望含 %q", out, want)
		}
	}
}

// TestLimitsCheckBuiltinProbeUnavailable 守护内置 probe 明确报未实现，不静默。
func TestLimitsCheckBuiltinProbeUnavailable(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("RELAY_KEY", "k")

	configPath := writeUsageConfig(t,
		"version 1\nprofiles {\n    source ./profiles\n}\nprovider relay\n",
		usageProfile("usage: { probe: opencode-go }"))

	var stdout, stderr bytes.Buffer
	if got := execute([]string{"limits", "check", "relay", "-c", configPath}, &stdout, &stderr); got != exitError {
		t.Fatalf("退出码 = %d，期望 %d", got, exitError)
	}
	if !strings.Contains(stderr.String(), "未实现内置探测") {
		t.Errorf("stderr = %q，期望说明内置探测未实现", stderr.String())
	}
}

// TestLimitsCheckWithoutUsage 守护没有声明 usage 的渠道给出可操作的报错。
func TestLimitsCheckWithoutUsage(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("RELAY_KEY", "k")

	configPath := writeUsageConfig(t,
		"version 1\nprofiles {\n    source ./profiles\n}\nprovider relay\n",
		usageProfile("")) // 不带 usage 行时该行会多出一个空行，YAML 允许

	var stdout, stderr bytes.Buffer
	if got := execute([]string{"limits", "check", "relay", "-c", configPath}, &stdout, &stderr); got != exitError {
		t.Fatalf("退出码 = %d，期望 %d\n--- stderr ---\n%s", got, exitError, stderr.String())
	}
	if !strings.Contains(stderr.String(), "没有声明 usage") {
		t.Errorf("stderr = %q，期望说明没有声明 usage", stderr.String())
	}
}

// TestLimitsCheckUnknownProvider 守护渠道名写错时列出已声明的渠道。
func TestLimitsCheckUnknownProvider(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("RELAY_KEY", "k")

	configPath := writeUsageConfig(t,
		"version 1\nprofiles {\n    source ./profiles\n}\nprovider relay\n",
		usageProfile(`usage: { probe: opencode-go }`))

	var stdout, stderr bytes.Buffer
	if got := execute([]string{"limits", "check", "nope", "-c", configPath}, &stdout, &stderr); got != exitError {
		t.Fatalf("退出码 = %d，期望 %d", got, exitError)
	}
	if !strings.Contains(stderr.String(), "没有渠道") || !strings.Contains(stderr.String(), "relay") {
		t.Errorf("stderr = %q，期望列出已声明的渠道", stderr.String())
	}
}
