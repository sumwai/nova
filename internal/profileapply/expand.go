package profileapply

import (
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/sumwai/nova/internal/config"
	"github.com/sumwai/nova/internal/credentials"
	"github.com/sumwai/nova/internal/domain"
	"github.com/sumwai/nova/internal/limits"
	"github.com/sumwai/nova/internal/price"
	"github.com/sumwai/nova/internal/profile"
)

// detectProfile 按端点地址与档案的 api_pattern 认档案。
//
// 只对未写 profile 的手写渠道生效：写了 profile 就是显式意图，不再猜。命中一份即采用；
// 命中两份及以上时报错并列候选——「哪一份对」没有确定答案，静默取一份会让价格、额度
// 与模型集合都来自另一份档案。
//
// 命中 0 不提醒：内网中继与模拟上游的地址都会落进这一类，逐条提醒会把真正需要显式
// 写 profile 的那次歧义淹没在噪声里。
func detectProfile(registry *profile.Registry, provider *config.Provider) (*profile.Profile, error) {
	if provider.Profile != "" {
		return nil, nil
	}
	hits := map[string]*profile.Profile{}
	for _, id := range registry.IDs() {
		loaded, ok := registry.Lookup(id)
		if !ok || loaded.APIPattern == "" {
			continue
		}
		// 档案加载已校验 api_pattern 可编译；这里再判一次是因为展开层不依赖
		// 「上游校验一定跑过」，一份编译不了的档案不该让整次展开崩掉。
		pattern, err := regexp.Compile(loaded.APIPattern)
		if err != nil {
			continue
		}
		for _, endpoint := range provider.Endpoints {
			if endpoint.URL != "" && pattern.MatchString(endpoint.URL) {
				hits[id] = loaded
				break
			}
		}
	}

	switch len(hits) {
	case 0:
		return nil, nil
	case 1:
		for _, loaded := range hits {
			return loaded, nil
		}
	}

	ids := make([]string, 0, len(hits))
	for id := range hits {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return nil, locationError(provider,
		"provider %s 的端点地址命中多份档案（%s）；按地址自动认档案有歧义，请在块内显式写 `profile <id>`",
		provider.Name, strings.Join(ids, "、"))
}

// missingProfileError 报告一条渠道引用的档案不在任何源里。
//
// 消息必须回答三个问题：看过了哪些源、远端源是不是还没同步、下一步先跑什么。
// 只报「档案不存在」会把「源里确实没有」与「源还没同步过」混成一句，
// 而这两者的处置完全不同：前者要改档案 id，后者要先跑 nova profiles update。
func missingProfileError(provider *config.Provider, statuses []profile.SourceStatus) error {
	return locationError(provider,
		"provider %s 引用的档案 %q 不在任何源里：%s；远端源尚未同步时先跑 `nova profiles update`，"+
			"再用 `nova profiles list` 核对档案 id",
		provider.Name, provider.Profile, describeSources(statuses))
}

// describeSources 把各源的装配结果排成人读文本，供「档案缺失」定位。
//
// 三种状态分开写：读取失败的源给出原因，未同步的远端源明确说「尚未同步」，
// 其余源报它贡献了几份档案。把它们并成一句计数会让「源里有几份」与
// 「源根本没读过」看起来一样。
func describeSources(statuses []profile.SourceStatus) string {
	if len(statuses) == 0 {
		return "没有任何档案源"
	}
	parts := make([]string, 0, len(statuses))
	for _, status := range statuses {
		switch {
		case status.Err != nil:
			parts = append(parts, fmt.Sprintf("%s（读取失败：%v）", status.Label, status.Err))
		case status.Kind == profile.SourceRemote && !status.Synced:
			parts = append(parts, status.Label+"（尚未同步）")
		default:
			parts = append(parts, fmt.Sprintf("%s（%d 份）", status.Label, status.Count))
		}
	}
	return "已检查 " + strings.Join(parts, "、")
}

// expandProvider 把一份档案展开进一条渠道，并保留块内手写的声明。
//
// 展开顺序有意如此：先按档案铺端点与模型，再把手写端点合并进来，最后补凭据。
// 端点先于模型，是为了让模型能按档案里声明的端点名找到落点；凭据最后，
// 是因为块内已写凭据时档案的 auth.env 根本不参与。
func expandProvider(
	provider *config.Provider,
	loaded *profile.Profile,
	getenv func(string) string,
	store *credentials.Store,
	cfg *config.Config,
) error {
	endpoints, byName, err := profileEndpoints(provider, loaded)
	if err != nil {
		return err
	}
	if err := attachProfileModels(provider, loaded, endpoints, byName); err != nil {
		return err
	}
	endpoints, err = mergeManualEndpoints(provider, endpoints, cfg)
	if err != nil {
		return err
	}
	if err := fillCredentials(provider, loaded, getenv, store, cfg); err != nil {
		return err
	}
	attachPlanLimits(provider, loaded, cfg)
	if err := fillChannelAuth(provider, loaded); err != nil {
		return err
	}

	provider.Endpoints = endpoints
	// 展开完成即清掉引用标记，Apply 因此幂等；Profile 保留作出处标注。
	provider.ProfileRef = false
	return nil
}

// attachPlanLimits 把档案 plans 的静态限制写进渠道的每个账号。
//
// 额度声明是账号级事实：同一个档案下的多个账号共用同一份计划限制，凭据不同而已。
// 档案只有一个 plan 时直接绑定；多个 plan 时本版取第一个并记一条提醒——按账号选 plan
// 需要「账号名 → plan id」的映射，而配置语法还没有承载它的位置，静默取第一个会让
// 「为什么这个账号用的是另一套限制」无从回答。
func attachPlanLimits(provider *config.Provider, loaded *profile.Profile, cfg *config.Config) {
	if len(loaded.Plans) == 0 {
		return
	}
	if len(loaded.Plans) > 1 {
		cfg.Warnings = append(cfg.Warnings, providerWarning(provider,
			"provider %s 的档案 %s 声明了 %d 个 plan，本版取第一个 %q；暂不支持按账号选 plan",
			provider.Name, loaded.ID, len(loaded.Plans), loaded.Plans[0].ID))
	}
	declared := limits.FromDoc(&profile.LimitsDoc{
		Schema:  profile.CurrentSchema,
		Source:  "profile",
		Account: loaded.Plans[0].Limits,
	})
	if len(declared) == 0 {
		return
	}
	for i := range provider.Accounts {
		provider.Accounts[i].Limits = declared
	}
}

// profileEndpoints 按档案声明生成端点。
//
// 档案里的端点是 map，Go 的 map 不保留声明顺序；模型声明里的端点引用是有序的，
// 因此端点按它首次出现的顺序排，未被任何模型引用的再按名字排序接在后面。
func profileEndpoints(
	provider *config.Provider,
	loaded *profile.Profile,
) ([]config.Endpoint, map[string]int, error) {
	names := orderedEndpointNames(loaded)

	endpoints := make([]config.Endpoint, 0, len(names))
	byName := make(map[string]int, len(names))
	for _, name := range names {
		spec := loaded.Endpoints[name]
		protocol := domain.Protocol(spec.Protocol)
		if !protocol.Valid() {
			return nil, nil, locationError(provider,
				"档案 %s 的端点 %s 协议 %q 不是本二进制支持的线协议", loaded.ID, name, spec.Protocol)
		}
		// 档案级 discover 与 gemini 互斥：Gemini 的清单响应形状与端点地址都不同，
		// 展开一个这样的端点只会在运行期变成一个发不出去的地址。
		if protocol == domain.ProtocolGemini && loaded.Discover != nil {
			return nil, nil, locationError(provider,
				"档案 %s 的端点 %s 是 gemini 协议，本版不支持 discover：请改用 model 逐条声明模型",
				loaded.ID, name)
		}
		byName[name] = len(endpoints)
		endpoints = append(endpoints, config.Endpoint{
			URL:      spec.URL,
			Protocol: protocol,
			Timeout:  config.DefaultTimeout,
			File:     provider.File,
			Line:     provider.Line,
			Col:      provider.Col,
		})
	}
	return endpoints, byName, nil
}

// orderedEndpointNames 按档案声明的优先级列出端点名。
//
// 模型声明了它引用哪些端点，那些引用是有序的（如 [openai_chat, anthropic_messages]），
// 它就是这个平台上的协议优先级；端点顺序即回退顺序，不能只按名字排序改掉它。
// 先用模型引用首次出现的顺序排出被引用的端点，未被任何模型引用的端点再按名字排序接在后。
func orderedEndpointNames(loaded *profile.Profile) []string {
	seen := make(map[string]bool, len(loaded.Endpoints))
	names := make([]string, 0, len(loaded.Endpoints))
	for _, model := range loaded.Models {
		for _, name := range model.Endpoints {
			if seen[name] {
				continue
			}
			// 档案层已校验引用存在；展开层不依赖上游校验一定跑过，越界取不到就跳过。
			if _, ok := loaded.Endpoints[name]; !ok {
				continue
			}
			seen[name] = true
			names = append(names, name)
		}
	}
	rest := make([]string, 0, len(loaded.Endpoints))
	for name := range loaded.Endpoints {
		if !seen[name] {
			rest = append(rest, name)
		}
	}
	sort.Strings(rest)
	return append(names, rest...)
}

// attachProfileModels 把档案声明的模型挂到对应端点上。
//
// 对外名取 public，未写时取 id；上游名恒为 id——档案里的 id 就是发往上游的名字，
// public 是给客户端看的别名。模型未声明 endpoints 时挂到档案的全部端点上。
func attachProfileModels(
	provider *config.Provider,
	loaded *profile.Profile,
	endpoints []config.Endpoint,
	byName map[string]int,
) error {
	all := make([]string, 0, len(byName))
	for name := range byName {
		all = append(all, name)
	}
	sort.Strings(all)

	for _, model := range loaded.Models {
		public := model.Public
		if public == "" {
			public = model.ID
		}
		targets := model.Endpoints
		if len(targets) == 0 {
			targets = all
		}
		for _, target := range targets {
			index, ok := byName[target]
			if !ok {
				// 档案层的跨字段校验已挡过这一条；这里再判一次是因为展开层
				// 不能依赖「上游校验一定跑过」，报错也比越界写入可读。
				return locationError(provider,
					"档案 %s 的模型 %s 引用了不存在的端点 %q", loaded.ID, model.ID, target)
			}
			if err := appendProfileModel(provider, &endpoints[index], loaded.ID, target, public, model.ID,
				declaredPrice(loaded.ID, model)); err != nil {
				return err
			}
		}
	}
	return nil
}

// appendProfileModel 往一条端点追加模型，并拒绝端点内重名。
//
// 口径与配置层的 appendModel 一致：同一个端点里出现两个同名对外名时，
// 它们在选路上没有可区分的含义，报错比让其中一个静默失效更省事。
func appendProfileModel(
	provider *config.Provider,
	endpoint *config.Endpoint,
	profileID, endpointName, name, upstream string,
	declared price.Declared,
) error {
	for _, existing := range endpoint.Models {
		if existing.Name != name {
			continue
		}
		return locationError(provider,
			"档案 %s 的端点 %s 里对外模型名 %q 重复；同一个端点里再写一次没有可区分的含义",
			profileID, endpointName, name)
	}
	endpoint.Models = append(endpoint.Models, config.Model{Name: name, Upstream: upstream, Price: declared})
	return nil
}

// declaredPrice 把档案里的模型价格声明转成中性类型。
//
// 键写成 <档案 id>/<模型 id>，与 price_from 的引用形式同一命名空间；上游 id 才是模型 id，
// 对外名（public）不是。free / price / price_from 三者原样搬运，优先级由价格层判定。
func declaredPrice(profileID string, model profile.Model) price.Declared {
	declared := price.Declared{
		Key:  price.FormatKey(profileID, model.ID),
		Free: model.Free,
		From: model.PriceFrom,
	}
	if model.Price != nil {
		declared.Currency = model.Price.Currency
		declared.Unit = &price.Unit{
			Currency:       model.Price.Currency,
			InputMTok:      model.Price.InputMTok,
			OutputMTok:     model.Price.OutputMTok,
			CacheReadMTok:  model.Price.CacheReadMTok,
			CacheWriteMTok: model.Price.CacheWriteMTok,
			ReasoningMTok:  model.Price.ReasoningMTok,
		}
	}
	return declared
}

// mergeManualEndpoints 把块内手写的端点合并到档案端点上。
//
// 合并依据是协议：同协议的多条端点按出现顺序配对，块内 url 覆盖配对到的档案端点地址，
// 并记一条提醒——「配置写的那条地址没有生效」必须被看见。块内写了档案没有的协议时
// 同样只记提醒、保留该端点：多出来的端点可能正是这份配置想额外加的一条路径。
//
// 同协议的多条手写端点不塌成一条：档案里该协议的端点排成队列，手写端点依次认领，
// 认领完的手写端点作为独立端点追加。只认「该协议的第一条」会让第二条覆盖第一条的地址。
//
// 合并后的端点顺序保持档案生成的顺序，新增的手写端点接在后面：
// 顺序即回退顺序，不能因为一次合并把已有候选的先后打乱。
func mergeManualEndpoints(
	provider *config.Provider,
	endpoints []config.Endpoint,
	cfg *config.Config,
) ([]config.Endpoint, error) {
	queues := make(map[domain.Protocol][]int, len(endpoints))
	for i, endpoint := range endpoints {
		queues[endpoint.Protocol] = append(queues[endpoint.Protocol], i)
	}
	paired := make(map[domain.Protocol]int, len(queues))

	for _, manual := range provider.Endpoints {
		queue := queues[manual.Protocol]
		next := paired[manual.Protocol]
		if next >= len(queue) {
			if len(queue) == 0 {
				cfg.Warnings = append(cfg.Warnings, providerWarning(provider,
					"provider %s 的端点 %s 的协议 %s 不在档案 %s 里，这条端点为配置独有，档案不提供它的模型",
					provider.Name, manual.URL, manual.Protocol, provider.Profile))
			} else {
				cfg.Warnings = append(cfg.Warnings, providerWarning(provider,
					"provider %s 的端点 %s 的协议 %s 已有同协议的档案端点被前一条手写端点配对；"+
						"这条端点作为配置独有的端点保留，不覆盖任何档案端点",
					provider.Name, manual.URL, manual.Protocol))
			}
			endpoints = append(endpoints, manual)
			continue
		}
		paired[manual.Protocol] = next + 1
		index := queue[next]

		cfg.Warnings = append(cfg.Warnings, providerWarning(provider,
			"provider %s 的端点 %s 覆盖了档案 %s 里同一协议端点的地址，以块内 url 为准",
			provider.Name, manual.URL, provider.Profile))

		merged := endpoints[index]
		merged.URL = manual.URL
		if manual.Timeout > 0 {
			merged.Timeout = manual.Timeout
		}
		for _, model := range manual.Models {
			if err := mergeManualModel(provider, &merged, model); err != nil {
				return nil, err
			}
		}
		endpoints[index] = merged
	}
	return endpoints, nil
}

// mergeManualModel 把手写模型并入一条已挂上档案模型的端点。
//
// 与 appendProfileModel 的差别在重名时的处置：档案自身的两条声明重名是错误，
// 而手写声明与档案声明同名，在按地址自动认档案时会大量出现——使用者写下地址与
// 模型名，档案再补上价格与额度。此时保留档案那条，只要求两处对「上游模型名」
// 的看法一致；不一致就是「同一个对外名指向两个上游模型」的语义冲突，报错要比
// 静默选一边诚实。
func mergeManualModel(provider *config.Provider, endpoint *config.Endpoint, manual config.Model) error {
	for i := range endpoint.Models {
		existing := &endpoint.Models[i]
		if existing.Name != manual.Name {
			continue
		}
		if manual.Upstream != existing.Upstream {
			return locationError(provider,
				"模型 %s 在手写端点里指向上游 %s，档案 %s 的同名模型指向上游 %s；"+
					"同一个对外名不能同时指两个上游模型，请改名或删掉手写声明",
				manual.Name, manual.Upstream, provider.Profile, existing.Upstream)
		}
		return nil
	}
	endpoint.Models = append(endpoint.Models, manual)
	return nil
}

// fillCredentials 按「块内 api_key > 凭据库 > 档案 auth.env」的顺序取凭据。
//
// 三条来源的优先级是有意的：块内写下的 api_key 是这份配置的明确意图，优先于任何
// 外部状态；凭据库是本机为这个平台存好的账号；档案 auth.env 是凭据库落地前的桥，
// 供不落盘的部署使用。三者都没有时报错，并同时给出两个下一步：跑 nova login 添加
// 账号，或在块内写 api_key {env.NAME}。
//
// account 指令可以写多条，顺序即账号顺序，权重按现有 balance 口径生效：展开后每个
// 账号都是渠道的一个账号条目，同一个渠道因此能分摊到同一个档案的多份凭据上。
func fillCredentials(
	provider *config.Provider,
	loaded *profile.Profile,
	getenv func(string) string,
	store *credentials.Store,
	cfg *config.Config,
) error {
	explicit, named := splitAccounts(provider.Accounts)
	if len(explicit) > 0 {
		provider.Accounts = explicit
		warnIgnoredStoreAccounts(provider, loaded, store, cfg)
		return nil
	}
	if len(named) > 0 {
		resolved, err := resolveNamedAccounts(provider, loaded, named, store)
		if err != nil {
			return err
		}
		provider.Accounts = resolved
		return nil
	}

	// 块内没有凭据声明：先看凭据库里这个档案有哪些账号。
	if entries := store.Entries(loaded.ID); len(entries) > 0 {
		provider.Accounts = accountsFromEntries(provider, entries)
		return nil
	}

	env := loaded.Auth.Env
	if env == "" {
		return locationError(provider,
			"provider %s 没有可用凭据：块内没有 api_key，档案 %s 在凭据库里没有任何账号，"+
				"档案也没有声明 auth.env；可跑 `nova login %s` 添加账号，或在块内写 api_key {env.NAME}",
			provider.Name, loaded.ID, loaded.ID)
	}
	value := getenv(env)
	if value == "" {
		return locationError(provider,
			"环境变量 %s 未设置或为空（档案 %s 的凭据来自它）；可跑 `nova login %s` 添加账号，"+
				"或在块内写 api_key {env.NAME}",
			env, loaded.ID, loaded.ID)
	}
	provider.Accounts = []config.Account{{
		APIKey: value,
		Weight: 1,
		Index:  1,
		File:   provider.File,
		Line:   provider.Line,
		Col:    provider.Col,
	}}
	return nil
}

// splitAccounts 把账号声明拆成「块内显式密钥」与「引用凭据库」两类。
func splitAccounts(accounts []config.Account) (explicit, named []config.Account) {
	for _, account := range accounts {
		if account.Name != "" {
			named = append(named, account)
			continue
		}
		explicit = append(explicit, account)
	}
	return explicit, named
}

// warnIgnoredStoreAccounts 在块内写了 api_key、凭据库里又存有同档案账号时记一条提醒。
//
// 「改了文件却没生效」是本层最难自查的一类现象：api_key 静默胜出后，凭据库里的账号
// 看上去一切正常。提醒把被忽略的账号名列出来，让这份凭据库当前的处境可见。
func warnIgnoredStoreAccounts(
	provider *config.Provider,
	loaded *profile.Profile,
	store *credentials.Store,
	cfg *config.Config,
) {
	names := store.Names(loaded.ID)
	if len(names) == 0 {
		return
	}
	cfg.Warnings = append(cfg.Warnings, providerWarning(provider,
		"provider %s 块内写了 api_key，档案 %s 在凭据库里的账号（%s）被忽略；"+
			"要改用凭据库，删掉块内 api_key",
		provider.Name, loaded.ID, strings.Join(names, "、")))
}

// resolveNamedAccounts 按 account 行给出的名字从凭据库取密钥。
//
// 账号名在凭据库里查不到时整体失败：少一个账号会让这个渠道的凭据池与配置里写下的
// 台账不一致，而这类不一致只会以「有一份凭据从未被用过」的形式滞后暴露。
func resolveNamedAccounts(
	provider *config.Provider,
	loaded *profile.Profile,
	named []config.Account,
	store *credentials.Store,
) ([]config.Account, error) {
	resolved := make([]config.Account, 0, len(named))
	for _, account := range named {
		key, ok := store.Lookup(loaded.ID, account.Name)
		if !ok {
			return nil, missingAccountError(provider, loaded, store, account.Name)
		}
		account.APIKey = key
		resolved = append(resolved, account)
	}
	return resolved, nil
}

// missingAccountError 报告 account 行引用的账号不在凭据库里。
//
// 消息必须回答两个问题：这个档案在凭据库里到底有哪些账号，以及下一步敲什么。
// 只说「账号不存在」会让人对着一个空凭据库反复改帐号名。
func missingAccountError(
	provider *config.Provider,
	loaded *profile.Profile,
	store *credentials.Store,
	name string,
) error {
	available := store.Names(loaded.ID)
	var state string
	if len(available) == 0 {
		state = fmt.Sprintf("档案 %s 在凭据库里还没有任何账号", loaded.ID)
	} else {
		state = fmt.Sprintf("档案 %s 在凭据库里有的账号：%s", loaded.ID, strings.Join(available, "、"))
	}
	return locationError(provider,
		"provider %s 引用的账号 %q 不在凭据库里；%s；可跑 `nova login %s %s` 添加",
		provider.Name, name, state, loaded.ID, name)
}

// accountsFromEntries 把凭据库里的账号变成渠道的账号条目。
//
// 存在名为 default 的账号时只取它：多数档案只配一份凭据，让「不写 account 就取哪一份」
// 有一个确定答案，不必把凭据库里的全部账号都摊进这条渠道。没有 default 时才取全部，
// 顺序与权重按凭据库的加入顺序与缺省权重给出。
func accountsFromEntries(provider *config.Provider, entries []credentials.Entry) []config.Account {
	if index := indexOfDefault(entries); index >= 0 {
		entries = entries[index : index+1]
	}
	accounts := make([]config.Account, 0, len(entries))
	for i, entry := range entries {
		accounts = append(accounts, config.Account{
			APIKey: entry.Key,
			Name:   entry.Name,
			Weight: 1,
			Index:  i + 1,
			File:   provider.File,
			Line:   provider.Line,
			Col:    provider.Col,
		})
	}
	return accounts
}

// indexOfDefault 返回名为 default 的账号下标，没有则返回 -1。
func indexOfDefault(entries []credentials.Entry) int {
	for i, entry := range entries {
		if entry.Name == "default" {
			return i
		}
	}
	return -1
}

// fillChannelAuth 把档案声明的凭据注入形态与静态请求头写进渠道。
//
// 运行期默认按协议猜注入形态（Anthropic 用 x-api-key），但同一个平台的 Anthropic
// 兼容端点未必认这个头；档案的 auth.header 是平台事实，优先于按协议猜。headers 同理：
// 它是平台要求的静态头，不写进渠道就会在每次请求里静默缺失。
func fillChannelAuth(provider *config.Provider, loaded *profile.Profile) error {
	if len(loaded.Headers) > 0 {
		headers := make(http.Header, len(loaded.Headers))
		for name, value := range loaded.Headers {
			headers.Set(name, value)
		}
		provider.Headers = headers
	}
	if loaded.Auth.Header == "" {
		return nil
	}
	style, err := credentialStyle(provider, loaded)
	if err != nil {
		return err
	}
	provider.CredentialHeaderStyle = style
	return nil
}

// credentialStyle 把档案的 auth.header / auth.scheme 映射成运行期的注入形态。
//
// 映射不上的取值在展开期报错，而不是留给运行期按协议猜：档案写下的注入形态与
// 实际发出的头相反时，上游只会回一个 401，而从状态码看不出是「头写错了」。
func credentialStyle(provider *config.Provider, loaded *profile.Profile) (domain.CredentialHeaderStyle, error) {
	switch strings.ToLower(loaded.Auth.Header) {
	case "authorization":
		if loaded.Auth.Scheme != "" && !strings.EqualFold(loaded.Auth.Scheme, "Bearer") {
			return "", locationError(provider,
				"档案 %s 声明 auth.header=authorization、scheme=%s；本版只支持 Bearer 方案，无法按声明注入凭据",
				loaded.ID, loaded.Auth.Scheme)
		}
		return domain.CredentialHeaderAuthorization, nil
	case "x-api-key":
		return domain.CredentialHeaderXAPIKey, nil
	case "x-goog-api-key":
		return domain.CredentialHeaderXGoogAPIKey, nil
	default:
		return "", locationError(provider,
			"档案 %s 的 auth.header=%q 不是本版支持的注入形态（authorization / x-api-key / x-goog-api-key）",
			loaded.ID, loaded.Auth.Header)
	}
}

// locationError 构造一条指回 provider 块头的配置错误。
//
// 展开期的错误都发生在档案与渠道的交界上，指向渠道声明处比指向档案文件更有用：
// 使用者改的是自己的配置，而不是随二进制发布的档案。
func locationError(provider *config.Provider, format string, args ...any) error {
	return &config.Error{
		File: provider.File,
		Line: provider.Line,
		Col:  provider.Col,
		Msg:  fmt.Sprintf(format, args...),
	}
}

// providerWarning 构造一条指回 provider 块头的提醒。
func providerWarning(provider *config.Provider, format string, args ...any) config.Warning {
	return config.Warning{
		File: provider.File,
		Line: provider.Line,
		Msg:  fmt.Sprintf(format, args...),
	}
}
