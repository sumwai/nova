package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// providerBlock 造一段最小但合法的 provider 配置，供 import 测试当被导入内容。
func providerBlock(name, url string) string {
	return "provider " + name + " {\n" +
		"    api_key k\n" +
		"    url " + url + "\n" +
		"    model m\n" +
		"}\n"
}

func TestImportExpandsInline(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "Novafile")
	writeFile(t, main, "version 1\nimport extra\n")
	writeFile(t, filepath.Join(dir, "extra"), providerBlock("a", "https://a.example.com/v1/chat/completions"))

	cfg, err := Load(main)
	if err != nil {
		t.Fatalf("Load 意外失败：%v", err)
	}
	if len(cfg.Providers) != 1 || cfg.Providers[0].Name != "a" {
		t.Fatalf("provider = %+v，期望只有被导入的那一个", cfg.Providers)
	}
}

// glob 命中多个文件时按文件名字典序拼接：同名模型在这些文件之间的回退顺序
// 因此由文件名决定，而不是由文件系统返回目录项的顺序决定。
func TestImportGlobFollowsFileOrder(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "Novafile")
	writeFile(t, main, "version 1\nimport ch-*\n")
	writeFile(t, filepath.Join(dir, "ch-b"), providerBlock("b", "https://b.example.com/v1/chat/completions"))
	writeFile(t, filepath.Join(dir, "ch-a"), providerBlock("a", "https://a.example.com/v1/chat/completions"))

	cfg, err := Load(main)
	if err != nil {
		t.Fatalf("Load 意外失败：%v", err)
	}
	if len(cfg.Providers) != 2 {
		t.Fatalf("provider 数 = %d，期望 2", len(cfg.Providers))
	}
	if cfg.Providers[0].Name != "a" || cfg.Providers[1].Name != "b" {
		t.Errorf("拼接顺序 = %s / %s，期望按文件名字典序",
			cfg.Providers[0].Name, cfg.Providers[1].Name)
	}
}

// glob 没命中只警告：拆出来的目录可能只是暂时为空，为它让网关起不来太重。
func TestImportGlobWithoutMatchOnlyWarns(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "Novafile")
	writeFile(t, main, "version 1\nimport missing-*\n")

	cfg, err := Load(main)
	if err != nil {
		t.Fatalf("glob 没命中不该让加载失败：%v", err)
	}
	if !hasWarningAbout(cfg, "没有命中任何文件") {
		t.Errorf("缺少「没有命中」的提醒：%v", cfg.Warnings)
	}
}

// 模式里没有通配符时它指向一个具体文件，读不到就是配置写错了。
func TestImportWithoutGlobMustMatch(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "Novafile")
	writeFile(t, main, "version 1\nimport 并不存在\n")

	_, err := Load(main)
	if err == nil {
		t.Fatal("期望报错，但加载成功了")
	}
	if !strings.Contains(err.Error(), "没有命中文件") {
		t.Errorf("消息 = %q，期望说明具体文件必须命中", err.Error())
	}
}

func TestImportCycleIsRejected(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	writeFile(t, a, "version 1\nimport b\n")
	writeFile(t, b, "import a\n")

	_, err := Load(a)
	if err == nil {
		t.Fatal("期望检出环形导入，但加载成功了")
	}
	if !strings.Contains(err.Error(), "成环") {
		t.Errorf("消息 = %q，期望说明成环", err.Error())
	}
}

