package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig 把一份配置写进临时文件并返回它的路径。
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "Novafile")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写配置文件失败：%v", err)
	}
	return path
}

// listingConfig 造一份只有一条发现型端点的配置，清单地址由调用方给出。
func listingConfig(listing string, extra ...string) string {
	lines := []string{
		"version 1",
		"provider relay {",
		"    api_key sk-test",
		"    url https://relay.example.com/v1/chat/completions",
		"    discover " + listing,
	}
	lines = append(lines, extra...)
	lines = append(lines, "}")
	return strings.Join(lines, "\n") + "\n"
}

func TestModelsCommandPrintsCatalog(t *testing.T) {
	listing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"object":"list","data":[
			{"id":"gpt-5","object":"model"},
			{"id":"gpt-5-mini","object":"model"},
			{"id":"text-embedding-3","object":"model"}
		]}`)
	}))
	defer listing.Close()

	path := writeConfig(t, listingConfig(listing.URL+"/v1/models", "    allow gpt-5*"))

	var stdout, stderr bytes.Buffer
	if got := execute([]string{"models", "-c", path}, &stdout, &stderr); got != exitOK {
		t.Fatalf("退出码 = %d，期望 %d\n--- stdout ---\n%s--- stderr ---\n%s",
			got, exitOK, stdout.String(), stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"relay", listing.URL + "/v1/models", "发现 3 保留 2 排除 1",
		"  保留 2", "    gpt-5", "    gpt-5-mini",
		"  排除 1", "    text-embedding-3",
		"对外模型 2 个（显式 0 个，发现 2 个）"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout = %q，期望含 %q", out, want)
		}
	}
	// 来源标注要加 --verbose 才有：默认输出先回答「有哪些」。
	if strings.Contains(out, "discovered") {
		t.Errorf("非 verbose 不该带来源标注：%q", out)
	}
	// 发现过程的日志不再另写 stderr：结果已经全在这份报告里。
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q，期望干净", stderr.String())
	}
}

func TestModelsCommandVerboseListsModelsAndExcluded(t *testing.T) {
	listing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"id":"gpt-5"},{"id":"other-model"}]}`)
	}))
	defer listing.Close()

	path := writeConfig(t, listingConfig(listing.URL+"/v1/models", "    allow gpt-5*"))

	var stdout, stderr bytes.Buffer
	if got := execute([]string{"models", "-c", path, "--verbose"}, &stdout, &stderr); got != exitOK {
		t.Fatalf("退出码 = %d，期望 %d\n--- stderr ---\n%s", got, exitOK, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"    gpt-5  discovered", "    other-model"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout = %q，期望含 %q", out, want)
		}
	}
}

// 发现型端点的清单由上游决定，config check 因此只校验写法并说明这一点，不去连上游。
func TestConfigCheckNotesDiscoveryWithoutNetwork(t *testing.T) {
	// 这个地址不可能连上：check 若真的去请求，它会以运行期失败收场。
	path := writeConfig(t, listingConfig("http://127.0.0.1:1/v1/models"))

	var stdout, stderr bytes.Buffer
	if got := execute([]string{"config", "check", "-c", path}, &stdout, &stderr); got != exitOK {
		t.Fatalf("退出码 = %d，期望 %d\n--- stderr ---\n%s", got, exitOK, stderr.String())
	}
	if !strings.Contains(stderr.String(), "声明了模型发现") {
		t.Errorf("stderr = %q，期望提示清单未经校验", stderr.String())
	}
	if !strings.Contains(stdout.String(), "校验通过") {
		t.Errorf("stdout = %q，期望结论", stdout.String())
	}
}

// 发现失败且端点没有显式模型时，nova models 以运行期失败收场（退出码 1）。
func TestModelsCommandFailsWhenDiscoveryIsFatal(t *testing.T) {
	listing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer listing.Close()

	path := writeConfig(t, listingConfig(listing.URL+"/v1/models"))

	var stdout, stderr bytes.Buffer
	if got := execute([]string{"models", "-c", path}, &stdout, &stderr); got != exitError {
		t.Fatalf("退出码 = %d，期望 %d\n--- stdout ---\n%s", got, exitError, stdout.String())
	}
	if !strings.Contains(stderr.String(), "模型发现失败") {
		t.Errorf("stderr = %q，期望说明发现失败", stderr.String())
	}
}

// multiProviderConfig 造两条各自发现同一份清单、但 allow 不同的渠道。
func multiProviderConfig(listing string) string {
	return "version 1\n" +
		"provider a {\n" +
		"    api_key ka\n" +
		"    url https://a.example.com/v1/chat/completions\n" +
		"    discover " + listing + "/v1/models\n" +
		"    allow gpt-*\n" +
		"}\n" +
		"provider b {\n" +
		"    api_key kb\n" +
		"    url https://b.example.com/v1/chat/completions\n" +
		"    discover " + listing + "/v1/models\n" +
		"    allow claude-*\n" +
		"}\n"
}

