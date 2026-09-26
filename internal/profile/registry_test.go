package profile

import (
	"os"
	"path/filepath"
	"testing"
)

// sourceProfile 造一份合法档案，id 与 name 可控，供本地源覆盖用例使用。
func sourceProfile(id, name string) string {
	return "schema: 1\nid: " + id + "\nname: " + name +
		"\nendpoints:\n  openai_chat:\n    url: https://example.com/v1/chat/completions\n    protocol: openai_chat\n"
}

// writeLocalSource 把一份档案写进新建的临时目录，返回目录路径。
func writeLocalSource(t *testing.T, filename, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, filename), []byte(content), 0o600); err != nil {
		t.Fatalf("写入本地源档案失败：%v", err)
	}
	return dir
}

// builtinByID 从内置档案里取出指定 id 的一份，取不到即失败。
func builtinByID(t *testing.T, id string) *Profile {
	t.Helper()
	profiles, err := Builtin()
	if err != nil {
		t.Fatalf("加载内置档案失败：%v", err)
	}
	for _, loaded := range profiles {
		if loaded.ID == id {
			return loaded
		}
	}
	t.Fatalf("内置档案里没有 id %q", id)
	return nil
}

// TestBuiltinProfilesLoad 守护三条内置档案可用，且 id 按文件名排序后与预期一致。
func TestBuiltinProfilesLoad(t *testing.T) {
	profiles, err := Builtin()
	if err != nil {
		t.Fatalf("内置档案应加载成功，实际失败：%v", err)
	}

	// 文件名排序：deepseek-official.yaml、opencode-go.yaml、sensenova.yaml。
	want := []string{"deepseek-official", "opencode-go", "sensenova"}
	got := make([]string, 0, len(profiles))
	for _, loaded := range profiles {
		got = append(got, loaded.ID)
	}
	if len(got) != len(want) {
		t.Fatalf("内置档案 %d 份 %v，期望 %d 份 %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 份内置档案 id = %q，期望 %q", i+1, got[i], want[i])
		}
	}
}

// TestBuiltinProfilesPinPlatformFacts 钉住三份内置档案的平台事实。
//
// api_pattern 不进入展开结果（配置层不承载它），auth.header 也只以注入形态的
// 形式落到渠道上；它们只能在这里拦。env 是凭据来源，写错会在运行期变成
// 「环境变量未设置」，同样值得钉住。
func TestBuiltinProfilesPinPlatformFacts(t *testing.T) {
	want := map[string]struct {
		pattern string
		header  string
		scheme  string
		env     string
	}{
		"opencode-go":       {`^https://opencode\.ai/zen/go/`, "authorization", "Bearer", "OPENCODE_API_KEY"},
		"sensenova":         {`^https://token\.sensenova\.cn/`, "authorization", "Bearer", "SENSENOVA_API_KEY"},
		"deepseek-official": {`^https://api\.deepseek\.com/`, "authorization", "Bearer", "DEEPSEEK_API_KEY"},
	}
	for id, facts := range want {
		loaded := builtinByID(t, id)
		if loaded.APIPattern != facts.pattern {
			t.Errorf("%s 的 api_pattern = %q，期望 %q", id, loaded.APIPattern, facts.pattern)
		}
		if loaded.Auth.Header != facts.header {
			t.Errorf("%s 的 auth.header = %q，期望 %q", id, loaded.Auth.Header, facts.header)
		}
		if loaded.Auth.Scheme != facts.scheme {
			t.Errorf("%s 的 auth.scheme = %q，期望 %q", id, loaded.Auth.Scheme, facts.scheme)
		}
		if loaded.Auth.Env != facts.env {
			t.Errorf("%s 的 auth.env = %q，期望 %q", id, loaded.Auth.Env, facts.env)
		}
	}
}

