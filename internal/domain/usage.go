package domain

// UsageSource 标识一份用量数据的来源，决定它能否直接用于结算。
//
// 它存在的唯一目的，是把「用量确实为零」与「没有取得用量」分开。零值是UsageSourceUnknown，
// 因此任何未显式赋值的 Usage 都表示未取得用量，不会被误当成「用量为零、无需结算」。
type UsageSource string

const (
	// UsageSourceUnknown 表示本次调用没有取得用量。零值即此值。
	//
	// 该状态下各个计数字段同样是 0，但那只是缺省填充，不代表真实用量为零。
	// 调用方必须走兜底分支，不得按零扣除。
	UsageSourceUnknown UsageSource = ""
	// UsageSourceUpstream 表示用量来自上游返回的计数。
	UsageSourceUpstream UsageSource = "upstream"
	// UsageSourceEstimated 表示上游未返回用量，由网关按已解码的请求与响应估算。
	UsageSourceEstimated UsageSource = "estimated"
)

// known 报告来源是否为已定义的可结算取值。未识别的取值一律按未取得用量处理。
//
// 它只判断来源本身；实际能否结算还要看 Known，后者会剔除「来源为上游但计数全零」
// 这种没有真实计数的形态。
func (s UsageSource) known() bool {
	return s == UsageSourceUpstream || s == UsageSourceEstimated
}

// combineUsageSource 返回两个来源合并后的来源，取其中较不可信的一个：
// 任一方未知则结果未知；否则任一方为估算则结果为估算。
func combineUsageSource(a, b UsageSource) UsageSource {
	switch {
	case !a.known() || !b.known():
		return UsageSourceUnknown
	case a == UsageSourceEstimated || b == UsageSourceEstimated:
		return UsageSourceEstimated
	default:
		return UsageSourceUpstream
	}
}

// Usage 是一次上游调用的 token 用量。
//
// 计数口径统一为「子项 ⊆ 主计数」：
//
//   - InputTokens：输入 token 总数，包含 CacheReadTokens 与 CacheWriteTokens；
//   - OutputTokens：输出 token 总数，包含 ReasoningTokens；
//   - CacheReadTokens：命中缓存的输入 token 子项；
//   - CacheWriteTokens：写入缓存的输入 token 子项；
//   - ReasoningTokens：用于推理的输出 token 子项；
//   - ServerToolUses：服务端工具（如 web_search）的执行次数（按次计费，非 token）。
//
// 该口径以 OpenAI 侧线格式为准；Anthropic Messages 的线格式（input_tokens 不含缓存 token）
// 由其适配器在解码边界换算对齐。
//
// 本类型不做任何估算：估算由调用方完成后把 Source 置为 UsageSourceEstimated 再交付。
type Usage struct {
	// Source 标识用量来源；零值表示未取得用量。见 UsageSource。
	Source UsageSource `json:"source,omitempty"`

	// InputTokens 是本次调用的输入 token 总数，含缓存读与缓存写。
	InputTokens int `json:"input_tokens"`
	// OutputTokens 是本次调用的输出 token 总数，含推理。
	OutputTokens int `json:"output_tokens"`
	// CacheReadTokens 是 InputTokens 中命中缓存的子项。
	CacheReadTokens int `json:"cache_read_tokens"`
	// CacheWriteTokens 是 InputTokens 中写入缓存的子项。
	CacheWriteTokens int `json:"cache_write_tokens"`
	// ReasoningTokens 是 OutputTokens 中用于推理的子项。
	ReasoningTokens int `json:"reasoning_tokens"`
	// ServerToolUses 是本次调用中上游服务端工具（如 web_search）的执行次数。
	// 它不是 token 计数，按渠道配置的单次单价另行计费；0 表示未执行或不计。
	ServerToolUses int `json:"server_tool_uses"`
}

