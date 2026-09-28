package keystore

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
	if names := store.Names("commandcode"); len(names) != 0 {
		t.Errorf("账号 = %v，期望空", names)
	}
	if _, ok := store.Lookup("commandcode", "default"); ok {
		t.Error("空存储里不该查到账号")
	}
	if ids := store.Namespaces(); len(ids) != 0 {
		t.Errorf("命名空间 = %v，期望空", ids)
	}
}

// 写入后重新读取要拿到同样的密钥，且目录 0700、文件 0600。
func TestSetLoadRoundTripAndPermissions(t *testing.T) {
	dir := t.TempDir()
	store, err := Load(dir)
	if err != nil {
		t.Fatalf("加载空存储失败：%v", err)
	}
	if err := store.Set("commandcode", "work", "sk-1234567890"); err != nil {
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
	key, ok := reloaded.Lookup("commandcode", "work")
	if !ok || key != "sk-1234567890" {
		t.Errorf("取到的密钥 = %q, %v，期望 sk-1234567890", key, ok)
	}
}

// 目录已存在且权限较宽时，写入会把它收紧到 0700。
func TestSaveTightensExistingDirPermissions(t *testing.T) {
	dir := t.TempDir()
	novaDir := filepath.Join(dir, "nova")
	if err := os.MkdirAll(novaDir, 0o755); err != nil {
		t.Fatalf("创建目录失败：%v", err)
	}
	if err := os.Chmod(novaDir, 0o755); err != nil {
		t.Fatalf("设置目录权限失败：%v", err)
	}

	store, err := Load(dir)
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	if err := store.Set("p", "a", "k"); err != nil {
		t.Fatalf("写入失败：%v", err)
	}

	info, err := os.Stat(novaDir)
	if err != nil {
		t.Fatalf("读取目录失败：%v", err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("目录权限 = %o，期望 700", got)
	}
}

// 不写账号名时取 default 账号，且覆盖写入会刷新加入时间。
func TestLookupDefaultAccountAndOverwrite(t *testing.T) {
	store, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	if err := store.Set("commandcode", "work", "sk-first"); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	if _, ok := store.Lookup("commandcode", ""); ok {
		t.Error("没有 default 账号时不该按空名字取到 work")
	}
	if err := store.Set("commandcode", DefaultAccount, "sk-default"); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	key, ok := store.Lookup("commandcode", "")
	if !ok || key != "sk-default" {
		t.Errorf("空名字取到的密钥 = %q, %v，期望 sk-default", key, ok)
	}

	if err := store.Set("commandcode", DefaultAccount, "sk-replaced"); err != nil {
		t.Fatalf("覆盖写入失败：%v", err)
	}
	entries := store.Entries("commandcode")
	if len(entries) != 2 {
		t.Fatalf("账号数 = %d，期望 2（覆盖不新增）", len(entries))
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

// All 的输出顺序稳定：命名空间按字典序。
func TestAllOrdersNamespaces(t *testing.T) {
	store, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	if err := store.Set("zeta", "a", "k1"); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	if err := store.Set("alpha", "b", "k2"); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	got := store.All()
	if len(got) != 2 || got[0].Namespace != "alpha" || got[1].Namespace != "zeta" {
		t.Errorf("All() = %v，期望 alpha 在前", got)
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

// Remove 支持删一个与删整个命名空间；删不存在的目标不报错。
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
		t.Fatalf("删除整个命名空间失败：%v", err)
	}
	if ids := store.Namespaces(); len(ids) != 0 {
		t.Errorf("命名空间 = %v，期望已清空", ids)
	}
	if err := store.Remove("nope", ""); err != nil {
		t.Errorf("删除不存在的命名空间不该报错：%v", err)
	}
}

// Mask：长密钥给前 12 后 8，短一些的给前 6 后 6，中间固定四个星号；
// 连头尾两段都盖不住时整体遮住。
func TestMask(t *testing.T) {
	tests := []struct {
		name string
		key  string
		want string
	}{
		{"长密钥取前 12 后 8", "sk-1234567890abcdefghijklmnopqrstuvwxyz", "sk-123456789****stuvwxyz"},
		{"刚好 21 位仍取前 12 后 8", "abcdefghijklmnopqrstu", "abcdefghijkl****nopqrstu"},
		{"20 位退到前 6 后 6", "abcdefghijklmnopqrst", "abcdef****opqrst"},
		{"13 位仍取前 6 后 6", "abcdefghijklm", "abcdef****hijklm"},
		{"12 位整体遮住", "abcdefghijkl", strings.Repeat("*", 12)},
		{"短密钥整体遮住", "abcd", "****"},
		{"两字符整体遮住", "ab", "**"},
		{"空密钥", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Mask(tt.key); got != tt.want {
				t.Errorf("Mask(%q) = %q，期望 %q", tt.key, got, tt.want)
			}
		})
	}
}
