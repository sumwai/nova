// Package credentials 读写 nova 的凭据库：档案 id 与账号名到 API Key 的映射。
//
// 存放位置是 ${XDG_CONFIG_HOME:-~/.config}/nova/credentials.json，目录 0700、文件 0600。
// 索引键是档案 id 加账号名，与 Novafile 里的 provider 名无关：一份配置可以给同一个
// 档案起多个渠道名，凭据却只该存一份。
//
// 本包是叶子包：不依赖任何 internal 包，也不联网、不校验密钥是否有效。
// 密钥是否可用由上游的第一次响应回答，登录流程只负责把它存好。
package credentials

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Version 是凭据库文件的格式代数。
//
// 与配置、档案同一口径：代数只增不减，读到高于本二进制的代数直接拒绝，
// 而不是按已知字段尽力解析出一份残缺的凭据。
const Version = 1

// dirName 与 fileName 固定凭据库在用户配置目录下的相对位置。
//
// 写成常量让「凭据库到底在哪」只有一处出处：命令行的提示、报错定位与读写路径
// 都从这里派生，不会各自拼一份可能漂移的相对路径。
const (
	dirName  = "nova"
	fileName = "credentials.json"
)

// Error 是一条带文件路径的凭据库错误。
//
// 它不带行号：凭据库由程序写、不由人逐行维护，位置信息到文件一级即可。
type Error struct {
	File string
	Msg  string
}

// Error 按「有多少位置信息就说多少」排版，与配置、档案错误同一形状。
func (e *Error) Error() string {
	if e.File == "" {
		return e.Msg
	}
	return e.File + ": " + e.Msg
}

// Entry 是一个账号在凭据库里的记录。
type Entry struct {
	// Name 是账号名。
	Name string
	// Key 是明文密钥。
	Key string
	// AddedAt 是加入时刻，RFC3339 文本。
	AddedAt string
}

// Suffix 返回密钥末尾四位，供展示。
//
// 不足四位时整体遮住：把一份短密钥原样显示等于泄露它的全部。前缀的省略号
// 用来表明这是一个截断后的尾巴，而不是密钥本身。
func Suffix(key string) string {
	runes := []rune(key)
	switch {
	case len(runes) == 0:
		return ""
	case len(runes) <= 4:
		return strings.Repeat("*", len(runes))
	default:
		return "…" + string(runes[len(runes)-4:])
	}
}

// storedAccount 是磁盘上的账号记录，字段名与 JSON 契约一一对应。
type storedAccount struct {
	Key     string `json:"key"`
	AddedAt string `json:"added_at"`
}

// fileData 是凭据库文件的顶层形状。
type fileData struct {
	Version  int                                 `json:"version"`
	Accounts map[string]map[string]storedAccount `json:"accounts"`
}

// Store 是内存中的凭据库。
//
// path 是落盘位置；为空表示这份存储没有落点（用于只有环境变量来源的场景），
// 对它调用 Set 或 Remove 会报错，读操作照常返回空结果。
type Store struct {
	path     string
	accounts map[string]map[string]storedAccount
}

// New 返回一份空的、没有落点的存储。
func New() *Store {
	return &Store{accounts: map[string]map[string]storedAccount{}}
}

// Load 读取配置目录下的凭据库。
//
// configDir 是用户配置目录（${XDG_CONFIG_HOME:-~/.config}），凭据库取它下面的
// nova/credentials.json。文件不存在时返回空存储而不是错误：还没登录过是正常状态。
// 文件存在但读不出来或解析不了时报错并带上路径，不静默当成空——那会让「改了文件
// 却没生效」变成一个没有线索的现象。
func Load(configDir string) (*Store, error) {
	if configDir == "" {
		return nil, &Error{Msg: "读取凭据库需要配置目录，未给出"}
	}
	path := filepath.Join(configDir, dirName, fileName)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Store{path: path, accounts: map[string]map[string]storedAccount{}}, nil
	}
	if err != nil {
		return nil, &Error{File: path, Msg: fmt.Sprintf("读取凭据库失败：%v", err)}
	}

	var parsed fileData
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, &Error{File: path, Msg: fmt.Sprintf("凭据库不是合法 JSON：%v", err)}
	}
	if parsed.Version != Version {
		return nil, &Error{File: path, Msg: fmt.Sprintf(
			"凭据库格式代数 %d 不被本二进制支持（支持 %d）", parsed.Version, Version)}
	}
	if parsed.Accounts == nil {
		parsed.Accounts = map[string]map[string]storedAccount{}
	}
	return &Store{path: path, accounts: parsed.Accounts}, nil
}

// Path 返回凭据库的落盘位置；没有落点时为空串。
func (s *Store) Path() string { return s.path }

