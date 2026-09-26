package profile

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// futureTime 是一个不可能过期的 not_after，供不需要覆盖到期分支的用例使用。
const futureTime = "2999-01-01T00:00:00Z"

// sampleProfile 造一份合法档案，id 可控，用于构造多档案的快照。
func sampleProfile(id string) []byte {
	return []byte("schema: 1\nid: " + id +
		"\nendpoints:\n  openai_chat:\n    url: https://example.com/v1/chat/completions\n    protocol: openai_chat\n")
}

// testKey 是一对测试用 ed25519 密钥。
type testKey struct {
	name string
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func newTestKey(t *testing.T, name string) testKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("生成 ed25519 密钥失败：%v", err)
	}
	return testKey{name: name, pub: pub, priv: priv}
}

func (k testKey) public() PublicKey { return PublicKey{Name: k.name, Key: k.pub} }

func signIndex(priv ed25519.PrivateKey, raw []byte) []byte {
	return []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, raw)) + "\n")
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// remoteFixture 是一个可变的远端源：文件内容与重定向目标都能在测试中替换。
type remoteFixture struct {
	mu       sync.Mutex
	files    map[string][]byte
	redirect map[string]string
}

func newFixture() *remoteFixture {
	return &remoteFixture{files: map[string][]byte{}, redirect: map[string]string{}}
}

func (f *remoteFixture) set(path string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[path] = data
}

func (f *remoteFixture) redirectTo(path, location string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.redirect[path] = location
}

func (f *remoteFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	location, isRedirect := f.redirect[r.URL.Path]
	data, ok := f.files[r.URL.Path]
	f.mu.Unlock()

	if isRedirect {
		http.Redirect(w, r, location, http.StatusFound)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	_, _ = w.Write(data)
}

// indexSpec 是构造索引 YAML 的一条声明。
type indexSpec struct {
	id     string
	path   string
	sha256 string
}

// indexYAML 按索引 schema 拼出一份索引文本；not_after 加引号，避免被 YAML 解析成时间戳。
func indexYAML(serial int64, notAfter string, entries []indexSpec) []byte {
	var b strings.Builder
	b.WriteString("$schema: https://nova.dev/schema/index-1.yaml\n")
	b.WriteString("schema: 1\n")
	b.WriteString("name: test source\n")
	fmt.Fprintf(&b, "serial: %d\n", serial)
	fmt.Fprintf(&b, "not_after: %q\n", notAfter)
	if len(entries) == 0 {
		b.WriteString("profiles: []\n")
		return []byte(b.String())
	}
	b.WriteString("profiles:\n")
	for _, entry := range entries {
		fmt.Fprintf(&b, "  - {id: %s, path: %s, sha256: %q}\n", entry.id, entry.path, entry.sha256)
	}
	return []byte(b.String())
}

// fixtureProfile 是一份发布到远端源的档案。
type fixtureProfile struct {
	id   string
	path string
	data []byte
}

// publish 发布一个版本：写入档案文件、索引与签名。
func publish(t *testing.T, fixture *remoteFixture, key testKey, serial int64, notAfter string, profiles []fixtureProfile) {
	t.Helper()
	entries := make([]indexSpec, 0, len(profiles))
	for _, p := range profiles {
		entries = append(entries, indexSpec{id: p.id, path: p.path, sha256: sha256Hex(p.data)})
		fixture.set("/"+p.path, p.data)
	}
	publishRaw(fixture, key, indexYAML(serial, notAfter, entries))
}

// publishRaw 用给定索引原文发布一个版本，供构造 sha 不符等异常索引。
func publishRaw(fixture *remoteFixture, key testKey, raw []byte) {
	fixture.set("/index.yaml", raw)
	fixture.set("/index.yaml.sig", signIndex(key.priv, raw))
}

func requireErrorContains(t *testing.T, err error, wants ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望失败，实际成功")
	}
	for _, want := range wants {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误 %q 应含 %q", err.Error(), want)
		}
	}
}

func readStateForTest(t *testing.T, stateDir string) stateFile {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(stateDir, "state.json"))
	if err != nil {
		t.Fatalf("读取状态文件失败：%v", err)
	}
	var state stateFile
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("解析状态文件失败：%v", err)
	}
	return state
}

func assertNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("路径 %s 应不存在，实际 err = %v", path, err)
	}
}

// snapshotPath 返回某个远端源在状态目录下的快照路径；name 为 "1" 或 "1.tmp"。
func snapshotPath(stateDir, sourceURL, name string) string {
	return filepath.Join(stateDir, "profiles", snapshotNamespace(normalizeURL(sourceURL)), name)
}

func TestVerifyIndexAcceptsSignatureForms(t *testing.T) {
	key := newTestKey(t, "main")
	raw := []byte("schema: 1\n")
	signature := ed25519.Sign(key.priv, raw)

	forms := map[string][]byte{}
	forms["原始 64 字节"] = ed25519.Sign(key.priv, raw)
	forms["标准 base64"] = []byte(base64.StdEncoding.EncodeToString(signature))
	forms["URL base64"] = []byte(base64.URLEncoding.EncodeToString(signature))
	forms["末尾换行"] = []byte(base64.StdEncoding.EncodeToString(signature) + "\n")
	forms["省略填充"] = []byte(base64.RawStdEncoding.EncodeToString(signature))
	for formName, form := range forms {
		t.Run(formName, func(t *testing.T) {
			name, err := VerifyIndex(raw, form, []PublicKey{key.public()})
			if err != nil {
				t.Fatalf("合法签名应通过，实际失败：%v", err)
			}
			if name != "main" {
				t.Errorf("命中公钥名 = %q，期望 main", name)
			}
		})
	}
}

func TestVerifyIndexRejectsWrongSignature(t *testing.T) {
	key := newTestKey(t, "main")
	other := newTestKey(t, "other")
	raw := []byte("schema: 1\n")
	signature := ed25519.Sign(key.priv, raw)

	t.Run("签名被换", func(t *testing.T) {
		tampered := append(append([]byte{}, raw...), '\n')
		_, err := VerifyIndex(tampered, []byte(base64.StdEncoding.EncodeToString(signature)), []PublicKey{key.public()})
		requireErrorContains(t, err, "签名", "不匹配")
	})

	t.Run("用未登记密钥签", func(t *testing.T) {
		_, err := VerifyIndex(raw, signIndex(other.priv, raw), []PublicKey{key.public()})
		requireErrorContains(t, err, "签名", "不匹配")
	})

	t.Run("公钥列表为空", func(t *testing.T) {
		_, err := VerifyIndex(raw, signature, nil)
		requireErrorContains(t, err, "未登记公钥")
	})

	t.Run("签名不是 base64", func(t *testing.T) {
		_, err := VerifyIndex(raw, []byte("!!! not base64 !!!"), []PublicKey{key.public()})
		requireErrorContains(t, err, "base64")
	})
}

