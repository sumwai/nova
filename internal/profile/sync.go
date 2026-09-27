package profile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sumwai/nova/internal/sourceurl"
)

// SourceKind 是订阅源的种类。
//
// 取值与配置层同形（builtin / remote / local），但类型在本包内自定义：
// 本包不认识 internal/config，调用方负责把配置拆成这里的纯数据。
type SourceKind string

const (
	// SourceBuiltin 是随二进制发布的档案，不由本包处理。
	SourceBuiltin SourceKind = "builtin"

	// SourceRemote 是 https 远端源，必须带公钥验签。
	SourceRemote SourceKind = "remote"

	// SourceLocal 是本地目录源，免签，不做 sha256 校验，不复制到 StateDir。
	SourceLocal SourceKind = "local"
)

// Source 是一个订阅源。
type Source struct {
	Kind SourceKind

	// URL 是远端源地址，仅 Kind 为 SourceRemote 时使用。
	URL string

	// Path 是本地源目录，仅 Kind 为 SourceLocal 时使用。
	Path string

	// Keys 是这个源的验签公钥，按声明顺序；仅远端源使用。
	Keys []PublicKey
}

// stateKey 返回一个源在状态文件里的键。
//
// 远端源用规范化 URL：写法不同但指向同一个源的地址应当命中同一条 serial 记录，
// 否则换一种写法重写源就绕开了 serial 单调性。
func (s Source) stateKey() string {
	if s.Kind == SourceRemote {
		return normalizeURL(s.URL)
	}
	return s.Path
}

// Snapshot 是一次解析得到的档案集合。
type Snapshot struct {
	// SourceKey 是这个源在状态里的标识：远端为规范化 URL，本地为目录路径。
	SourceKey string

	// Serial 是快照的索引序号；本地源没有序号，取 0。
	Serial int64

	// Root 是快照文件所在目录（远端为已安装的绝对路径，本地为源目录）。
	Root string

	// Profiles 是快照内的档案，远端按索引声明顺序，本地按文件名顺序。
	Profiles []*Profile

	// Index 是远端索引；本地源没有索引，为 nil。
	Index *Index
}

// Warning 是一条不阻止本次解析的提醒。
type Warning struct {
	// Source 是提醒所属的源标识。
	Source string

	// Msg 是提醒正文。
	Msg string
}

// String 排版成「源: 正文」，便于与错误一起 grep。
func (w Warning) String() string {
	if w.Source == "" {
		return w.Msg
	}
	return w.Source + ": " + w.Msg
}

// Syncer 同步订阅源。
//
// StateDir 由调用方给出：本包不读环境变量，也不猜缓存目录在哪。
// Client 为空时用一个带超时的客户端（见 requestTimeout）；远端请求会复制一份并加上
// 同源重定向约束，调用方传进来的客户端不会被改动。
type Syncer struct {
	StateDir string
	Client   *http.Client
}

// stateVersion 是 state.json 的格式代数。
const stateVersion = 1

const (
	// requestTimeout 是单次远端请求的时长上限。Syncer.Client 为空时用它，
	// 避免一个不响应的源把命令永久挂住。调用方传入的客户端不被覆写。
	requestTimeout = 30 * time.Second

	// maxIndexBytes 是索引与签名的响应体上限。
	maxIndexBytes = 1 << 20

	// maxProfileBytes 是单份档案的响应体上限。
	maxProfileBytes = 8 << 20
)

// Resolve 解析一个源，返回本次可用的快照。
//
// builtin 源返回明确错误：内置档案随二进制发布，装配层直接读嵌入资源即可，
// 本包不复制它们。返回错误而不是空快照，是为了让「源还没接」与「源里一份档案都没有」
// 保持可区分。
func (s *Syncer) Resolve(ctx context.Context, src Source) (*Snapshot, []Warning, error) {
	switch src.Kind {
	case SourceBuiltin:
		return nil, nil, &Error{Msg: "内置源由调用方直接读取，不经过同步"}
	case SourceRemote:
		return s.resolveRemote(ctx, src)
	case SourceLocal:
		return s.resolveLocal(src)
	default:
		return nil, nil, &Error{Msg: fmt.Sprintf("未知的源类型 %q", string(src.Kind))}
	}
}