// Known 报告用量是否可用于结算。
//
// 返回 false 表示上游未返回可用计数，此时各计数字段是缺省填充的 0，不是真实用量。
// 调用方必须据此走兜底分支，绝不能因为 IsZero 为真就跳过结算。
//
// 注意：来源标为上游但各类计数全为 0 时同样返回 false。一个真实转发的请求必须带
// 消息，因此不可能消耗 0 个输入 token；上游给出全零计数代表它没有真的给出计数
// （例如只发了一个空的用量对象），不能当作用量确实为零。
func (u Usage) Known() bool {
	switch u.Source {
	case UsageSourceEstimated:
		return true
	case UsageSourceUpstream:
		return !u.IsZero()
	default:
		return false
	}
}

// Total 返回本次调用的 token 总数：输入与输出之和，与 OpenAI 的 total_tokens 同义。
//
// 缓存读、缓存写与推理是主计数的子项（见 Usage 的字段说明），不参与求和，否则会对
// OpenAI 口径的用量重复计数。它**不是可结算金额**：
// 结算按分类单价加权求和，必须读取五类计数字段本身。
func (u Usage) Total() int {
	return u.InputTokens + u.OutputTokens
}

// Add 返回两个用量之和。用于把一次请求的多次上游尝试用量合并。
//
// 合并后的来源取两者中较不可信的一个：任一方未知则结果未知。这样合并不会把
// 不完整的用量伪装成完整的。
func (u Usage) Add(other Usage) Usage {
	return Usage{
		Source:           combineUsageSource(u.Source, other.Source),
		InputTokens:      u.InputTokens + other.InputTokens,
		OutputTokens:     u.OutputTokens + other.OutputTokens,
		CacheReadTokens:  u.CacheReadTokens + other.CacheReadTokens,
		CacheWriteTokens: u.CacheWriteTokens + other.CacheWriteTokens,
		ReasoningTokens:  u.ReasoningTokens + other.ReasoningTokens,
		ServerToolUses:   u.ServerToolUses + other.ServerToolUses,
	}
}

// BoundSubitemsToMain 返回一份把子项钳回主计数之内的用量，守护「子项 ⊆ 主计数」不变量。
//
// 部分兼容上游偶发把子项统计得比主计数还大（例如 reasoning_tokens 大于 completion_tokens），
// 原样落库会违反数据库「子项不得大于主计数」的检查约束，使一次已经发生的调用结算失败。
// 这里只抬高主计数、不削减子项，既不丢弃上游给出的子项，也不让用量凭空变小：
//
//   - OutputTokens 抬到不小于 ReasoningTokens；
//   - InputTokens 抬到不小于 CacheReadTokens 与 CacheWriteTokens 之和。
//
// 方法为值接收者，返回新值、不改动接收者，且幂等：对已满足不变量的用量重复调用结果不变。
func (u Usage) BoundSubitemsToMain() Usage {
	if u.ReasoningTokens > u.OutputTokens {
		u.OutputTokens = u.ReasoningTokens
	}
	if cacheTotal := u.CacheReadTokens + u.CacheWriteTokens; cacheTotal > u.InputTokens {
		u.InputTokens = cacheTotal
	}
	return u
}

// IsZero 报告各类计数是否均为 0。
//
// 它只做算术判断，不看 Source：未取得用量时各字段同样是 0。因此「是否结算」不能
// 用本方法决定，必须先看 Known()。
func (u Usage) IsZero() bool {
	return u.InputTokens == 0 &&
		u.OutputTokens == 0 &&
		u.CacheReadTokens == 0 &&
		u.CacheWriteTokens == 0 &&
		u.ReasoningTokens == 0
}

// NonNegative 报告是否所有类别均为非负数。上游返回负数属于异常，调用方应据此拒绝。
func (u Usage) NonNegative() bool {
	return u.InputTokens >= 0 &&
		u.OutputTokens >= 0 &&
		u.CacheReadTokens >= 0 &&
		u.CacheWriteTokens >= 0 &&
		u.ReasoningTokens >= 0
}