func TestResolveRemoteInstallsSnapshot(t *testing.T) {
	key := newTestKey(t, "main")
	fixture := newFixture()
	server := httptest.NewTLSServer(fixture)
	defer server.Close()

	content := sampleProfile("sample")
	publish(t, fixture, key, 7, futureTime, []fixtureProfile{
		{id: "sample", path: "providers/sample.yaml", data: content},
	})

	stateDir := t.TempDir()
	syncer := &Syncer{StateDir: stateDir, Client: server.Client()}
	snap, warnings, err := syncer.Resolve(context.Background(), Source{
		Kind: SourceRemote, URL: server.URL, Keys: []PublicKey{key.public()},
	})
	if err != nil {
		t.Fatalf("同步应成功，实际失败：%v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("正常同步不应有提醒，实际为 %v", warnings)
	}
	if snap.Serial != 7 {
		t.Errorf("serial = %d，期望 7", snap.Serial)
	}
	if len(snap.Profiles) != 1 || snap.Profiles[0].ID != "sample" {
		t.Errorf("档案集合不正确：%+v", snap.Profiles)
	}
	if snap.Index == nil || snap.Index.Name != "test source" {
		t.Errorf("索引未随快照返回：%+v", snap.Index)
	}
	got, readErr := os.ReadFile(filepath.Join(snap.Root, "providers", "sample.yaml"))
	if readErr != nil {
		t.Fatalf("已装快照不可读：%v", readErr)
	}
	if string(got) != string(content) {
		t.Errorf("已装档案内容不一致")
	}

	state := readStateForTest(t, stateDir)
	entry, ok := state.Sources[normalizeURL(server.URL)]
	if !ok {
		t.Fatalf("状态里没有 %q：%+v", normalizeURL(server.URL), state.Sources)
	}
	if entry.Serial != 7 {
		t.Errorf("状态 serial = %d，期望 7", entry.Serial)
	}
	if entry.Root != snap.Root {
		t.Errorf("状态 root = %q，期望 %q", entry.Root, snap.Root)
	}
}

func TestResolveVerifiesBeforeParsing(t *testing.T) {
	key := newTestKey(t, "main")
	other := newTestKey(t, "other")
	fixture := newFixture()
	server := httptest.NewTLSServer(fixture)
	defer server.Close()

	// 索引本身不是合法 YAML，但用未登记密钥签了名。
	badIndex := []byte("schema: [1, 2\n")
	fixture.set("/index.yaml", badIndex)
	fixture.set("/index.yaml.sig", signIndex(other.priv, badIndex))

	syncer := &Syncer{StateDir: t.TempDir(), Client: server.Client()}
	_, _, err := syncer.Resolve(context.Background(), Source{
		Kind: SourceRemote, URL: server.URL, Keys: []PublicKey{key.public()},
	})
	requireErrorContains(t, err, "签名")
	if strings.Contains(err.Error(), "YAML") {
		t.Errorf("验签失败时不得进入解析：%v", err)
	}
}

func TestResolveRejectsSerialRollback(t *testing.T) {
	key := newTestKey(t, "main")
	fixture := newFixture()
	server := httptest.NewTLSServer(fixture)
	defer server.Close()

	stateDir := t.TempDir()
	syncer := &Syncer{StateDir: stateDir, Client: server.Client()}
	src := Source{Kind: SourceRemote, URL: server.URL, Keys: []PublicKey{key.public()}}

	publish(t, fixture, key, 5, futureTime, []fixtureProfile{
		{id: "sample", path: "providers/sample.yaml", data: sampleProfile("sample")},
	})
	if _, _, err := syncer.Resolve(context.Background(), src); err != nil {
		t.Fatalf("首次同步应成功：%v", err)
	}

	t.Run("同 serial", func(t *testing.T) {
		_, _, err := syncer.Resolve(context.Background(), src)
		requireErrorContains(t, err, "回滚", "不大于")
	})

	t.Run("更低 serial", func(t *testing.T) {
		publish(t, fixture, key, 4, futureTime, []fixtureProfile{
			{id: "sample", path: "providers/sample.yaml", data: sampleProfile("sample")},
		})
		_, _, err := syncer.Resolve(context.Background(), src)
		requireErrorContains(t, err, "回滚")
	})
}

func TestResolveRejectsExpiredIndexKeepsInstalled(t *testing.T) {
	key := newTestKey(t, "main")
	fixture := newFixture()
	server := httptest.NewTLSServer(fixture)
	defer server.Close()

	stateDir := t.TempDir()
	syncer := &Syncer{StateDir: stateDir, Client: server.Client()}
	src := Source{Kind: SourceRemote, URL: server.URL, Keys: []PublicKey{key.public()}}

	content := sampleProfile("sample")
	publish(t, fixture, key, 1, futureTime, []fixtureProfile{
		{id: "sample", path: "providers/sample.yaml", data: content},
	})
	first, _, err := syncer.Resolve(context.Background(), src)
	if err != nil {
		t.Fatalf("首次同步应成功：%v", err)
	}

	publish(t, fixture, key, 2, "2000-01-01T00:00:00Z", []fixtureProfile{
		{id: "sample", path: "providers/sample.yaml", data: sampleProfile("sample")},
	})
	snap, warnings, err := syncer.Resolve(context.Background(), src)
	requireErrorContains(t, err, "本次更新被拒", "not_after")
	if snap != nil {
		t.Errorf("拒绝更新时不应返回新快照")
	}
	if len(warnings) != 1 {
		t.Fatalf("过期索引应给出一条提醒，实际 %v", warnings)
	}
	if !strings.Contains(warnings[0].Msg, "已装快照保持可用") {
		t.Errorf("提醒文案应说明已装快照仍可用：%q", warnings[0].Msg)
	}

	// 已装快照仍可读，状态未推进。
	got, readErr := os.ReadFile(filepath.Join(first.Root, "providers", "sample.yaml"))
	if readErr != nil {
		t.Fatalf("已装快照应仍可读：%v", readErr)
	}
	if string(got) != string(content) {
		t.Errorf("已装快照内容被改动")
	}
	if serial := readStateForTest(t, stateDir).Sources[normalizeURL(server.URL)].Serial; serial != 1 {
		t.Errorf("状态 serial = %d，期望仍为 1", serial)
	}
}

func TestResolveRejectsSHA256Mismatch(t *testing.T) {
	key := newTestKey(t, "main")
	fixture := newFixture()
	server := httptest.NewTLSServer(fixture)
	defer server.Close()

	fixture.set("/providers/sample.yaml", sampleProfile("sample"))
	publishRaw(fixture, key, indexYAML(1, futureTime, []indexSpec{
		{id: "sample", path: "providers/sample.yaml", sha256: strings.Repeat("0", 64)},
	}))

	stateDir := t.TempDir()
	syncer := &Syncer{StateDir: stateDir, Client: server.Client()}
	_, _, err := syncer.Resolve(context.Background(), Source{
		Kind: SourceRemote, URL: server.URL, Keys: []PublicKey{key.public()},
	})
	requireErrorContains(t, err, "sha256")

	assertNotExist(t, snapshotPath(stateDir, server.URL, "1"))
	assertNotExist(t, snapshotPath(stateDir, server.URL, "1.tmp"))
}

func TestResolveRejectsInvalidProfile(t *testing.T) {
	key := newTestKey(t, "main")
	fixture := newFixture()
	server := httptest.NewTLSServer(fixture)
	defer server.Close()

	bad := []byte("schema: 1\nid: sample\nbogus_field: true\nendpoints:\n  openai_chat:\n    url: https://example.com/v1/chat/completions\n    protocol: openai_chat\n")
	publish(t, fixture, key, 1, futureTime, []fixtureProfile{
		{id: "sample", path: "providers/sample.yaml", data: bad},
	})

	stateDir := t.TempDir()
	syncer := &Syncer{StateDir: stateDir, Client: server.Client()}
	_, _, err := syncer.Resolve(context.Background(), Source{
		Kind: SourceRemote, URL: server.URL, Keys: []PublicKey{key.public()},
	})
	requireErrorContains(t, err, "未知字段", "bogus_field")

	var profileErr *Error
	if !errors.As(err, &profileErr) {
		t.Fatalf("错误应为 *profile.Error，实际 %T", err)
	}
	if profileErr.Line <= 0 {
		t.Errorf("档案错误应带行号，实际 Line = %d", profileErr.Line)
	}

	assertNotExist(t, snapshotPath(stateDir, server.URL, "1"))
	assertNotExist(t, snapshotPath(stateDir, server.URL, "1.tmp"))
}

func TestResolveRejectsDownloadedSetMismatch(t *testing.T) {
	key := newTestKey(t, "main")
	fixture := newFixture()
	server := httptest.NewTLSServer(fixture)
	defer server.Close()

	// 两条声明经路径归一后落到同一个文件，实际集合比声明少一个。
	content := sampleProfile("a")
	sum := sha256Hex(content)
	fixture.set("/providers/a.yaml", content)
	fixture.set("/providers//a.yaml", content)
	publishRaw(fixture, key, indexYAML(1, futureTime, []indexSpec{
		{id: "a", path: "providers/a.yaml", sha256: sum},
		{id: "b", path: "providers//a.yaml", sha256: sum},
	}))

	stateDir := t.TempDir()
	syncer := &Syncer{StateDir: stateDir, Client: server.Client()}
	_, _, err := syncer.Resolve(context.Background(), Source{
		Kind: SourceRemote, URL: server.URL, Keys: []PublicKey{key.public()},
	})
	requireErrorContains(t, err, "完全相等")
	assertNotExist(t, snapshotPath(stateDir, server.URL, "1.tmp"))
}

func TestResolveAtomicOnSecondFailure(t *testing.T) {
	key := newTestKey(t, "main")
	fixture := newFixture()
	server := httptest.NewTLSServer(fixture)
	defer server.Close()

	stateDir := t.TempDir()
	syncer := &Syncer{StateDir: stateDir, Client: server.Client()}
	src := Source{Kind: SourceRemote, URL: server.URL, Keys: []PublicKey{key.public()}}

	content := sampleProfile("sample")
	publish(t, fixture, key, 1, futureTime, []fixtureProfile{
		{id: "sample", path: "providers/sample.yaml", data: content},
	})
	first, _, err := syncer.Resolve(context.Background(), src)
	if err != nil {
		t.Fatalf("首次同步应成功：%v", err)
	}

	// 第二个版本 sha256 不符，应在安装前失败。
	publishRaw(fixture, key, indexYAML(2, futureTime, []indexSpec{
		{id: "sample", path: "providers/sample.yaml", sha256: strings.Repeat("0", 64)},
	}))
	_, _, err = syncer.Resolve(context.Background(), src)
	requireErrorContains(t, err, "sha256")

	got, readErr := os.ReadFile(filepath.Join(first.Root, "providers", "sample.yaml"))
	if readErr != nil {
		t.Fatalf("旧快照应仍可读：%v", readErr)
	}
	if string(got) != string(content) {
		t.Errorf("旧快照内容被改动")
	}
	assertNotExist(t, snapshotPath(stateDir, server.URL, "2"))
	assertNotExist(t, snapshotPath(stateDir, server.URL, "2.tmp"))
	if serial := readStateForTest(t, stateDir).Sources[normalizeURL(server.URL)].Serial; serial != 1 {
		t.Errorf("状态 serial = %d，期望仍为 1", serial)
	}
}

func TestResolveRejectsPlainHTTPRemote(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer plain.Close()

	syncer := &Syncer{StateDir: t.TempDir(), Client: plain.Client()}
	_, _, err := syncer.Resolve(context.Background(), Source{Kind: SourceRemote, URL: plain.URL})
	requireErrorContains(t, err, "https")
}

func TestResolveRejectsCrossHostRedirect(t *testing.T) {
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer other.Close()

	fixture := newFixture()
	fixture.redirectTo("/index.yaml", other.URL+"/index.yaml")
	server := httptest.NewTLSServer(fixture)
	defer server.Close()

	syncer := &Syncer{StateDir: t.TempDir(), Client: server.Client()}
	_, _, err := syncer.Resolve(context.Background(), Source{Kind: SourceRemote, URL: server.URL})
	requireErrorContains(t, err, "重定向", "其它主机")
}

func TestResolveLocalSource(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string][]byte{
		"b.yaml":     sampleProfile("bbb"),
		"a.yaml":     sampleProfile("aaa"),
		"notes.txt":  []byte("ignored"),
		"sub/c.yaml": sampleProfile("ccc"),
	} {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatalf("创建目录失败：%v", err)
		}
		if err := os.WriteFile(full, content, 0o600); err != nil {
			t.Fatalf("写入档案失败：%v", err)
		}
	}

	syncer := &Syncer{StateDir: t.TempDir()}
	snap, warnings, err := syncer.Resolve(context.Background(), Source{Kind: SourceLocal, Path: dir})
	if err != nil {
		t.Fatalf("本地源应加载成功：%v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("本地源不应有提醒：%v", warnings)
	}
	if snap.Index != nil {
		t.Errorf("本地源没有索引，实际 %+v", snap.Index)
	}
	if snap.Root != dir {
		t.Errorf("root = %q，期望 %q", snap.Root, dir)
	}
	if len(snap.Profiles) != 2 {
		t.Fatalf("档案数 = %d，期望 2（只认直接子目录下的 *.yaml）", len(snap.Profiles))
	}
	if snap.Profiles[0].ID != "aaa" || snap.Profiles[1].ID != "bbb" {
		t.Errorf("档案应按文件名排序，实际 %q、%q", snap.Profiles[0].ID, snap.Profiles[1].ID)
	}
	// 本地源不写状态文件。
	assertNotExist(t, filepath.Join(syncer.StateDir, "state.json"))
}