// TestBuiltinPreservesDeclarationOrder 守护内置档案的声明顺序不被改动：
// 模型条目顺序、模型引用的端点顺序，以及带点的模型 id。
func TestBuiltinPreservesDeclarationOrder(t *testing.T) {
	deepseek := builtinByID(t, "deepseek-official")
	wantModels := []string{"deepseek-flash", "deepseek-v4-pro"}
	if len(deepseek.Models) != len(wantModels) {
		t.Fatalf("deepseek-official 有 %d 条模型，期望 %d 条", len(deepseek.Models), len(wantModels))
	}
	for i, want := range wantModels {
		if deepseek.Models[i].ID != want {
			t.Errorf("deepseek-official 第 %d 条模型 id = %q，期望 %q", i+1, deepseek.Models[i].ID, want)
		}
	}
	// 端点声明顺序决定协议优先级，必须与档案书写一致。
	wantEndpoints := []string{"openai_chat", "anthropic_messages"}
	got := deepseek.Models[0].Endpoints
	if len(got) != len(wantEndpoints) {
		t.Fatalf("deepseek-flash 声明了 %d 条端点 %v，期望 %d 条", len(got), got, len(wantEndpoints))
	}
	for i := range wantEndpoints {
		if got[i] != wantEndpoints[i] {
			t.Errorf("deepseek-flash 第 %d 条端点 = %q，期望 %q", i+1, got[i], wantEndpoints[i])
		}
	}

	// deepseek-v4.1-flash 的点是模型 id 的一部分，不得被解析或改写。
	opencode := builtinByID(t, "opencode-go")
	found := false
	for _, model := range opencode.Models {
		if model.ID == "deepseek-v4.1-flash" {
			found = true
		}
	}
	if !found {
		t.Errorf("opencode-go 缺少模型 id %q，实际为 %v", "deepseek-v4.1-flash", modelIDs(opencode))
	}
}

// modelIDs 取档案里的模型 id 列表，用于失败信息。
func modelIDs(loaded *Profile) []string {
	ids := make([]string, 0, len(loaded.Models))
	for _, model := range loaded.Models {
		ids = append(ids, model.ID)
	}
	return ids
}

// TestRegistryAddOverwritesAndKeepsPosition 覆盖同 id 覆盖与位置保持。
func TestRegistryAddOverwritesAndKeepsPosition(t *testing.T) {
	registry := NewRegistry()
	registry.Add(&Profile{ID: "a", Name: "第一"})
	registry.Add(&Profile{ID: "b", Name: "第二"})
	registry.Add(&Profile{ID: "a", Name: "覆盖"})

	if got := registry.Lookup; got == nil {
		t.Fatal("Lookup 不应为 nil")
	}
	loaded, ok := registry.Lookup("a")
	if !ok || loaded.Name != "覆盖" {
		t.Errorf("id a 的内容 = %+v，期望 name 为「覆盖」", loaded)
	}
	want := []string{"a", "b"}
	got := registry.IDs()
	if len(got) != len(want) {
		t.Fatalf("IDs() = %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("IDs()[%d] = %q，期望 %q", i, got[i], want[i])
		}
	}
}

// TestLoadSourcesLaterSourceOverrides 覆盖「后声明的源覆盖前声明的源」，
// 以及 builtin 无论声明在哪里都只是基底、不回改已覆盖的条目。
func TestLoadSourcesLaterSourceOverrides(t *testing.T) {
	first := writeLocalSource(t, "a.yaml", sourceProfile("opencode-go", "先声明的源"))
	second := writeLocalSource(t, "b.yaml", sourceProfile("opencode-go", "后声明的源"))

	sources := []Source{
		{Kind: SourceLocal, Path: first},
		{Kind: SourceLocal, Path: second},
		{Kind: SourceBuiltin},
	}
	registry, statuses, err := LoadSources(sources, &Syncer{})
	if err != nil {
		t.Fatalf("装配注册表失败：%v", err)
	}

	loaded, ok := registry.Lookup("opencode-go")
	if !ok {
		t.Fatal("注册表里应有 opencode-go")
	}
	if loaded.Name != "后声明的源" {
		t.Errorf("opencode-go 的 name = %q，期望后声明的源覆盖", loaded.Name)
	}
	// builtin 声明在最后，但基底优先级最低，不得把覆盖结果改回去。
	if loaded.Name == "OpenCode Go" {
		t.Errorf("builtin 声明在最后时不应覆盖前面的源")
	}
	// 未被子孙覆盖的内置档案仍在。
	if _, ok := registry.Lookup("sensenova"); !ok {
		t.Errorf("注册表里应保留内置档案 sensenova")
	}

	want := []string{"deepseek-official", "opencode-go", "sensenova"}
	got := registry.IDs()
	if len(got) != len(want) {
		t.Fatalf("IDs() = %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("IDs()[%d] = %q，期望 %q", i, got[i], want[i])
		}
	}

	if len(statuses) != 3 {
		t.Fatalf("状态 %d 条，期望 3 条", len(statuses))
	}
	if statuses[0].Kind != SourceLocal || statuses[0].Count != 1 || !statuses[0].Synced {
		t.Errorf("第一个本地源状态 = %+v，期望 1 份且已同步", statuses[0])
	}
	if statuses[1].Kind != SourceLocal || statuses[1].Count != 1 || !statuses[1].Synced {
		t.Errorf("第二个本地源状态 = %+v，期望 1 份且已同步", statuses[1])
	}
	if statuses[2].Kind != SourceBuiltin || statuses[2].Count != 3 || !statuses[2].Synced {
		t.Errorf("内置源状态 = %+v，期望 3 份且视为已同步", statuses[2])
	}
}

