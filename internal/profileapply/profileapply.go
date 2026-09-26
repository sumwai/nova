// Package profileapply 是解析层与档案层之间的装配胶水。
//
// 它做三件事：把配置层的档案源转成档案层的纯数据、装配一份档案注册表、
// 把配置里尚未展开的档案引用就地补成具体的端点、模型与凭据。
//
// 依赖方向只有一条：profileapply → internal/config 与 internal/profile，
// 反过来不成立。配置层不认档案，档案层不认配置，`config check` 才能在不联网、
// 不读档案的前提下只校验配置写法，而展开这件事集中在本包一处。
package profileapply

import (
	"os"

	"github.com/sumwai/nova/internal/config"
	"github.com/sumwai/nova/internal/credentials"
	"github.com/sumwai/nova/internal/profile"
)

// Options 是装配层的三个外部依赖。
//
// 三者都作为入参注入，而不是就地读环境：StateDir 决定远端源的已装快照从哪读，
// ConfigDir 决定凭据库从哪读，Getenv 决定档案 auth.env 指向的凭据从哪取。
// 注入让「档案缺失」「凭据库里没有账号」「环境变量未设置」这三条分支
// 都能在测试里被完全控制。
type Options struct {
	// StateDir 是 nova 的状态目录，远端源从这里读已装快照；空表示没有可用快照。
	StateDir string

	// ConfigDir 是用户配置目录（${XDG_CONFIG_HOME:-~/.config}），凭据库取它下面的
	// nova/credentials.json；空表示不读凭据库，只按块内 api_key 与档案 auth.env 取凭据。
	ConfigDir string

	// Getenv 读环境变量，用于取档案 auth.env 指向的凭据。为零值时用 os.Getenv。
	Getenv func(string) string
}

// ToSources 把配置层的档案源转成档案层的纯数据。
//
// 转换只发生在装配层：internal/profile 不认识 internal/config，依赖方向始终是
// 「上层的本包」→「下层的两个实现包」。本函数是本仓库里唯一的转换出处，
// 命令行与展开路径因此不会各长出一份可能漂移的口径。
func ToSources(sources []config.ProfileSource) []profile.Source {
	converted := make([]profile.Source, 0, len(sources))
	for _, src := range sources {
		item := profile.Source{
			Kind: profile.SourceKind(src.Kind),
			URL:  src.URL,
			Path: src.Path,
		}
		for _, key := range src.Keys {
			item.Keys = append(item.Keys, profile.PublicKey{Name: key.Name, Key: key.PublicKey})
		}
		converted = append(converted, item)
	}
	return converted
}

// Load 读取并解析一份配置，再把其中的档案引用展开成具体声明。
//
// 它等价于 config.Load 加 Apply，是命令行侧「要一份可以直接装配的配置」时
// 应当调用的入口。档案源类命令（profiles update / list）不走这里：
// 它们要看的正是尚未展开的源声明，展开反而会因引用未同步的档案而失败。
func Load(path string, opts Options) (*config.Config, error) {
	cfg, err := config.LoadWith(path, config.Options{Getenv: opts.Getenv})
	if err != nil {
		return nil, err
	}
	if err := Apply(cfg, opts); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Apply 就地展开 cfg.Providers 里全部 ProfileRef 为真的条目，补全端点与凭据。
//
// 展开顺序按渠道的声明顺序：同一条渠道的端点、模型与凭据要一起补齐，
// 任一渠道失败即整体失败——一份半展开的配置比一份报错的配置更难排查。
//
// 幂等：展开后 ProfileRef 置回 false，因此对同一份配置调用两次不会重复展开。
// 档案 id 保留在 Profile 里，作为展开结果的出处标注，供报错与日志指回档案。
//
// 档案源装配失败只体现在「引用的档案找不到」上：某个源读不出来不中止其余源，
// 失败源与未同步源都会写进错误消息，让使用者一次看清该修哪一个。
func Apply(cfg *config.Config, opts Options) error {
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}

	registry, statuses, err := profile.LoadSources(
		ToSources(cfg.Profiles.Sources),
		&profile.Syncer{StateDir: opts.StateDir},
	)
	if err != nil {
		return err
	}

	store, err := loadCredentials(cfg, opts)
	if err != nil {
		return err
	}

	for i := range cfg.Providers {
		provider := &cfg.Providers[i]
		if !provider.ProfileRef {
			continue
		}
		loaded, ok := registry.Lookup(provider.Profile)
		if !ok {
			return missingProfileError(provider, statuses)
		}
		if err := expandProvider(provider, loaded, getenv, store, cfg); err != nil {
			return err
		}
	}
	return nil
}

// loadCredentials 读取凭据库；没有任何档案引用时不读磁盘。
//
// 只让需要凭据库的配置去读它：一份全部手写 api_key 的配置不该因为一份损坏的
// credentials.json 而无法校验。ConfigDir 为空时给一份没有落点的空存储，
// 展开层因此不必区分「没配凭据库」与「凭据库是空的」两种情况。
func loadCredentials(cfg *config.Config, opts Options) (*credentials.Store, error) {
	needed := false
	for i := range cfg.Providers {
		if cfg.Providers[i].ProfileRef {
			needed = true
			break
		}
	}
	if !needed {
		return credentials.New(), nil
	}
	if opts.ConfigDir == "" {
		return credentials.New(), nil
	}
	return credentials.Load(opts.ConfigDir)
}