// resolveLocal 加载一个本地目录源。
//
// 本地源免签、不校验 sha256、也不复制到 StateDir：它就在使用者的磁盘上，
// 签名与内容散列都不能带来额外保证。目录下的 *.yaml 按文件名排序后逐份 Load，
// 任一份失败即整次失败——本地覆盖里出现坏档案时，静默跳过会让「哪份生效」变得不可预期。
func (s *Syncer) resolveLocal(src Source) (*Snapshot, []Warning, error) {
	if src.Path == "" {
		return nil, nil, &Error{Msg: "本地源没有目录路径"}
	}
	entries, err := os.ReadDir(src.Path)
	if err != nil {
		return nil, nil, &Error{File: src.Path, Msg: fmt.Sprintf("读取本地源目录失败：%v", err)}
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)

	profiles := make([]*Profile, 0, len(names))
	for _, name := range names {
		loaded, loadErr := Load(filepath.Join(src.Path, name))
		if loadErr != nil {
			return nil, nil, loadErr
		}
		profiles = append(profiles, loaded)
	}

	return &Snapshot{
		SourceKey: src.Path,
		Root:      src.Path,
		Profiles:  profiles,
	}, nil, nil
}

// resolveRemote 拉取并安装一个远端源。
//
// 步骤严格按设计文档第四节：验签先于解析，serial 与 not_after 先于下载，
// 全部文件校验通过后整体 rename 安装。任一步失败都不改动已装快照。
func (s *Syncer) resolveRemote(ctx context.Context, src Source) (*Snapshot, []Warning, error) {
	if s.StateDir == "" {
		return nil, nil, &Error{Msg: "远端同步需要 StateDir，未给出"}
	}
	base, urlErr := parseRemoteURL(src)
	if urlErr != nil {
		return nil, nil, urlErr
	}
	client := s.remoteClient(base)
	key := src.stateKey()

	// 1. 取 index.yaml 与 index.yaml.sig。
	indexURL := joinRemoteURL(base, "index.yaml")
	rawIndex, err := s.fetch(ctx, client, indexURL, maxIndexBytes)
	if err != nil {
		return nil, nil, err
	}
	rawSig, err := s.fetch(ctx, client, joinRemoteURL(base, "index.yaml.sig"), maxIndexBytes)
	if err != nil {
		return nil, nil, err
	}

	// 2. 先验签。验签失败即返回，索引字节不进入解析器：
	// 未经验证的输入不该有机会触发解析路径上的任何行为。
	if _, sigErr := VerifyIndex(rawIndex, rawSig, src.Keys); sigErr != nil {
		return nil, nil, sigErr
	}
	index, indexErr := LoadIndex(indexURL, rawIndex)
	if indexErr != nil {
		return nil, nil, indexErr
	}

	// 3. serial 单调：不大于已见最大值一律拒绝。签名只证明来源，不证明最新，
	// 回滚只能靠本地记录的单调性挡。
	//
	// 读状态到写状态全程持锁：两个并发的 update 如果各自读到旧 serial，会双双
	// 认为本次 serial 合法，随后一个把另一个刚装的目录删掉重装。锁让
	// 「检查 serial」与「安装新 serial」成为一件事。
	unlock, lockErr := s.lockState()
	if lockErr != nil {
		return nil, nil, lockErr
	}
	defer unlock()

	state, stateErr := s.readState()
	if stateErr != nil {
		return nil, nil, stateErr
	}
	if seen := state.Sources[key].Serial; index.Serial <= seen {
		return nil, nil, &Error{upToDate: true, Msg: fmt.Sprintf(
			"源 %s 的索引 serial %d 不大于已见的最大值 %d，拒绝回滚", key, index.Serial, seen)}
	}

	// 4. not_after 已过：拒绝本次更新并告警，已装快照不动。
	notAfter, parseErr := time.Parse(time.RFC3339, index.NotAfter)
	if parseErr != nil {
		return nil, nil, &Error{Msg: fmt.Sprintf("源 %s 的索引 not_after %q 不是 RFC3339 时刻：%v",
			key, index.NotAfter, parseErr)}
	}
	if !time.Now().Before(notAfter) {
		warning := Warning{Source: key, Msg: fmt.Sprintf(
			"索引 not_after %s 已过，本次更新被拒；已装快照保持可用", index.NotAfter)}
		return nil, []Warning{warning}, &Error{upToDate: true, Msg: fmt.Sprintf(
			"源 %s 的索引 not_after %s 已过，本次更新被拒（源并非不可用，已装快照继续生效）",
			key, index.NotAfter)}
	}

	// 5-7. 下载、逐份校验，再整体安装。
	return s.installRemote(ctx, client, base, key, index, state)
}

