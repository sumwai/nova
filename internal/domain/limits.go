package domain

// LimitsClass 是档案对「某个状态码 + 响应体形态」给出的错误分类。
//
// 分类决定两件事：账号状态是否被打上「本窗口耗尽」的标记，以及这次失败能否换渠道重试。
// 取值与档案 schema 的 limits_mapping.class 一一对应。
type LimitsClass string

const (
	// LimitsTransientRate 表示秒级限流：本请求退避，不写账号状态。
	LimitsTransientRate LimitsClass = "transient-rate"
	// LimitsWindowExhausted 表示本窗口额度耗尽：写「至 until 不可用」标记并可重试。
	LimitsWindowExhausted LimitsClass = "window-exhausted"
	// LimitsPermanent 表示模型无权、组织未实名、地区受限等：不重试、不打标记、原样回传。
	LimitsPermanent LimitsClass = "permanent"
)

// Valid 报告取值是否为本包认识的分类。
func (c LimitsClass) Valid() bool {
	switch c {
	case LimitsTransientRate, LimitsWindowExhausted, LimitsPermanent:
		return true
	default:
		return false
	}
}

// LimitsRule 是一条档案声明的错误分类规则。
//
// 命中条件是「状态码相等，且 MatchBody 为空或响应体命中它」。MatchBody 是正则文本，
// 由档案加载期校验可编译；多条规则按声明顺序取第一条命中的。
//
// 规则是平台事实，由档案承载，经装配层写进 Route；本包只表达形状，不做匹配。
type LimitsRule struct {
	Status    int
	MatchBody string
	Class     LimitsClass
}
