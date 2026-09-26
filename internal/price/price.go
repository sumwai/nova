// Package price 定义模型价格的三态、按量单价与排序用的估计成本。
//
// 本包只依赖标准库：价格的中性声明由 config 携带，档案的定型解析由 profile 负责，
// 依赖方向因此是 config → price →（无 profile 依赖），与额度层同一形状。
//
// 三态且禁止推导：Free 必须由档案显式声明（free: true），Known 来自一条带单价的
// 条目，其余一律是 Unknown。**查表失败是 Unknown，不是 Free**——把「没查到价格」
// 当成「免费」会把一个未知成本的候选排到所有已知价之前，那正是本层要避免的错误。
package price

import (
	"sort"
	"strings"
)

// Kind 是价格的三态。
type Kind int

const (
	// Unknown 表示查不到价格；参与排序时用名义价，并标记为假定值。
	Unknown Kind = iota
	// Free 表示显式免费（档案写 free: true），排序成本为 0。
	Free
	// Known 表示有明确单价。
	Known
)

// String 返回三态的可读写法，用于日志与提醒。
func (k Kind) String() string {
	switch k {
	case Free:
		return "free"
	case Known:
		return "known"
	default:
		return "unknown"
	}
}

// perMillion 是单价的计价单位：每百万 token。
const perMillion = 1_000_000

// DefaultCurrency 是未声明币种时的缺省币种。
const DefaultCurrency = "USD"

// DefaultOutputTokens 是未给出输出估计时用于排序的缺省输出 token 数。
//
// 它只影响排序（把不同单价折算成一个可比较的成本），不进入任何计费口径。
const DefaultOutputTokens = 800

// Unit 是一条按量单价：币种加每百万 token 的价格。
//
// 五个价格分量与档案里的字段一一对应；排序只看输入与输出两项，其余留给统计口径。
type Unit struct {
	Currency       string
	InputMTok      float64
	OutputMTok     float64
	CacheReadMTok  float64
	CacheWriteMTok float64
	ReasoningMTok  float64
}

// empty 报告这条单价有没有任何价格信息。
//
// 全零的单价不被当成 Known：设计里「免费」必须显式写 free，一条全零的 price 对象
// 只会被当成查不到价格，从而按 Unknown 处理，排在名义价上。
func (u Unit) empty() bool {
	return u.InputMTok == 0 && u.OutputMTok == 0 &&
		u.CacheReadMTok == 0 && u.CacheWriteMTok == 0 && u.ReasoningMTok == 0
}

// Declared 是一条不依赖 profile 的中性价格声明。
//
// Key 是条目在价格表里的键，形如 <档案 id>/<模型 id>。Free、Unit、From 三者按
// 声明里的优先级取值：free 最高，其次直接单价，再次 price_from 指向的另一个键。
// Currency 在 Unit 为空时仍有意义：它决定这条 Unknown 条目在排序时归入哪个币种组。
type Declared struct {
	Key      string
	Free     bool
	Currency string
	Unit     *Unit
	From     string
}

// Usage 是估计成本用的用量口径：输入与输出 token。
//
// 排序用固定估计（输出取缺省值），真实用量在请求路径上另有来源，本类型只服务排序。
type Usage struct {
	Input  int
	Output int
}

// Entry 是解析后的三态条目。
//
// Kind 为 Unknown 时 Unit 填的是名义价、Assumed 为真：调用方据此知道这条条目
// 参与了排序但值不是权威数据，输出里应标 assumed。
type Entry struct {
	Kind    Kind
	Unit    Unit
	Assumed bool
	// Source 记录这条条目来自哪里：profile / file / builtin / unknown。
	Source string
}

// Options 是构造 Table 的选项。
type Options struct {
	// Currency 是声明与文件条目未写币种时的缺省币种；空表示 USD。
	Currency string

	// OutputTokens 是未给出输出估计时的缺省输出 token 数；<= 0 表示 800。
	OutputTokens int

	// Nominal 是 Unknown 条目参与排序时用的名义价；nil 或全零时取已知按量档的中位数。
	//
	// 它只在缺省币种上生效：名义价是给「查不到价格」的条目一个可比较的挡位，
	// 而不是一条真实单价；币种为空时归入缺省币种。
	Nominal *Unit
}

// withDefaults 补齐选项缺省值。
func withDefaults(opts Options) Options {
	if opts.Currency == "" {
		opts.Currency = DefaultCurrency
	}
	if opts.OutputTokens <= 0 {
		opts.OutputTokens = DefaultOutputTokens
	}
	return opts
}

// Table 是一张价格表：键到三态条目的映射。
//
// 表在装配期一次建成，运行期只读，因此不需要锁。构建顺序是有意的——先解析全部
// 声明与文件条目，再按币种算名义价，最后把名义价填进 Unknown 条目，这样 Unknown
// 在排序里落在已知价之间，而不是被无条件排到最后。
type Table struct {
	opts    Options
	entries map[string]Entry
	// unresolved 是 price_from 指向了但三处来源都没有的键。它服务装配期提醒：
	// 一条悬空引用与「这个模型本来就没价」在查表结果上都是 Unknown，只有把
	// 悬空这件事单独记下来，提醒才不必靠猜。
	unresolved map[string]bool
}