// TestLoadSourcesRemoteUnsyncedIsNotError 覆盖「远端源尚未同步」：
// 不记为错误，只在 SourceStatus 里标出未同步，其余来源照常加载。
func TestLoadSourcesRemoteUnsyncedIsNotError(t *testing.T) {
	syncer := &Syncer{StateDir: t.TempDir()}
	sources := []Source{{Kind: SourceRemote, URL: "https://example.com/nova/profiles"}}

	registry, statuses, err := LoadSources(sources, syncer)
	if err != nil {
		t.Fatalf("未同步不应让装配失败：%v", err)
	}
	if len(statuses) != 1 {
		t.Fatalf("状态 %d 条，期望 1 条", len(statuses))
	}
	status := statuses[0]
	if status.Err != nil {
		t.Errorf("未同步不应记为错误，实际 Err = %v", status.Err)
	}
	if status.Synced {
		t.Errorf("未同步的源 Synced 应为 false")
	}
	if status.Count != 0 {
		t.Errorf("未同步的源 Count = %d，期望 0", status.Count)
	}
	// 基底不受远端源状态影响。
	if _, ok := registry.Lookup("opencode-go"); !ok {
		t.Errorf("注册表里应保留内置档案 opencode-go")
	}
}

// TestLoadSourcesRemoteReadFailureKeepsOtherSources 覆盖「远端源读失败」：
// 失败只进 SourceStatus.Err，不中止装配，其余来源仍加载。
func TestLoadSourcesRemoteReadFailureKeepsOtherSources(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stateDir, "state.json"), []byte("不是 JSON"), 0o600); err != nil {
		t.Fatalf("写入损坏状态文件失败：%v", err)
	}
	local := writeLocalSource(t, "a.yaml", sourceProfile("local-only", "本地档案"))

	syncer := &Syncer{StateDir: stateDir}
	sources := []Source{
		{Kind: SourceRemote, URL: "https://example.com/nova/profiles"},
		{Kind: SourceLocal, Path: local},
	}

	registry, statuses, err := LoadSources(sources, syncer)
	if err != nil {
		t.Fatalf("源读失败不应让装配失败：%v", err)
	}
	if len(statuses) != 2 {
		t.Fatalf("状态 %d 条，期望 2 条", len(statuses))
	}
	if statuses[0].Err == nil {
		t.Errorf("远端源读失败应记进 SourceStatus.Err")
	}
	if statuses[0].Synced {
		t.Errorf("读失败的源 Synced 应为 false")
	}
	// 失败源后面的源照常加载。
	if statuses[1].Err != nil {
		t.Errorf("失败源之后的本地源不应受影响，实际 Err = %v", statuses[1].Err)
	}
	if _, ok := registry.Lookup("local-only"); !ok {
		t.Errorf("失败源之后的本地档案应仍在注册表里")
	}
	if _, ok := registry.Lookup("opencode-go"); !ok {
		t.Errorf("注册表里应保留内置档案 opencode-go")
	}
}