func TestModelsCommandFiltersByProvider(t *testing.T) {
	listing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"id":"gpt-5"},{"id":"claude-sonnet-4"}]}`)
	}))
	defer listing.Close()
	path := writeConfig(t, multiProviderConfig(listing.URL))

	var stdout, stderr bytes.Buffer
	if got := execute([]string{"models", "-c", path}, &stdout, &stderr); got != exitOK {
		t.Fatalf("退出码 = %d，期望 %d\n--- stderr ---\n%s", got, exitOK, stderr.String())
	}
	all := stdout.String()
	// 不带过滤时两条渠道都在，汇总是整份目录。
	for _, want := range []string{"a  " + listing.URL, "b  " + listing.URL,
		"对外模型 2 个（显式 0 个，发现 2 个）"} {
		if !strings.Contains(all, want) {
			t.Errorf("未过滤的输出 = %q，期望含 %q", all, want)
		}
	}

	stdout.Reset()
	stderr.Reset()
	if got := execute([]string{"models", "-c", path, "--provider", "a"}, &stdout, &stderr); got != exitOK {
		t.Fatalf("退出码 = %d，期望 %d\n--- stderr ---\n%s", got, exitOK, stderr.String())
	}
	filtered := stdout.String()
	if !strings.Contains(filtered, "provider a 的对外模型 1 个（显式 0 个，发现 1 个）") {
		t.Errorf("过滤后的输出 = %q，期望汇总只算这条渠道", filtered)
	}
	// 别人家的端点段一概不出现，否则过滤等于没做。
	// 注意排除列表里出现 claude-sonnet-4 是对的：它确实是这条渠道清单里被 allow 拦下的模型。
	for _, unwanted := range []string{"provider b", "b  " + listing.URL} {
		if strings.Contains(filtered, unwanted) {
			t.Errorf("过滤后的输出 = %q，不该含 %q", filtered, unwanted)
		}
	}
	for _, want := range []string{"  排除 1", "    claude-sonnet-4"} {
		if !strings.Contains(filtered, want) {
			t.Errorf("过滤后的输出 = %q，期望含 %q", filtered, want)
		}
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q，期望干净", stderr.String())
	}
}

// 名字写错时直接报错并列出可用名字：静默给一份空报告会被读成「这条渠道没有模型」。
func TestModelsCommandRejectsUnknownProvider(t *testing.T) {
	listing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"id":"gpt-5"}]}`)
	}))
	defer listing.Close()
	path := writeConfig(t, multiProviderConfig(listing.URL))

	var stdout, stderr bytes.Buffer
	if got := execute([]string{"models", "-c", path, "--provider", "nope"}, &stdout, &stderr); got != exitError {
		t.Fatalf("退出码 = %d，期望 %d\n--- stdout ---\n%s", got, exitError, stdout.String())
	}
	if !strings.Contains(stderr.String(), "没有名为") || !strings.Contains(stderr.String(), "a, b") {
		t.Errorf("stderr = %q，期望指出名字不存在并列出可用渠道", stderr.String())
	}
}

// 别名映射（model gpt-4o openai/gpt-4o）只能从显式声明那一段看见：
// 只列清单视角的保留与排除时，客户端能用的这个名字会凭空少一个。
func TestModelsCommandShowsDeclaredAliases(t *testing.T) {
	listing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"id":"openai/gpt-4o"},{"id":"openai/gpt-4o-mini"}]}`)
	}))
	defer listing.Close()

	path := writeConfig(t, `
version 1
provider agg {
    api_key k
    url https://agg.example.com/v1/chat/completions
    discover `+listing.URL+`/v1/models
    allow *
    deny openai/gpt-4o
    model gpt-4o openai/gpt-4o
}
`)

	var stdout, stderr bytes.Buffer
	if got := execute([]string{"models", "-c", path}, &stdout, &stderr); got != exitOK {
		t.Fatalf("退出码 = %d，期望 %d\n--- stderr ---\n%s", got, exitOK, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"  显式声明 1", "    gpt-4o  → openai/gpt-4o",
		"对外模型 2 个（显式 1 个，发现 1 个）"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout = %q，期望含 %q", out, want)
		}
	}
}

// expose 改名后的对外名要进「保留」段，并标出上游名：客户端请求的是前者，
// 网关发给上游的是后者。
func TestModelsCommandShowsExposeMapping(t *testing.T) {
	listing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"id":"sensenova/kimi-k3"},{"id":"glm-5.2"}]}`)
	}))
	defer listing.Close()

	path := writeConfig(t, `
version 1
provider cc {
    api_key k
    url https://cc.example.com/v1/chat/completions
    discover `+listing.URL+`/v1/models
    allow *
    expose sensenova/* *
    expose nope/* x/*
}
`)

	var stdout, stderr bytes.Buffer
	if got := execute([]string{"models", "-c", path}, &stdout, &stderr); got != exitOK {
		t.Fatalf("退出码 = %d，期望 %d\n--- stderr ---\n%s", got, exitOK, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"  保留 2", "    kimi-k3  → sensenova/kimi-k3", "    glm-5.2",
		"未命中的改名规则 1", "    expose nope/* x/*",
		"对外模型 2 个（显式 0 个，发现 2 个）"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout = %q，期望含 %q", out, want)
		}
	}
}

// 指定的渠道没有声明 discover 时，列出它显式声明的模型，而不是只回一句「没有发现」。
func TestModelsCommandProviderWithoutDiscovery(t *testing.T) {
	path := writeConfig(t, `
version 1
provider solo {
    api_key k
    url https://solo.example.com/v1/chat/completions
    model m1
    model m2
}
`)

	var stdout, stderr bytes.Buffer
	if got := execute([]string{"models", "-c", path, "--provider", "solo"}, &stdout, &stderr); got != exitOK {
		t.Fatalf("退出码 = %d，期望 %d\n--- stderr ---\n%s", got, exitOK, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"solo  没有声明 discover", "  显式声明 2", "    m1", "    m2",
		"provider solo 的对外模型 2 个（显式 2 个，发现 0 个）"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout = %q，期望含 %q", out, want)
		}
	}
}