// builtin 是内置价格表。
//
// 本版为空：价格数字必须有实测来源，凭空写进二进制的价格会变成一个看起来权威、
// 实则无从核对的数。内置表落地后由档案或 prices.file 之外的那一份填入。
var builtin = map[string]Unit{}

// Build 由三处来源按优先级合成一张价格表。
//
// 优先级：档案里的 price/free/price_from > prices { file } > 内置表。查不到任何
// 来源的键是 Unknown，不是 Free。
func Build(declared []Declared, external map[string]Unit, opts Options) *Table {
	table := &Table{opts: withDefaults(opts), entries: map[string]Entry{}, unresolved: map[string]bool{}}

	declaredByKey := make(map[string]Declared, len(declared))
	for _, item := range declared {
		if item.Key == "" {
			continue
		}
		// 同一个键写两次（同一份档案模型挂到多条端点上）取先声明的那条：
		// 两份声明不一致时，「哪一份赢」没有正确答案，取先出现的保持确定性。
		if _, ok := declaredByKey[item.Key]; ok {
			continue
		}
		declaredByKey[item.Key] = item
	}

	keys := make([]string, 0, len(declaredByKey)+len(external))
	for key := range declaredByKey {
		keys = append(keys, key)
	}
	for key := range external {
		if _, ok := declaredByKey[key]; ok {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		table.entries[key] = table.resolve(key, declaredByKey, external, map[string]bool{})
	}

	table.applyNominal()
	return table
}

// resolve 解析一个键，带记忆与循环引用检测。
func (t *Table) resolve(key string, declared map[string]Declared, external map[string]Unit, stack map[string]bool) Entry {
	if entry, ok := t.entries[key]; ok {
		return entry
	}
	if stack[key] {
		// 循环引用：查表失败，按 Unknown 处理，不写缓存以免污染其他路径。
		return Entry{Kind: Unknown, Unit: Unit{Currency: t.opts.Currency}, Assumed: true, Source: "unknown"}
	}
	stack[key] = true
	defer delete(stack, key)

	entry := t.resolveUncached(key, declared, external, stack)
	t.entries[key] = entry
	return entry
}

// resolveUncached 按优先级解析一个键。
func (t *Table) resolveUncached(key string, declared map[string]Declared, external map[string]Unit, stack map[string]bool) Entry {
	if spec, ok := declared[key]; ok {
		currency := spec.Currency
		if currency == "" {
			currency = t.opts.Currency
		}
		switch {
		case spec.Free:
			return Entry{Kind: Free, Unit: Unit{Currency: currency}, Source: "profile"}
		case spec.Unit != nil && !spec.Unit.empty():
			unit := *spec.Unit
			if unit.Currency == "" {
				unit.Currency = currency
			}
			return Entry{Kind: Known, Unit: unit, Source: "profile"}
		case spec.From != "":
			referenced := t.resolve(spec.From, declared, external, stack)
			if referenced.Kind != Unknown {
				return referenced
			}
			// 引用目标根本没被任何来源声明过才算悬空；目标存在只是自己也没价，
			// 那是那张表的问题，不是这条引用写错了键。
			if !t.declares(spec.From, declared, external) {
				t.unresolved[spec.From] = true
			}
		}
	}
	if unit, ok := external[key]; ok && !unit.empty() {
		if unit.Currency == "" {
			unit.Currency = t.opts.Currency
		}
		return Entry{Kind: Known, Unit: unit, Source: "file"}
	}
	if unit, ok := builtin[key]; ok && !unit.empty() {
		if unit.Currency == "" {
			unit.Currency = t.opts.Currency
		}
		return Entry{Kind: Known, Unit: unit, Source: "builtin"}
	}
	return Entry{
		Kind:    Unknown,
		Unit:    Unit{Currency: t.unknownCurrency(key, declared, external)},
		Assumed: true,
		Source:  "unknown",
	}
}

// unknownCurrency 取一条 Unknown 条目应归入的币种。
//
// 声明里写了币种就用它；文件条目写了币种也用它；都没有时用缺省币种。这条取值只
// 决定排序时的分组，不代表任何真实单价。
func (t *Table) unknownCurrency(key string, declared map[string]Declared, external map[string]Unit) string {
	if spec, ok := declared[key]; ok && spec.Currency != "" {
		return spec.Currency
	}
	if unit, ok := external[key]; ok && unit.Currency != "" {
		return unit.Currency
	}
	return t.opts.Currency
}

// applyNominal 给 Unknown 条目填上名义价。
//
// 名义价默认按币种取已知按量档单价的各分量中位数，可由 Options.Nominal 覆盖。
// 它保证 Unknown 参与排序时落在已知价之间，而不是被静默排到所有已知价之后；
// 标记 Assumed 让调用方能把这一点报出去。
func (t *Table) applyNominal() {
	for key, entry := range t.entries {
		if entry.Kind != Unknown {
			continue
		}
		entry.Unit = t.nominalFor(entry.Unit.Currency)
		entry.Assumed = true
		t.entries[key] = entry
	}
}

// nominalFor 返回某个币种下 Unknown 条目使用的名义价。
//
// Options.Nominal 只覆盖它声明的币种（未声明币种时覆盖缺省币种）；其余情况取已知
// 按量档的中位数；一个已知价都没有时返回零值单价，此时同一币种内全部条目同价，
// 相对顺序由声明顺序决定。
func (t *Table) nominalFor(currency string) Unit {
	if nominal := t.opts.Nominal; nominal != nil && !nominal.empty() {
		if nominal.Currency == "" || nominal.Currency == currency {
			unit := *nominal
			if unit.Currency == "" {
				unit.Currency = t.opts.Currency
			}
			return unit
		}
	}
	if known, ok := t.nominalUnits()[currency]; ok {
		return known
	}
	return Unit{Currency: currency}
}

// nominalUnits 逐币种算已知按量档的中位数单价。
func (t *Table) nominalUnits() map[string]Unit {
	rates := map[string][5][]float64{}
	for _, entry := range t.entries {
		if entry.Kind != Known {
			continue
		}
		currency := entry.Unit.Currency
		bucket := rates[currency]
		bucket[0] = append(bucket[0], entry.Unit.InputMTok)
		bucket[1] = append(bucket[1], entry.Unit.OutputMTok)
		bucket[2] = append(bucket[2], entry.Unit.CacheReadMTok)
		bucket[3] = append(bucket[3], entry.Unit.CacheWriteMTok)
		bucket[4] = append(bucket[4], entry.Unit.ReasoningMTok)
		rates[currency] = bucket
	}
	out := make(map[string]Unit, len(rates))
	for currency, bucket := range rates {
		out[currency] = Unit{
			Currency:       currency,
			InputMTok:      median(bucket[0]),
			OutputMTok:     median(bucket[1]),
			CacheReadMTok:  median(bucket[2]),
			CacheWriteMTok: median(bucket[3]),
			ReasoningMTok:  median(bucket[4]),
		}
	}
	return out
}

// median 取一组数的中位数；空集返回 0。
func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	middle := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[middle]
	}
	return (sorted[middle-1] + sorted[middle]) / 2
}