func TestImportDirectoryIsRejected(t *testing.T) {
	dir := t.TempDir()
	channels := filepath.Join(dir, "channels")
	if err := os.MkdirAll(channels, 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	main := filepath.Join(dir, "Novafile")
	writeFile(t, main, "version 1\nimport channels\n")

	_, err := Load(main)
	if err == nil {
		t.Fatal("期望拒绝导入目录，但它成功了")
	}
	if !strings.Contains(err.Error(), "是一个目录") {
		t.Errorf("消息 = %q，期望说明这是目录", err.Error())
	}
}

func TestImportRejectsUnsupportedPatterns(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		wantMsg string
	}{
		// 字符组的含义随 locale 与 shell 配置而变，而配置文件的含义不该取决于运行环境。
		{"字符组", "ch-[ab]", "字符组不支持"},
		{"两个通配符", "a*b*", "至多一个"},
		{"模式为空", `""`, "不能为空"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			main := filepath.Join(dir, "Novafile")
			writeFile(t, main, "version 1\nimport "+tt.pattern+"\n")

			_, err := Load(main)
			if err == nil {
				t.Fatal("期望报错，但加载成功了")
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("消息 = %q，期望包含 %q", err.Error(), tt.wantMsg)
			}
		})
	}
}

// 相对路径相对于写下这一行的那份文件，而不是主配置所在的目录，
// 也不是进程的工作目录：渠道文件自己再拆一层时不会跑偏。
func TestImportPathIsRelativeToImportingFile(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	main := filepath.Join(dir, "Novafile")
	writeFile(t, main, "version 1\nimport sub/one\n")
	writeFile(t, filepath.Join(sub, "one"), "import two\n")
	writeFile(t, filepath.Join(sub, "two"), providerBlock("a", "https://a.example.com/v1/chat/completions"))

	cfg, err := Load(main)
	if err != nil {
		t.Fatalf("Load 意外失败：%v", err)
	}
	if len(cfg.Providers) != 1 {
		t.Fatalf("provider 数 = %d，期望 1（sub/one 里的 import two 应解析成 sub/two）",
			len(cfg.Providers))
	}
}

// 被导入文件里的错误要指回那个文件，而不是主配置的某个行号。
func TestImportErrorPointsAtImportedFile(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "Novafile")
	extra := filepath.Join(dir, "extra")
	writeFile(t, main, "version 1\nimport extra\n")
	writeFile(t, extra, "provider a {\n"+
		"    api_key k\n"+
		"    url https://a.example.com/v1/chat/completions\n"+
		"    model m\n"+
		"    log_level debug\n"+
		"}\n")

	_, err := Load(main)
	cerr := configError(t, err)
	if cerr.File != extra {
		t.Errorf("错误文件 = %q，期望 %q", cerr.File, extra)
	}
	if cerr.Line != 5 {
		t.Errorf("错误行号 = %d，期望 5（被导入文件里 log_level 那一行）", cerr.Line)
	}
}

func TestImportEmptyFileOnlyWarns(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "Novafile")
	writeFile(t, main, "version 1\nimport extra\n")
	writeFile(t, filepath.Join(dir, "extra"), "\n# 只有注释\n\n")

	cfg, err := Load(main)
	if err != nil {
		t.Fatalf("空文件不该让加载失败：%v", err)
	}
	if !hasWarningAbout(cfg, "空的") {
		t.Errorf("缺少「文件是空的」提醒：%v", cfg.Warnings)
	}
}

// import 拼接进来的内容参与同一次语法分析：它声明的渠道与主配置里写的完全等价。
func TestImportedProvidersJoinSelectionOrder(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "Novafile")
	writeFile(t, main, "version 1\n"+providerBlock("inline", "https://inline.example.com/v1/chat/completions")+"import extra\n")
	writeFile(t, filepath.Join(dir, "extra"), providerBlock("imported", "https://imported.example.com/v1/chat/completions"))

	cfg, err := Load(main)
	if err != nil {
		t.Fatalf("Load 意外失败：%v", err)
	}
	routes := cfg.Routes("m")
	if len(routes) != 2 {
		t.Fatalf("候选数 = %d，期望 2（主配置里写的与导入的合并选路）", len(routes))
	}
	if routes[0].Provider != "inline" || routes[1].Provider != "imported" {
		t.Errorf("候选顺序 = %s / %s，期望按拼接后的声明位置",
			routes[0].Provider, routes[1].Provider)
	}
}
