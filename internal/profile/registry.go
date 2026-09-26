package profile

import "fmt"

// Registry 是按 id 索引的档案集合。
//
// 集合保留 id 首次加入时的位置：同名档案由后加入者覆盖内容，位置不动，因此「按声明
// 顺序逐个覆盖」的结果与「最后一次写入的内容 + 第一次出现的位置」一致，顺序不会被
// 覆盖行为打乱。
type Registry struct {
	profiles map[string]*Profile
	order    []string
}

// NewRegistry 返回一个空注册表。
func NewRegistry() *Registry {
	return &Registry{profiles: map[string]*Profile{}}
}

// Lookup 按 id 取档案；不存在时返回 false。
func (r *Registry) Lookup(id string) (*Profile, bool) {
	loaded, ok := r.profiles[id]
	return loaded, ok
}

// IDs 按加入顺序返回全部 id。
//
// 返回副本：调用方按顺序遍历时，注册表的后续加入不该改变它手上的切片。
func (r *Registry) IDs() []string {
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// Add 加入一份档案；同 id 覆盖内容，位置保持在首次加入处。
func (r *Registry) Add(loaded *Profile) {
	if loaded == nil {
		return
	}
	if _, ok := r.profiles[loaded.ID]; !ok {
		r.order = append(r.order, loaded.ID)
	}
	r.profiles[loaded.ID] = loaded
}

// SourceStatus 是一个源在装配注册表时的结果。
//
// 源失败不中止装配，而是记进 Err：调用方据此既能照常使用其余来源的档案，又能在
// 「引用的档案缺失」时把失败源一并说清。Synced 区分「源里确实没有档案」与
// 「源尚未同步」——远端源从未同步过不是错误，新加的源本来就还没有本地快照。
type SourceStatus struct {
	// Kind 是源的种类。
	Kind SourceKind

	// Label 是源的展示标签：种类加地址（本地为路径）。
	Label string

	// Count 是这个源贡献的档案数；失败时为 0。
	Count int

	// Serial 是远端源的已装索引序号；本地源与内置源为 0。
	Serial int64

	// Synced 表示这个源是否已有一份可用快照。
	Synced bool

	// Err 是这个源的读取失败原因；未同步不是失败，为 nil。
	Err error
}

// LoadSources 装配注册表：内置档案作基底，再按声明顺序让各源的档案覆盖同名条目。
//
// 内置源永远存在且优先级最低：它随二进制发布，是理解其余档案的前提，因此先整份铺上，
// 声明里的 builtin 只记录状态、不再重复读取，也不因它出现在后面而把已覆盖的条目改回去。
//
// 源的失败不中止装配：某个源读不出来不该让整张表消失，否则「引用的档案缺失」这类报错
// 会只剩一个与它无关的源错误。失败记进 SourceStatus.Err，由调用方决定是否阻断启动、
// 以及如何把失败源与缺失引用一起呈现。
//
// 返回的 error 只表示基底不可用（内置档案加载失败）：那是程序自身的错误，
// 与使用者配置的源无关。
func LoadSources(sources []Source, syncer *Syncer) (*Registry, []SourceStatus, error) {
	builtin, err := Builtin()
	if err != nil {
		return nil, nil, err
	}

	registry := NewRegistry()
	for _, loaded := range builtin {
		registry.Add(loaded)
	}

	statuses := make([]SourceStatus, 0, len(sources))
	for _, src := range sources {
		status := SourceStatus{Kind: src.Kind, Label: sourceLabel(src)}
		if syncer == nil && src.Kind != SourceBuiltin {
			status.Err = &Error{Msg: "读取档案源需要 Syncer，未给出"}
			statuses = append(statuses, status)
			continue
		}

		switch src.Kind {
		case SourceBuiltin:
			// 基底已经铺好，这里只报告它有多少份。
			status.Count = len(builtin)
			status.Synced = true

		case SourceLocal:
			// 本地源就在磁盘上，读它不涉及网络，也不算「安装」。
			snap, _, resolveErr := syncer.resolveLocal(src)
			if resolveErr != nil {
				status.Err = resolveErr
				break
			}
			for _, loaded := range snap.Profiles {
				registry.Add(loaded)
			}
			status.Count = len(snap.Profiles)
			status.Synced = true

		case SourceRemote:
			installed, readErr := syncer.Installed(src)
			switch {
			case readErr != nil:
				status.Err = readErr
			case installed == nil:
				// 尚未同步不是错误，不改动基底，也不贡献档案。
				status.Synced = false
			default:
				for _, loaded := range installed.Profiles {
					registry.Add(loaded)
				}
				status.Count = len(installed.Profiles)
				status.Serial = installed.Serial
				status.Synced = true
			}

		default:
			status.Err = &Error{Msg: fmt.Sprintf("未知的源类型 %q", string(src.Kind))}
		}
		statuses = append(statuses, status)
	}
	return registry, statuses, nil
}

// sourceLabel 把一个源排成人读标签：种类加地址（本地为路径）。
//
// 与 cmd 层的展示口径一致，注册表的使用者不必再按 Kind 分一遍。
func sourceLabel(src Source) string {
	switch src.Kind {
	case SourceBuiltin:
		return "内置源"
	case SourceLocal:
		return "本地源 " + src.Path
	default:
		return "远端源 " + src.URL
	}
}
