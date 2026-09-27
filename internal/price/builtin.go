package price

// builtin 是随二进制发布的厂商基础价表。
//
// 数据来源：genai-prices（MIT）的 `prices/new_data/v2/data.json`，抓取日期 2026-09-27；
// 键取「厂商 id/模型 id」，与 `price_from` 的命名空间一致，档案因此可以跨档案引用它。
//
// 两处有意简化，均为「取一个可核对的代表值」而不是推导：
//   - 同一模型有多档时段/日期价（如 DeepSeek 的错峰折扣）时取不带约束的基础档；
//   - 分级计价（如 MiniMax 按上下文长度分档）时取首档。
//
// 它只服务 `prefer price` 的排序与统计口径，不参与任何计费；真实单价以平台账单为准。
var builtin = map[string]Unit{
	"deepseek/deepseek-v4-flash": {
		Currency:      "USD",
		InputMTok:     0.14,
		OutputMTok:    0.28,
		CacheReadMTok: 0.0028,
	},
	"deepseek/deepseek-v4-pro": {
		Currency:      "USD",
		InputMTok:     0.435,
		OutputMTok:    0.87,
		CacheReadMTok: 0.003625,
	},
	"moonshotai/kimi-k3": {
		Currency:      "USD",
		InputMTok:     3,
		OutputMTok:    15,
		CacheReadMTok: 0.3,
	},
	"zai/GLM-5.2": {
		Currency:      "USD",
		InputMTok:     1.4,
		OutputMTok:    4.4,
		CacheReadMTok: 0.26,
	},
	"zai/GLM-5.3": {
		Currency:      "USD",
		InputMTok:     1.4,
		OutputMTok:    4.4,
		CacheReadMTok: 0.26,
	},
	"minimax/minimax-m3": {
		Currency:      "USD",
		InputMTok:     0.3,
		OutputMTok:    1.2,
		CacheReadMTok: 0.06,
	},
}
