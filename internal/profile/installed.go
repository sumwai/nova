package profile

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// InstalledSnapshot 是一个源已安装快照的登记与内容。
//
// 它与 Snapshot 的区别在来源：Snapshot 是「解析一个源」的结果，InstalledSnapshot
// 是「读磁盘上的状态」的结果。Serial 与 InstalledAt 来自 StateDir 里的 state.json，
// Profiles 来自对应目录下已落盘的档案，取这些都不需要再连一次远端。
type InstalledSnapshot struct {
	// SourceKey 是这个源在状态里的标识。
	SourceKey string

	// Serial 是已安装快照的索引序号。
	Serial int64

	// InstalledAt 是上次成功安装时刻，RFC3339 文本。
	InstalledAt string

	// Root 是已安装快照的绝对路径。
	Root string

	// Profiles 是快照内的档案，按相对路径排序。
	Profiles []*Profile
}

// Installed 读取一个源已安装的快照，不发起任何网络请求。
//
// 从未同步过时返回 (nil, nil)：那是「还没装过」，不是「读失败」，调用方据此显示
// 「尚未同步」并正常退出，不该把它当错误处理。
func (s *Syncer) Installed(src Source) (*InstalledSnapshot, error) {
	if s.StateDir == "" {
		return nil, &Error{Msg: "读取已装快照需要 StateDir，未给出"}
	}
	state, readErr := s.readState()
	if readErr != nil {
		return nil, readErr
	}
	key := src.stateKey()
	entry, ok := state.Sources[key]
	if !ok {
		return nil, nil
	}
	// 快照路径来自 state.json，不该无条件相信：不在 profiles 目录以内的路径
	// 会让 list 递归加载任意目录下的 *.yaml。
	if !withinProfilesDir(s.StateDir, entry.Root) {
		return nil, &Error{File: entry.Root, Msg: fmt.Sprintf(
			"状态里的快照路径 %q 不在 %s 以内，拒绝加载", entry.Root, filepath.Join(s.StateDir, "profiles"))}
	}
	profiles, loadErr := loadProfilesIn(entry.Root)
	if loadErr != nil {
		return nil, loadErr
	}
	return &InstalledSnapshot{
		SourceKey:   key,
		Serial:      entry.Serial,
		InstalledAt: entry.InstalledAt,
		Root:        entry.Root,
		Profiles:    profiles,
	}, nil
}

// withinProfilesDir 报告 root 是否落在 stateDir/profiles 以内。
//
// 先各自取绝对路径再算相对路径，不用字符串前缀比较：前缀比较会把
// /state/profiles-other 误判成 /state/profiles 的子目录。
func withinProfilesDir(stateDir, root string) bool {
	if stateDir == "" || root == "" {
		return false
	}
	base, baseErr := filepath.Abs(filepath.Join(stateDir, "profiles"))
	target, targetErr := filepath.Abs(root)
	if baseErr != nil || targetErr != nil {
		return false
	}
	rel, relErr := filepath.Rel(base, target)
	if relErr != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

// loadProfilesIn 递归加载一个目录下的全部 *.yaml，按路径排序。
//
// 与本地源只认直接子文件不同，已装快照里的档案按索引声明的相对路径存放，
// 可能落在子目录里。排序给出与文件系统无关的稳定顺序：已装快照不再附带索引，
// 索引的声明顺序无从恢复。
func loadProfilesIn(root string) ([]*Profile, error) {
	var names []string
	walkErr := filepath.WalkDir(root, func(current string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			return nil
		}
		names = append(names, current)
		return nil
	})
	if walkErr != nil {
		return nil, &Error{File: root, Msg: fmt.Sprintf("读取已装快照目录失败：%v", walkErr)}
	}
	sort.Strings(names)

	profiles := make([]*Profile, 0, len(names))
	for _, name := range names {
		loaded, loadErr := Load(name)
		if loadErr != nil {
			return nil, loadErr
		}
		profiles = append(profiles, loaded)
	}
	return profiles, nil
}