func TestResolveBuiltinIsRejected(t *testing.T) {
	syncer := &Syncer{}
	_, _, err := syncer.Resolve(context.Background(), Source{Kind: SourceBuiltin})
	requireErrorContains(t, err, "内置源")
}

func TestLoadIndexRejectsPathTraversal(t *testing.T) {
	for _, badPath := range []string{"../escape.yaml", "/etc/passwd", "providers/../../escape.yaml"} {
		t.Run(badPath, func(t *testing.T) {
			raw := indexYAML(1, futureTime, []indexSpec{
				{id: "sample", path: badPath, sha256: strings.Repeat("0", 64)},
			})
			_, err := LoadIndex("index.yaml", raw)
			requireErrorContains(t, err, "路径")
		})
	}
}

func TestLoadIndexRejectsDuplicatesAndEmpty(t *testing.T) {
	sum := strings.Repeat("0", 64)

	t.Run("id 重复", func(t *testing.T) {
		raw := indexYAML(1, futureTime, []indexSpec{
			{id: "dup", path: "a.yaml", sha256: sum},
			{id: "dup", path: "b.yaml", sha256: sum},
		})
		_, err := LoadIndex("index.yaml", raw)
		requireErrorContains(t, err, "id", "重复")
	})

	t.Run("path 重复", func(t *testing.T) {
		raw := indexYAML(1, futureTime, []indexSpec{
			{id: "a", path: "same.yaml", sha256: sum},
			{id: "b", path: "same.yaml", sha256: sum},
		})
		_, err := LoadIndex("index.yaml", raw)
		requireErrorContains(t, err, "路径", "重复")
	})

	t.Run("profiles 为空", func(t *testing.T) {
		raw := indexYAML(1, futureTime, nil)
		_, err := LoadIndex("index.yaml", raw)
		requireErrorContains(t, err, "至少列出")
	})
}