// installRemote 下载索引列出的全部档案，校验后原子安装，并更新状态。
//
// 临时目录是安装的中间态：只有全部文件通过 sha256、schema 与语义校验，
// 且实际文件集合与索引声明完全相等时，才 rename 成正式快照。
// 在此之前任何失败都只留下一次 RemoveAll。
func (s *Syncer) installRemote(
	ctx context.Context,
	client *http.Client,
	base *url.URL,
	key string,
	index *Index,
	state *stateFile,
) (*Snapshot, []Warning, error) {
	// 每个源一个命名空间：serial 只是发行方各自发布的序号，不同源之间没有任何可比较性，
	// 不能共用同一个目录名。把源身份写进路径，跨源撞号就不会互相覆盖。
	namespaceDir := filepath.Join(s.StateDir, "profiles", snapshotNamespace(key))
	if err := os.MkdirAll(namespaceDir, 0o700); err != nil {
		return nil, nil, &Error{File: namespaceDir, Msg: fmt.Sprintf("创建快照目录失败：%v", err)}
	}

	serialName := strconv.FormatInt(index.Serial, 10)
	tmpDir := filepath.Join(namespaceDir, serialName+".tmp")
	// 清掉同名残留：上一次在安装前中断会留下这个目录，而它的内容不可信。
	if err := os.RemoveAll(tmpDir); err != nil {
		return nil, nil, &Error{File: tmpDir, Msg: fmt.Sprintf("清理残留临时目录失败：%v", err)}
	}
	if err := os.Mkdir(tmpDir, 0o700); err != nil {
		return nil, nil, &Error{File: tmpDir, Msg: fmt.Sprintf("创建临时目录失败：%v", err)}
	}
	installed := false
	defer func() {
		if !installed {
			os.RemoveAll(tmpDir)
		}
	}()

	declared := make(map[string]bool, len(index.Profiles))
	profiles := make([]*Profile, 0, len(index.Profiles))
	for _, entry := range index.Profiles {
		data, err := s.fetch(ctx, client, joinRemoteURL(base, entry.Path), maxProfileBytes)
		if err != nil {
			return nil, nil, err
		}
		sum := sha256.Sum256(data)
		if got := hex.EncodeToString(sum[:]); got != entry.SHA256 {
			return nil, nil, &Error{Msg: fmt.Sprintf(
				"源 %s 的 %s sha256 不符：索引声明 %s，实际 %s", key, entry.Path, entry.SHA256, got)}
		}

		dest := filepath.Join(tmpDir, filepath.FromSlash(entry.Path))
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			return nil, nil, &Error{File: dest, Msg: fmt.Sprintf("创建档案目录失败：%v", err)}
		}
		if err := os.WriteFile(dest, data, 0o600); err != nil {
			return nil, nil, &Error{File: dest, Msg: fmt.Sprintf("写入档案失败：%v", err)}
		}
		// 从落盘的临时文件加载，而不是从内存加载：校验的对象因此与最终安装的字节完全一致。
		loaded, loadErr := Load(dest)
		if loadErr != nil {
			// 报错里的文件是临时路径，而 defer 会立刻删掉它；把源与索引声明的位置带上，
			// 使用者才知道去看哪一条声明。行号保留，临时路径只是定位辅助。
			var located *Error
			if errors.As(loadErr, &located) {
				return nil, nil, &Error{File: located.File, Line: located.Line, Col: located.Col,
					Msg: fmt.Sprintf("源 %s serial %d 的索引声明 %s：%s", key, index.Serial, entry.Path, located.Msg)}
			}
			return nil, nil, &Error{Msg: fmt.Sprintf(
				"源 %s serial %d 的索引声明 %s：%v", key, index.Serial, entry.Path, loadErr)}
		}
		profiles = append(profiles, loaded)
		declared[entry.Path] = true
	}

	// 6. 实际下载到的文件集合必须与索引声明完全相等，多一个也拒。
	if setErr := checkDownloadedSet(tmpDir, declared); setErr != nil {
		return nil, nil, setErr
	}

	// 顺带核对每条声明的 id 与档案自身的 id：索引声明的 id 与档案内的 id 必须一致，
	// 否则 list 显示档案内的 id、update 显示声明 id，两者不同会让人无从对账。
	for i, entry := range index.Profiles {
		if profiles[i].ID != entry.ID {
			return nil, nil, &Error{Msg: fmt.Sprintf(
				"源 %s 的索引声明 %s 为 id %q，档案内的 id 是 %q", key, entry.Path, entry.ID, profiles[i].ID)}
		}
	}

	// 7. 原子安装：rename 是唯一的可见切换点。
	finalDir := filepath.Join(namespaceDir, serialName)
	// 同 serial 的目录只可能是本源上次「已 rename、状态未写成功」留下的孤儿：
	// 状态里的 serial 严格更小，而本次内容刚通过全部校验，替换它不会丢已登记的快照。
	if err := os.RemoveAll(finalDir); err != nil {
		return nil, nil, &Error{File: finalDir, Msg: fmt.Sprintf("清理同序号残留目录失败：%v", err)}
	}
	if err := os.Rename(tmpDir, finalDir); err != nil {
		return nil, nil, &Error{File: finalDir, Msg: fmt.Sprintf("安装快照失败：%v", err)}
	}
	installed = true

	absRoot, absErr := filepath.Abs(finalDir)
	if absErr != nil {
		return nil, nil, &Error{File: finalDir, Msg: fmt.Sprintf("解析快照绝对路径失败：%v", absErr)}
	}

	// 8. 更新状态。写失败时撤掉刚装的目录，让磁盘回到上一次成功的状态。
	state.Sources[key] = stateEntry{
		Serial:      index.Serial,
		InstalledAt: time.Now().UTC().Format(time.RFC3339),
		Root:        absRoot,
	}
	if stateErr := s.writeState(state); stateErr != nil {
		os.RemoveAll(finalDir)
		return nil, nil, stateErr
	}

	// 9. 回收本源的旧序号目录。命名空间已按源隔离，这里只会删掉本源的旧快照。
	pruneSnapshots(namespaceDir, serialName)

	return &Snapshot{
		SourceKey: key,
		Serial:    index.Serial,
		Root:      absRoot,
		Profiles:  profiles,
		Index:     index,
	}, nil, nil
}

