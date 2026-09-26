package main

import (
	"bytes"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeProfileFile 把一个合法档案写进目录。
func writeProfileFile(t *testing.T, dir, name, id string) {
	t.Helper()
	full := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatalf("创建档案目录失败：%v", err)
	}
	content := "schema: 1\nid: " + id + "\n" +
		"endpoints:\n  openai_chat:\n    url: https://example.com/v1/chat/completions\n    protocol: openai_chat\n"
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatalf("写档案失败：%v", err)
	}
}

// unreachableRemote 返回一个确定连不上的回环远端地址：本机随机取一个端口后立即关闭，
// 之后的连接尝试会得到 connection refused，而不是依赖某个固定端口恰好无人监听。
func unreachableRemote(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占用回环端口失败：%v", err)
	}
	address := listener.Addr().String()
	listener.Close()
	return "https://" + address + "/profiles"
}

// 某个源失败不中断后面的源：一次跑完才知道全部情况，退出码仍为非 0。
func TestProfilesUpdateContinuesAfterFailure(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)

	remote := unreachableRemote(t)
	path := writeConfig(t, "version 1\nprofiles {\n"+
		"    source "+remote+"\n"+
		"    source ./profiles.local\n"+
		"}\n")
	local := filepath.Join(filepath.Dir(path), "profiles.local")
	writeProfileFile(t, local, "b.yaml", "bbb")
	writeProfileFile(t, local, "a.yaml", "aaa")

	var stdout, stderr bytes.Buffer
	if got := execute([]string{"profiles", "update", "-c", path}, &stdout, &stderr); got != exitError {
		t.Fatalf("退出码 = %d，期望 %d\n--- stdout ---\n%s--- stderr ---\n%s",
			got, exitError, stdout.String(), stderr.String())
	}

	out := stdout.String()
	// 失败的源给一行结果，但后面的本地源仍然被处理。
	for _, want := range []string{
		remote + " 失败",
		"本地源 " + local,
		"档案 2 份",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout = %q，期望含 %q", out, want)
		}
	}
	// 失败详情在 stderr，stdout 只留给人看的结果；详情必须来自同步层，
	// 而不是命令层自己拼一句「失败了」。
	for _, want := range []string{"失败 " + remote, "请求 "} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr = %q，期望含 %q", stderr.String(), want)
		}
	}
}

// builtin 随二进制发布，同步它既不联网也不落地任何状态。
func TestProfilesUpdateBuiltinDoesNotSync(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)

	path := writeConfig(t, "version 1\nprofiles {\n    source builtin\n}\n")

	var stdout, stderr bytes.Buffer
	if got := execute([]string{"profiles", "update", "-c", path}, &stdout, &stderr); got != exitOK {
		t.Fatalf("退出码 = %d，期望 %d\n--- stderr ---\n%s", got, exitOK, stderr.String())
	}
	if !strings.Contains(stdout.String(), "内置源") || !strings.Contains(stdout.String(), "不联网") {
		t.Errorf("stdout = %q，期望说明内置源不联网", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q，期望干净", stderr.String())
	}
	// 没有本地状态被写出：builtin 从未经过同步路径。
	if _, err := os.Stat(filepath.Join(base, "nova", "state.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("内置源不应写状态文件，实际 err = %v", err)
	}
}

// 从未同步过的远端源显示「尚未同步」而不是报错，且不联网（地址不可达）。
func TestProfilesListWithoutSyncDoesNotFail(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	path := writeConfig(t, "version 1\nprofiles {\n    source "+unreachableRemote(t)+"\n}\n")

	var stdout, stderr bytes.Buffer
	if got := execute([]string{"profiles", "list", "-c", path}, &stdout, &stderr); got != exitOK {
		t.Fatalf("退出码 = %d，期望 %d\n--- stderr ---\n%s", got, exitOK, stderr.String())
	}
	if !strings.Contains(stdout.String(), "尚未同步") {
		t.Errorf("stdout = %q，期望显示尚未同步", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q，期望干净", stderr.String())
	}
}

// list 直接读 state.json 与已装快照目录，不重跑同步。
func TestProfilesListReadsInstalledSnapshot(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)
	stateDir := filepath.Join(base, "nova")
	root := filepath.Join(stateDir, "profiles", "7")
	writeProfileFile(t, filepath.Join(root, "providers"), "a.yaml", "alpha")
	writeProfileFile(t, filepath.Join(root, "providers"), "b.yaml", "beta")

	writeStateFile(t, filepath.Join(stateDir, "state.json"),
		`{"version":1,"sources":{"https://example.com/profiles":`+
			`{"serial":7,"installed_at":"2026-01-02T03:04:05Z","root":`+jsonQuote(root)+`}}}`)

	path := writeConfig(t, "version 1\nprofiles {\n    source https://example.com/profiles\n}\n")

	var stdout, stderr bytes.Buffer
	if got := execute([]string{"profiles", "list", "-c", path}, &stdout, &stderr); got != exitOK {
		t.Fatalf("退出码 = %d，期望 %d\n--- stderr ---\n%s", got, exitOK, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"serial 7", "上次更新 2026-01-02T03:04:05Z", "档案 2 份：alpha、beta"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout = %q，期望含 %q", out, want)
		}
	}

	stdout.Reset()
	if got := execute([]string{"profiles", "list", "-c", path, "--verbose"}, &stdout, &stderr); got != exitOK {
		t.Fatalf("退出码 = %d，期望 %d\n--- stderr ---\n%s", got, exitOK, stderr.String())
	}
	if !strings.Contains(stdout.String(), "\n  alpha\n") {
		t.Errorf("verbose 输出 = %q，期望逐份列出档案", stdout.String())
	}
}

// 磁盘上一份已装快照被外部删掉时，list 不该整条不可用：失败的源记一行，后面的源继续。
func TestProfilesListContinuesAfterBrokenRemote(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)
	stateDir := filepath.Join(base, "nova")
	// 状态里登记了一个已被删除的快照目录。
	writeStateFile(t, filepath.Join(stateDir, "state.json"),
		`{"version":1,"sources":{"https://example.com/broken":`+
			`{"serial":3,"installed_at":"2026-01-02T03:04:05Z","root":`+
			jsonQuote(filepath.Join(stateDir, "profiles", "gone"))+`}}}`)

	path := writeConfig(t, "version 1\nprofiles {\n"+
		"    source https://example.com/broken\n"+
		"    source ./profiles.local\n"+
		"}\n")
	local := filepath.Join(filepath.Dir(path), "profiles.local")
	writeProfileFile(t, local, "a.yaml", "aaa")

	var stdout, stderr bytes.Buffer
	if got := execute([]string{"profiles", "list", "-c", path}, &stdout, &stderr); got != exitError {
		t.Fatalf("退出码 = %d，期望 %d\n--- stdout ---\n%s--- stderr ---\n%s",
			got, exitError, stdout.String(), stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"远端源 https://example.com/broken 读取失败",
		"本地源 " + local,
		"档案 1 份：aaa",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout = %q，期望含 %q", out, want)
		}
	}
	if !strings.Contains(stderr.String(), "失败 https://example.com/broken") {
		t.Errorf("stderr = %q，期望说明远端源读取失败", stderr.String())
	}
}

// writeStateFile 写一份 state.json。
func writeStateFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("创建状态目录失败：%v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写状态文件失败：%v", err)
	}
}

// jsonQuote 把路径安全地嵌进 JSON 字符串，避免 Windows 风格反斜杠破坏文档。
func jsonQuote(s string) string {
	return `"` + strings.ReplaceAll(s, `\`, `\\`) + `"`
}