func TestLoadIndexErrorsAreLocated(t *testing.T) {
	sum := strings.Repeat("0", 64)
	raw := indexYAML(1, futureTime, []indexSpec{
		{id: "a", path: "same.yaml", sha256: sum},
		{id: "b", path: "same.yaml", sha256: sum},
	})
	_, err := LoadIndex("index.yaml", raw)

	var profileErr *Error
	if !errors.As(err, &profileErr) {
		t.Fatalf("错误应为 *profile.Error，实际 %T", err)
	}
	if profileErr.File != "index.yaml" {
		t.Errorf("File = %q，期望 index.yaml", profileErr.File)
	}
	if profileErr.Line <= 0 {
		t.Errorf("索引错误应带行号，实际 Line = %d", profileErr.Line)
	}
}

// 索引里声明的路径必须是相对路径：绝对 URL 会被 ResolveReference 直接返回，
// 于是请求绕开同源重定向约束与「只支持 https」两条。
func TestLoadIndexRejectsAbsoluteURLPath(t *testing.T) {
	for _, badPath := range []string{
		"https://evil.example/x.yaml",
		"http://10.0.0.1/x.yaml",
		"//evil.example/x.yaml",
	} {
		t.Run(badPath, func(t *testing.T) {
			raw := indexYAML(1, futureTime, []indexSpec{
				{id: "sample", path: badPath, sha256: strings.Repeat("0", 64)},
			})
			_, err := LoadIndex("index.yaml", raw)
			requireErrorContains(t, err, "相对路径")
		})
	}
}