// snapshotNamespace 返回一个源在 profiles 目录下的命名空间。
//
// 用状态键的 sha256 前 16 位（64 bit）：目的是让不同源不撞号，不是防碰撞攻击，
// 同时避免把地址里的主机名与路径直接摆成一个目录层级。
func snapshotNamespace(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:8])
}

// pruneSnapshots 删除命名空间内除 keep 之外的条目。
//
// 回收失败不改变本次同步的结论：旧快照占空间，但不影响新快照可用，
// 因此这里只做尽力而为，不把清理失败当成同步失败。
func pruneSnapshots(namespaceDir, keep string) {
	entries, err := os.ReadDir(namespaceDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.Name() == keep {
			continue
		}
		os.RemoveAll(filepath.Join(namespaceDir, entry.Name()))
	}
}

// lockState 取得状态目录上的互斥锁，串行化同一 StateDir 的远端安装。
//
// 跨进程用 flock：锁随进程退出自动释放，不会因为崩溃留下死锁。
// 返回的函数释放锁并关闭文件，调用方必须 defer 它。
func (s *Syncer) lockState() (func(), *Error) {
	if err := os.MkdirAll(s.StateDir, 0o700); err != nil {
		return nil, &Error{File: s.StateDir, Msg: fmt.Sprintf("创建状态目录失败：%v", err)}
	}
	path := filepath.Join(s.StateDir, ".lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, &Error{File: path, Msg: fmt.Sprintf("打开状态锁失败：%v", err)}
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		file.Close()
		return nil, &Error{File: path, Msg: fmt.Sprintf("锁定状态目录失败：%v", err)}
	}
	return func() {
		syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		file.Close()
	}, nil
}

