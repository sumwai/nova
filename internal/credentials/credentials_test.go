package credentials

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 文件不存在是「还没登录过」，不是错误。
func TestLoadMissingFileIsEmpty(t *testing.T) {
	store, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("文件不存在应视为空存储：%v", err)
	}
	if names := store.Names("opencode-go"); len(names) != 0 {
		t.Errorf("账号 = %v，期望空", names)
	}
	if _, ok := store.Lookup("opencode-go", "default"); ok {
		t.Error("空存储里不该查到账号")
	}
	if ids := store.ProfileIDs(); len(ids) != 0 {
		t.Errorf("档案 = %v，期望空", ids)
	}
}

// 写入后重新读取要拿到同样的密钥，且目录 0700、文件 0600。
func TestSetLoadRoundTripAndPermissions(t *testing.T) {
	dir := t.TempDir()
	store, err := Load(dir)
	if err != nil {
		t.Fatalf("加载空存储失败：%v", err)
	}
	if err := store.Set("opencode-go", "work", "sk-1234567890"); err != nil {
		t.Fatalf("写入失败：%v", err)
	}

	path := filepath.Join(dir, "nova", "credentials.json")
	if store.Path() != path {
		t.Errorf("路径 = %q，期望 %q", store.Path(), path)
	}
	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("凭据库文件未写出：%v", err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Errorf("文件权限 = %o，期望 600", got)
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("凭据库目录未写出：%v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("目录权限 = %o，期望 700", got)
	}

	reloaded, err := Load(dir)
	if err != nil {
		t.Fatalf("重新加载失败：%v", err)
	}
	key, ok := reloaded.Lookup("opencode-go", "work")
	if !ok || key != "sk-1234567890" {
		t.Errorf("取到的密钥 = %q, %v，期望 sk-1234567890", key, ok)
	}
}

// 账号顺序按加入顺序，不按名字：报错与选路都要一个确定的「第一个账号」。
func TestNamesKeepInsertionOrder(t *testing.T) {
	store, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	for _, name := range []string{"zeta", "alpha"} {
		if err := store.Set("p", name, "k-"+name); err != nil {
			t.Fatalf("写入 %s 失败：%v", name, err)
		}
	}
	if got := store.Names("p"); len(got) != 2 || got[0] != "zeta" || got[1] != "alpha" {
		t.Errorf("账号顺序 = %v，期望 [zeta alpha]", got)
	}
}

// 文件损坏时报错并带路径，不静默当成空存储。
func TestLoadCorruptFileReportsPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nova", "credentials.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("创建目录失败：%v", err)
	}
	if err := os.WriteFile(path, []byte("{ 这不是 JSON"), 0o600); err != nil {
		t.Fatalf("写入损坏文件失败：%v", err)
	}

	_, err := Load(dir)
	if err == nil {
		t.Fatal("损坏的凭据库应报错")
	}
	var cerr *Error
	if !errors.As(err, &cerr) {
		t.Fatalf("错误类型 = %T，期望 *Error", err)
	}
	if cerr.File != path || !strings.Contains(err.Error(), path) {
		t.Errorf("错误 = %v，期望带路径 %s", err, path)
	}
}

// 高于本二进制的格式代数直接拒绝，而不是尽力解析。
func TestLoadRejectsNewerVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nova", "credentials.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("创建目录失败：%v", err)
	}
	if err := os.WriteFile(path, []byte(`{"version":2,"accounts":{}}`), 0o600); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "代数") {
		t.Errorf("错误 = %v，期望拒绝更高的代数", err)
	}
}

// Remove 支持删一个与删整个档案；删不存在的目标不报错。
func TestRemove(t *testing.T) {
	store, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	if err := store.Set("p", "a", "ka"); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	if err := store.Set("p", "b", "kb"); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	if err := store.Remove("p", "a"); err != nil {
		t.Fatalf("删除一个账号失败：%v", err)
	}
	if _, ok := store.Lookup("p", "a"); ok {
		t.Error("账号 a 应已删除")
	}
	if err := store.Remove("p", "missing"); err != nil {
		t.Errorf("删除不存在的账号不该报错：%v", err)
	}
	if err := store.Remove("p", ""); err != nil {
		t.Fatalf("删除整个档案失败：%v", err)
	}
	if ids := store.ProfileIDs(); len(ids) != 0 {
		t.Errorf("档案 = %v，期望已清空", ids)
	}
	if err := store.Remove("nope", ""); err != nil {
		t.Errorf("删除不存在的档案不该报错：%v", err)
	}
}

// Suffix 只暴露尾四位；短密钥整体遮住。
func TestSuffix(t *testing.T) {
	tests := []struct {
		key  string
		want string
	}{
		{"sk-1234567890", "…7890"},
		{"abcd", "****"},
		{"ab", "**"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := Suffix(tt.key); got != tt.want {
			t.Errorf("Suffix(%q) = %q，期望 %q", tt.key, got, tt.want)
		}
	}
}