// 与已装快照按 *.yaml 收集档案的口径一致：声明成别的后缀会在 list 里消失。
func TestLoadIndexRejectsNonYAMLPath(t *testing.T) {
	raw := indexYAML(1, futureTime, []indexSpec{
		{id: "sample", path: "providers/sample.yml", sha256: strings.Repeat("0", 64)},
	})
	_, err := LoadIndex("index.yaml", raw)
	requireErrorContains(t, err, ".yaml")
}

// 更高代数的索引不能被当成代数 1 处理：形状恰好相同时，新代数新增的约束会被静默忽略。
func TestLoadIndexRejectsUnknownGeneration(t *testing.T) {
	raw := indexYAML(1, futureTime, []indexSpec{
		{id: "sample", path: "providers/sample.yaml", sha256: strings.Repeat("0", 64)},
	})
	raw = []byte(strings.Replace(string(raw), "schema: 1", "schema: 2", 1))
	_, err := LoadIndex("index.yaml", raw)
	requireErrorContains(t, err, "索引格式代数 2")
}

// 两个源各自发布 serial 1：这是不同源各自的序号，互不可比，快照目录必须按源隔离，
// 否则后同步的源会删掉先同步的源正在用的目录。
func TestResolveKeepsSnapshotsOfDistinctSources(t *testing.T) {
	key := newTestKey(t, "main")
	fixtureA, fixtureB := newFixture(), newFixture()
	serverA := httptest.NewTLSServer(fixtureA)
	defer serverA.Close()
	serverB := httptest.NewTLSServer(fixtureB)
	defer serverB.Close()

	publish(t, fixtureA, key, 1, futureTime, []fixtureProfile{
		{id: "a", path: "providers/a.yaml", data: sampleProfile("a")},
	})
	publish(t, fixtureB, key, 1, futureTime, []fixtureProfile{
		{id: "b", path: "providers/b.yaml", data: sampleProfile("b")},
	})

	stateDir := t.TempDir()
	snapA, _, err := (&Syncer{StateDir: stateDir, Client: serverA.Client()}).Resolve(
		context.Background(), Source{Kind: SourceRemote, URL: serverA.URL, Keys: []PublicKey{key.public()}})
	if err != nil {
		t.Fatalf("源 A 同步应成功：%v", err)
	}
	snapB, _, err := (&Syncer{StateDir: stateDir, Client: serverB.Client()}).Resolve(
		context.Background(), Source{Kind: SourceRemote, URL: serverB.URL, Keys: []PublicKey{key.public()}})
	if err != nil {
		t.Fatalf("源 B 同步应成功：%v", err)
	}
	if snapA.Root == snapB.Root {
		t.Fatalf("两个源的快照目录不应相同：%q", snapA.Root)
	}

	got, readErr := os.ReadFile(filepath.Join(snapA.Root, "providers", "a.yaml"))
	if readErr != nil {
		t.Fatalf("源 A 的快照应仍可读：%v", readErr)
	}
	if string(got) != string(sampleProfile("a")) {
		t.Errorf("源 A 的快照内容被源 B 覆盖")
	}

	state := readStateForTest(t, stateDir)
	if state.Sources[normalizeURL(serverA.URL)].Root != snapA.Root {
		t.Errorf("状态里源 A 的 root 变了：%q", state.Sources[normalizeURL(serverA.URL)].Root)
	}
	if state.Sources[normalizeURL(serverB.URL)].Root != snapB.Root {
		t.Errorf("状态里源 B 的 root 不对：%q", state.Sources[normalizeURL(serverB.URL)].Root)
	}

	installedA, err := (&Syncer{StateDir: stateDir}).Installed(Source{Kind: SourceRemote, URL: serverA.URL})
	if err != nil {
		t.Fatalf("源 A 的已装快照应可读：%v", err)
	}
	if installedA == nil || len(installedA.Profiles) != 1 || installedA.Profiles[0].ID != "a" {
		t.Errorf("源 A 的已装快照应只有档案 a，实际 %+v", installedA)
	}
}