// Set 写入一份账号密钥；同名账号覆盖旧值。
//
// 覆盖时刷新加入时间：它记录的是「这份密钥是什么时候存进来的」，
// 换了一份密钥却保留旧时间会让排查时对着一个对不上号的时刻。
func (s *Store) Set(profileID, name, key string) error {
	if profileID == "" || name == "" {
		return &Error{File: s.path, Msg: "档案 id 与账号名都不能为空"}
	}
	if key == "" {
		return &Error{File: s.path, Msg: "密钥不能为空"}
	}
	if s.accounts == nil {
		s.accounts = map[string]map[string]storedAccount{}
	}
	if s.accounts[profileID] == nil {
		s.accounts[profileID] = map[string]storedAccount{}
	}
	s.accounts[profileID][name] = storedAccount{
		Key:     key,
		AddedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	return s.save()
}

// Remove 删除一个账号；name 为空时删除该档案下的全部账号。
//
// 目标不存在时不报错，也不写文件：删除已经不存在的东西是可重复执行的操作，
// 「删了 0 个」由调用方按删除前的计数呈现。
func (s *Store) Remove(profileID, name string) error {
	if profileID == "" {
		return &Error{File: s.path, Msg: "档案 id 不能为空"}
	}
	if name == "" {
		if _, ok := s.accounts[profileID]; !ok {
			return nil
		}
		delete(s.accounts, profileID)
		return s.save()
	}
	accounts, ok := s.accounts[profileID]
	if !ok {
		return nil
	}
	if _, ok := accounts[name]; !ok {
		return nil
	}
	delete(accounts, name)
	if len(accounts) == 0 {
		delete(s.accounts, profileID)
	}
	return s.save()
}

// Lookup 按档案 id 与账号名取密钥。
func (s *Store) Lookup(profileID, name string) (string, bool) {
	account, ok := s.accounts[profileID][name]
	if !ok {
		return "", false
	}
	return account.Key, true
}

// Names 按加入顺序列出某个档案下的账号名。
func (s *Store) Names(profileID string) []string {
	entries := s.Entries(profileID)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name)
	}
	return names
}

// Entries 按加入顺序列出某个档案下的账号记录。
//
// 顺序取加入时间再按名字：map 的遍历顺序随机，而「哪些账号、谁是第一个」在
// 报错与选路上都要用，随机顺序会让同一份凭据库每次给出不同的答案。
func (s *Store) Entries(profileID string) []Entry {
	accounts := s.accounts[profileID]
	entries := make([]Entry, 0, len(accounts))
	for name, account := range accounts {
		entries = append(entries, Entry{Name: name, Key: account.Key, AddedAt: account.AddedAt})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		ti, tj := parseAddedAt(entries[i].AddedAt), parseAddedAt(entries[j].AddedAt)
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		return entries[i].Name < entries[j].Name
	})
	return entries
}

// ProfileIDs 列出凭据库里出现过账号的档案 id，按字典序。
func (s *Store) ProfileIDs() []string {
	ids := make([]string, 0, len(s.accounts))
	for id := range s.accounts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// parseAddedAt 解析加入时刻；解析不了时返回零值，让它排在最前。
//
// 手工改过的凭据库可能带着一个不合格式的时间，这不该让整份清单读不出来。
func parseAddedAt(text string) time.Time {
	parsed, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

// save 以临时文件加 rename 写凭据库。
//
// 写入与 rename 都先 fsync：掉电后 rename 可能尚未落盘，一份新存的密钥因此丢失，
// 而文件表面上还在。目录权限 0700、文件权限 0600：明文密钥不该让同机的其他用户读到。
func (s *Store) save() error {
	if s.path == "" {
		return &Error{Msg: "凭据库没有落点，无法写入"}
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return &Error{File: s.path, Msg: fmt.Sprintf("创建凭据库目录失败：%v", err)}
	}

	data, err := json.MarshalIndent(fileData{Version: Version, Accounts: s.accounts}, "", "  ")
	if err != nil {
		return &Error{File: s.path, Msg: fmt.Sprintf("序列化凭据库失败：%v", err)}
	}

	tmp, err := os.CreateTemp(dir, fileName+".tmp-*")
	if err != nil {
		return &Error{File: s.path, Msg: fmt.Sprintf("创建临时凭据文件失败：%v", err)}
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return &Error{File: s.path, Msg: fmt.Sprintf("写入临时凭据文件失败：%v", err)}
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return &Error{File: s.path, Msg: fmt.Sprintf("同步临时凭据文件失败：%v", err)}
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return &Error{File: s.path, Msg: fmt.Sprintf("设置凭据文件权限失败：%v", err)}
	}
	if err := tmp.Close(); err != nil {
		return &Error{File: s.path, Msg: fmt.Sprintf("关闭临时凭据文件失败：%v", err)}
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return &Error{File: s.path, Msg: fmt.Sprintf("替换凭据文件失败：%v", err)}
	}
	// rename 只保证目录项可见，目录项本身要 fsync 目录才会落盘。
	if dirFile, err := os.Open(dir); err == nil {
		dirFile.Sync()
		dirFile.Close()
	}
	return nil
}