// parseRemoteURL 校验并把远端源地址解析成 URL。
//
// 明文 http 直接拒绝：公钥校验是远端源唯一的来源保证，明文传输会让它失去意义。
// 需要免签的来源应当放成本地目录。
func parseRemoteURL(src Source) (*url.URL, *Error) {
	if src.URL == "" {
		return nil, &Error{Msg: "远端源没有地址"}
	}
	parsed, err := url.Parse(src.URL)
	if err != nil || parsed.Host == "" {
		return nil, &Error{Msg: fmt.Sprintf("远端源地址 %q 不是合法的 URL", src.URL)}
	}
	if !strings.EqualFold(parsed.Scheme, "https") {
		return nil, &Error{Msg: fmt.Sprintf(
			"远端源 %q 不是 https；远端源只支持 https，需要免签请改用本地路径", src.URL)}
	}
	return parsed, nil
}

// remoteClient 返回本次远端同步用的客户端：同源重定向约束加在副本上。
//
// 跳出同源意味着「签名公钥属于 A，字节却从 B 取」，而签名只覆盖字节、不覆盖来源，
// 因此跨主机重定向一律失败，不做任何例外。同源判定用同一套归一后的主机，
// 大小写与显式默认端口不构成不同主机。
func (s *Syncer) remoteClient(base *url.URL) *http.Client {
	client := http.Client{Timeout: requestTimeout}
	if s.Client != nil {
		client = *s.Client
	}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if sourceurl.CanonicalHost(req.URL.Scheme, req.URL.Host) !=
			sourceurl.CanonicalHost(base.Scheme, base.Host) {
			return fmt.Errorf("重定向到其它主机 %s；远端源只允许同源重定向", req.URL.Host)
		}
		if req.URL.Scheme != "https" {
			return fmt.Errorf("重定向到非 https 地址（%s）", req.URL.Scheme)
		}
		if len(via) >= 10 {
			return fmt.Errorf("重定向次数过多")
		}
		return nil
	}
	return &client
}

// joinRemoteURL 把源目录地址与相对名拼成完整地址。
//
// 源地址按目录理解：路径末尾补上 /，否则 ResolveReference 会把最后一段当成文件名而丢掉。
func joinRemoteURL(base *url.URL, name string) string {
	dir := *base
	if !strings.HasSuffix(dir.Path, "/") {
		dir.Path += "/"
	}
	ref, err := url.Parse(name)
	if err != nil {
		return dir.String()
	}
	return dir.ResolveReference(ref).String()
}

// normalizeURL 把一个远端地址规范化，用作状态文件里的键。
//
// 规则集中在 internal/sourceurl：配置层的去重键与这里的状态键必须给出同一个结论，
// 否则「同一个源写两种地址」既能绕过 serial 单调性，也能造出永远失败的合法配置。
func normalizeURL(raw string) string {
	return sourceurl.Normalize(raw)
}

// fetch 取回一个地址的响应体。
//
// 非 200 一律失败：重定向由客户端的 CheckRedirect 处理，走到这里还非 200 就是错误，
// 把响应体当档案解析只会得到与真实原因无关的报错。
//
// limit 是响应体字节上限。上限在读入时就生效，而不是先 ReadAll 再判断：
// 校验发生在读完之后，超大的响应体在那之前就已经吃满内存。
func (s *Syncer) fetch(ctx context.Context, client *http.Client, address string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, &Error{Msg: fmt.Sprintf("构造请求 %s 失败：%v", address, err)}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, &Error{Msg: fmt.Sprintf("请求 %s 失败：%v", address, err)}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &Error{Msg: fmt.Sprintf("请求 %s 返回 %s", address, resp.Status)}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, &Error{Msg: fmt.Sprintf("读取 %s 响应失败：%v", address, err)}
	}
	if int64(len(data)) > limit {
		return nil, &Error{Msg: fmt.Sprintf("请求 %s 的响应体超过上限 %d 字节", address, limit)}
	}
	return data, nil
}