// serial 推进后旧序号目录被回收，状态指向新目录。
func TestResolveAdvancesSerialAndPrunesOldSnapshot(t *testing.T) {
	key := newTestKey(t, "main")
	fixture := newFixture()
	server := httptest.NewTLSServer(fixture)
	defer server.Close()

	syncer := &Syncer{StateDir: t.TempDir(), Client: server.Client()}
	src := Source{Kind: SourceRemote, URL: server.URL, Keys: []PublicKey{key.public()}}

	publish(t, fixture, key, 1, futureTime, []fixtureProfile{
		{id: "sample", path: "providers/sample.yaml", data: sampleProfile("sample")},
	})
	first, _, err := syncer.Resolve(context.Background(), src)
	if err != nil {
		t.Fatalf("首次同步应成功：%v", err)
	}

	publish(t, fixture, key, 2, futureTime, []fixtureProfile{
		{id: "sample", path: "providers/sample.yaml", data: sampleProfile("sample")},
	})
	second, _, err := syncer.Resolve(context.Background(), src)
	if err != nil {
		t.Fatalf("第二个 serial 应同步成功：%v", err)
	}
	if second.Serial != 2 {
		t.Errorf("serial = %d，期望 2", second.Serial)
	}
	if second.Root == first.Root {
		t.Errorf("新 serial 应装到新目录，实际仍是 %q", second.Root)
	}
	assertNotExist(t, first.Root)
	if serial := readStateForTest(t, syncer.StateDir).Sources[normalizeURL(server.URL)].Serial; serial != 2 {
		t.Errorf("状态 serial = %d，期望 2", serial)
	}
}