// Lookup 返回一个键的三态条目；未登记的键是 Unknown 且已带名义价。
func (t *Table) Lookup(key string) Entry {
	if entry, ok := t.entries[key]; ok {
		return entry
	}
	// 未登记的键也要带上名义价：否则它在排序里会退化成 0，看起来像免费。
	return Entry{Kind: Unknown, Unit: t.nominalFor(t.opts.Currency), Assumed: true, Source: "unknown"}
}

// declares 报告一个键是否被任何一处来源声明过（不看它最终解析成什么）。
func (t *Table) declares(key string, declared map[string]Declared, external map[string]Unit) bool {
	if _, ok := declared[key]; ok {
		return true
	}
	if _, ok := external[key]; ok {
		return true
	}
	_, ok := builtin[key]
	return ok
}

// UnresolvedReferences 返回被 price_from 引用但没有任何来源声明的键，按字典序。
//
// 装配层用它在启动时记一条提醒：悬空引用与「查不到价格」在选路上都退化成 Unknown，
// 不单独说一句，写错键这件事就只在排序结果里匿名地体现出来。
func (t *Table) UnresolvedReferences() []string {
	out := make([]string, 0, len(t.unresolved))
	for key := range t.unresolved {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// Currency 返回表的缺省币种。
func (t *Table) Currency() string {
	return t.opts.Currency
}

// DefaultUsage 返回排序用的固定用量估计：输入 0，输出取缺省 token 数。
func (t *Table) DefaultUsage() Usage {
	return Usage{Output: t.opts.OutputTokens}
}

// Estimate 按单价与用量估算一次请求的成本。
//
// 免费条目恒为 0；已知条目按输入与输出两项线性折算；Unknown 条目用的是表里填好的
// 名义价，因此同一个函数对三态都给出可比较的标量。不同币种的估计值不可互相比较，
// 分组由调用方负责。
func Estimate(entry Entry, usage Usage) float64 {
	if entry.Kind == Free {
		return 0
	}
	return unitCost(entry.Unit, usage)
}

// unitCost 按单价折算一次用量的成本。
func unitCost(unit Unit, usage Usage) float64 {
	return float64(usage.Input)*unit.InputMTok/perMillion +
		float64(usage.Output)*unit.OutputMTok/perMillion
}

// FormatKey 把档案 id 与模型 id 拼成价格条目的键。
//
// 键只在这一个函数里成形：构建与引用两处若各拼一次，改动命名空间时会漏改一处。
func FormatKey(profileID, modelID string) string {
	return strings.Join([]string{profileID, modelID}, "/")
}