// checkDownloadedSet 校验下载目录里的文件集合与索引声明完全相等。
//
// 驱动下载的是索引，因此集合通常天然相等；能在这里被抓住的是路径归一后的冲突：
// 两条声明（如 a/b.yaml 与 a//b.yaml）落到同一个文件，使实际集合比声明少一个。
// 少一个与多一个都必须拒：前者意味着有条声明没有兑现，后者意味着下载目录里
// 存在索引没授权的内容。
func checkDownloadedSet(root string, declared map[string]bool) *Error {
	found := make(map[string]bool, len(declared))
	walkErr := filepath.WalkDir(root, func(current string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(root, current)
		if relErr != nil {
			return relErr
		}
		found[filepath.ToSlash(rel)] = true
		return nil
	})
	if walkErr != nil {
		return &Error{File: root, Msg: fmt.Sprintf("清点已下载档案失败：%v", walkErr)}
	}
	for name := range found {
		if !declared[name] {
			return &Error{Msg: fmt.Sprintf(
				"下载目录里出现了索引未声明的档案 %q；文件集合必须与索引完全相等", name)}
		}
	}
	for name := range declared {
		if !found[name] {
			return &Error{Msg: fmt.Sprintf(
				"索引声明的档案 %q 不在下载目录里；文件集合必须与索引完全相等", name)}
		}
	}
	return nil
}

// stateFile 是 state.json 的内容。
type stateFile struct {
	Version int                   `json:"version"`
	Sources map[string]stateEntry `json:"sources"`
}

// stateEntry 是一个源已安装快照的登记项。
type stateEntry struct {
	Serial      int64  `json:"serial"`
	InstalledAt string `json:"installed_at"`
	Root        string `json:"root"`
}

// statePath 返回状态文件路径。
func (s *Syncer) statePath() string {
	return filepath.Join(s.StateDir, "state.json")
}

// readState 读取状态文件；文件不存在时返回一份空状态。
//
// 文件存在但不可解析或版本不认识时失败，不退化成空状态：空状态意味着「什么都没见过」，
// 会把一次回滚放行，而回滚正是 serial 单调性要挡的东西。
func (s *Syncer) readState() (*stateFile, *Error) {
	file := s.statePath()
	data, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return &stateFile{Version: stateVersion, Sources: map[string]stateEntry{}}, nil
	}
	if err != nil {
		return nil, &Error{File: file, Msg: fmt.Sprintf("读取状态文件失败：%v", err)}
	}

	var state stateFile
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, &Error{File: file, Msg: fmt.Sprintf("状态文件不是合法 JSON：%v", err)}
	}
	if state.Version != stateVersion {
		return nil, &Error{File: file, Msg: fmt.Sprintf(
			"状态文件版本 %d 不被本二进制支持（支持 %d）", state.Version, stateVersion)}
	}
	if state.Sources == nil {
		state.Sources = map[string]stateEntry{}
	}
	return &state, nil
}

// writeState 以临时文件 + rename 写状态文件。
//
// 写入与 rename 都先 fsync：掉电后 rename 可能尚未落盘，而 state.json 丢失会被
// readState 当成「什么都没见过」，serial 单调性因此从 0 重新开始。
func (s *Syncer) writeState(state *stateFile) *Error {
	if err := os.MkdirAll(s.StateDir, 0o700); err != nil {
		return &Error{File: s.StateDir, Msg: fmt.Sprintf("创建状态目录失败：%v", err)}
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return &Error{File: s.statePath(), Msg: fmt.Sprintf("序列化状态失败：%v", err)}
	}

	file := s.statePath()
	tmp, err := os.CreateTemp(s.StateDir, "state.json.tmp-*")
	if err != nil {
		return &Error{File: file, Msg: fmt.Sprintf("创建临时状态文件失败：%v", err)}
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return &Error{File: file, Msg: fmt.Sprintf("写入临时状态文件失败：%v", err)}
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return &Error{File: file, Msg: fmt.Sprintf("同步临时状态文件失败：%v", err)}
	}
	if err := tmp.Close(); err != nil {
		return &Error{File: file, Msg: fmt.Sprintf("关闭临时状态文件失败：%v", err)}
	}
	if err := os.Rename(tmpName, file); err != nil {
		return &Error{File: file, Msg: fmt.Sprintf("替换状态文件失败：%v", err)}
	}
	// rename 只保证目录项可见，目录项本身要 fsync 目录才会落盘。
	if dir, err := os.Open(s.StateDir); err == nil {
		dir.Sync()
		dir.Close()
	}
	return nil
}
