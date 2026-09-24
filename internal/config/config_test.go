package config

import (
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestLoadReportsUnreadableFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "并不存在的文件")

	_, err := Load(missing)
	if err == nil {
		t.Fatal("期望 Load 报错，但它成功了")
	}
	var cerr *Error
	if !errors.As(err, &cerr) {
		t.Fatalf("错误类型是 %T，期望 *Error", err)
	}
	// 读取失败也带文件名、但不指向某一行：这样调用方只需处理一种错误类型。
	if cerr.File != missing {
		t.Errorf("文件名 = %q，期望 %q", cerr.File, missing)
	}
	if cerr.Line != 0 {
		t.Errorf("行号 = %d，期望 0（文件级错误不指向某一行）", cerr.Line)
	}
	if !strings.Contains(cerr.Error(), missing+":") {
		t.Errorf("排版 = %q，期望带文件前缀", cerr.Error())
	}
}

func TestLoadReadsRealFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Novafile")
	writeFile(t, path, "version 1\nlisten :9090\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load 意外失败：%v", err)
	}
	if cfg.Path != path {
		t.Errorf("Path = %q，期望 %q", cfg.Path, path)
	}
	if cfg.Listen != ":9090" {
		t.Errorf("监听地址 = %q，期望 :9090", cfg.Listen)
	}
}

func TestInstructionNamesIsSortedAndDeduplicated(t *testing.T) {
	names := InstructionNames()

	if !sort.StringsAreSorted(names) {
		t.Errorf("指令清单未排序：%v", names)
	}
	seen := map[string]bool{}
	for _, name := range names {
		if seen[name] {
			t.Errorf("指令清单有重复项：%q", name)
		}
		seen[name] = true
	}
	for _, want := range []string{directiveVersion, directiveLogLevel, directiveListen, directiveAdmin} {
		if !seen[want] {
			t.Errorf("指令清单漏掉了 %q：%v", want, names)
		}
	}
}

// 每次返回新切片：调用方（例如版本输出的排版层）改它不得影响解析行为。
func TestInstructionNamesReturnsFreshCopy(t *testing.T) {
	first := InstructionNames()
	if len(first) == 0 {
		t.Fatal("指令清单为空，测试前提不成立")
	}
	first[0] = "被调用方改掉的"

	if got := InstructionNames()[0]; got == "被调用方改掉的" {
		t.Error("两次调用共享了底层数组：调用方改返回值污染了解析器的数据源")
	}
}

func TestSupportedSchemas(t *testing.T) {
	schemas := SupportedSchemas()

	if len(schemas) != CurrentSchema-MinSchema+1 {
		t.Fatalf("代数个数 = %d，期望 %d", len(schemas), CurrentSchema-MinSchema+1)
	}
	if schemas[0] != MinSchema || schemas[len(schemas)-1] != CurrentSchema {
		t.Errorf("代数区间 = %v，期望从 %d 到 %d", schemas, MinSchema, CurrentSchema)
	}

	schemas[0] = -1
	if SupportedSchemas()[0] != MinSchema {
		t.Error("两次调用共享了底层数组")
	}
}