// 两次并发的同源同步必须被串行化：否则两边各自读到旧 serial，双双认为本次合法，
// 一个把另一个刚装的目录删掉重装。持锁后应恰好成功一次，另一次以回滚拒绝。
func TestResolveSerializesConcurrentUpdates(t *testing.T) {
	key := newTestKey(t, "main")
	fixture := newFixture()
	server := httptest.NewTLSServer(fixture)
	defer server.Close()

	publish(t, fixture, key, 1, futureTime, []fixtureProfile{
		{id: "sample", path: "providers/sample.yaml", data: sampleProfile("sample")},
	})

	syncer := &Syncer{StateDir: t.TempDir(), Client: server.Client()}
	src := Source{Kind: SourceRemote, URL: server.URL, Keys: []PublicKey{key.public()}}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, errs[i] = syncer.Resolve(context.Background(), src)
		}(i)
	}
	wg.Wait()

	success := 0
	for i, err := range errs {
		if err == nil {
			success++
			continue
		}
		if !strings.Contains(err.Error(), "回滚") {
			t.Errorf("第 %d 次并发同步的错误应为回滚拒绝，实际 %v", i, err)
		}
	}
	if success != 1 {
		t.Fatalf("并发两次同源同 serial 应恰好成功一次，实际 %d 次", success)
	}
}

// 同一个源的不同写法必须命中同一条状态键，否则改一种写法就能绕过 serial 单调性。
func TestSourceStateKeyMergesEquivalentWritings(t *testing.T) {
	a := Source{Kind: SourceRemote, URL: "https://EXAMPLE.com/nova/profiles"}
	b := Source{Kind: SourceRemote, URL: "https://example.com.:443/nova/profiles"}
	if a.stateKey() != b.stateKey() {
		t.Errorf("等价写法应得同一状态键：%q / %q", a.stateKey(), b.stateKey())
	}
}

// 重定向约束按归一后的主机比较：同一主机的等价写法放行，跨主机与非 https 一律拒绝。
func TestRemoteClientRedirectBoundary(t *testing.T) {
	base, err := url.Parse("https://EXAMPLE.com./profiles")
	if err != nil {
		t.Fatalf("构造 base 失败：%v", err)
	}
	client := (&Syncer{}).remoteClient(base)

	sameHost, err := http.NewRequest(http.MethodGet, "https://example.com:443/index.yaml", nil)
	if err != nil {
		t.Fatalf("构造同源请求失败：%v", err)
	}
	if err := client.CheckRedirect(sameHost, nil); err != nil {
		t.Errorf("同一主机的等价写法应放行，实际 %v", err)
	}

	otherHost, err := http.NewRequest(http.MethodGet, "https://evil.example/index.yaml", nil)
	if err != nil {
		t.Fatalf("构造跨主机请求失败：%v", err)
	}
	requireErrorContains(t, client.CheckRedirect(otherHost, nil), "其它主机")

	plain, err := http.NewRequest(http.MethodGet, "http://example.com/index.yaml", nil)
	if err != nil {
		t.Fatalf("构造明文请求失败：%v", err)
	}
	requireErrorContains(t, client.CheckRedirect(plain, nil), "https")
}
