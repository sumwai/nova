// Package profile 加载并校验平台档案。
//
// 档案描述平台事实：端点、协议、认证头、模型 id、计划形态、用量探测与错误映射。
// 本包只做两件事——用嵌入的 JSON Schema 校验形状，再做 schema 表达不了的跨字段
// 语义校验（引用存在、正则可编译）——不实现价格、额度求值、订阅源拉取与选路。
//
// 校验与取值共用同一份解析结果：档案禁用锚点、别名、多文档与重复键，解析出的节点树
// 既用于报错定位，也用于解码成 Profile，禁止两处各解析一次。
package profile

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

// CurrentSchema 是本二进制实现的最高档案格式代数。
//
// 代数只增不减。新增一个代数意味着发生了一次不兼容的字段变更：旧写法要么仍被接受
// （那个代数留在 MinSchema..CurrentSchema 区间里），要么只能靠报错把人推向新写法。
// 同一代数内的兼容性新增不改变代数。
const CurrentSchema = 1

// MinSchema 是本二进制仍能接受的档案格式代数下界。
const MinSchema = 1

// Profile 是一份通过校验的平台档案。
//
// 字段与 schema/profile-1.yaml 一一对应；后续按档案工作的层从这里取值，
// 不各自再解析一次 YAML。
type Profile struct {
	Schema        int                 `yaml:"schema"`
	ID            string              `yaml:"id"`
	Name          string              `yaml:"name"`
	UpdatedAt     string              `yaml:"updated_at"`
	Docs          []string            `yaml:"docs"`
	APIPattern    string              `yaml:"api_pattern"`
	Auth          Auth                `yaml:"auth"`
	Headers       map[string]string   `yaml:"headers"`
	Endpoints     map[string]Endpoint `yaml:"endpoints"`
	Models        []Model             `yaml:"models"`
	Discover      *Discover           `yaml:"discover"`
	Usage         *Usage              `yaml:"usage"`
	Plans         []Plan              `yaml:"plans"`
	LimitsMapping []LimitsMapping     `yaml:"limits_mapping"`
}

// Auth 是凭据注入方式：请求头名、方案前缀与默认取值的环境变量名。
type Auth struct {
	Header string `yaml:"header"`
	Scheme string `yaml:"scheme"`
	Env    string `yaml:"env"`
}

// Endpoint 是一条上游端点：地址与协议。
type Endpoint struct {
	URL      string `yaml:"url"`
	Protocol string `yaml:"protocol"`
}

// Model 是一条模型声明。
type Model struct {
	ID        string   `yaml:"id"`
	Public    string   `yaml:"public"`
	Endpoints []string `yaml:"endpoints"`
	Free      bool     `yaml:"free"`
	Price     *Price   `yaml:"price"`
	PriceFrom string   `yaml:"price_from"`
}

// Price 是一条按量单价：币种加每百万 token 的价格。
//
// free 与 price 是两条互斥的事实：免费必须显式写 free，一条全零的 price 会被当成
// 「查不到价格」，而不是「免费」。price_from 指向价格表里另一个键，与 price 同时
// 出现时以 price 为准。
type Price struct {
	Currency       string  `yaml:"currency"`
	InputMTok      float64 `yaml:"input_mtok"`
	OutputMTok     float64 `yaml:"output_mtok"`
	CacheReadMTok  float64 `yaml:"cache_read_mtok"`
	CacheWriteMTok float64 `yaml:"cache_write_mtok"`
	ReasoningMTok  float64 `yaml:"reasoning_mtok"`
}

// Discover 是清单发现配置：地址、形状与准入模式。
type Discover struct {
	URL   string   `yaml:"url"`
	Shape string   `yaml:"shape"`
	Allow []string `yaml:"allow"`
	Deny  []string `yaml:"deny"`
}

// Usage 是用量探测配置：内置 probe 或外部 exec 二选一，加间隔与解析 schema。
type Usage struct {
	Probe    string `yaml:"probe"`
	Exec     string `yaml:"exec"`
	Interval string `yaml:"interval"`
	Schema   string `yaml:"schema"`
}

// Plan 是一条计划形态：付费信息与静态限制。
type Plan struct {
	ID   string `yaml:"id"`
	Paid *Paid  `yaml:"paid"`
	// ExpiresAt 是计划到期时刻（RFC3339，带时区）；空表示不过期。
	//
	// 它是额度文档的 expires_at 在静态声明侧的来源：计划到期后该账号的额度条目
	// 一律不可用，直到换一份带新到期时刻的档案。订阅制平台的周期结束、试用期结束
	// 都属于这一类，不需要等探测落地。
	ExpiresAt string  `yaml:"expires_at"`
	Limits    []Limit `yaml:"limits"`
}

// Paid 只作展示与顺序表达，不参与跨账号自动比价。
type Paid struct {
	Amount   float64 `yaml:"amount"`
	Currency string  `yaml:"currency"`
	Period   string  `yaml:"period"`
}

// Limit 是一条额度条目。
//
// limit / used / remaining 用指针接收，显式区分「缺失」与「零值」：Go 的数值零值就是 0，
// 把 remaining: 0 当成「字段不存在」会让已耗尽账号被当作可用。
type Limit struct {
	Kind      string   `yaml:"kind"`
	Metric    string   `yaml:"metric"`
	Window    string   `yaml:"window"`
	TZ        string   `yaml:"tz"`
	Anchor    string   `yaml:"anchor"`
	Limit     *float64 `yaml:"limit"`
	Used      *float64 `yaml:"used"`
	Remaining *float64 `yaml:"remaining"`
	ResetsAt  string   `yaml:"resets_at"`
	Unbounded bool     `yaml:"unbounded"`
	Assumed   bool     `yaml:"assumed"`
	Pool      string   `yaml:"pool"`
}

// LimitsMapping 是状态码与响应体关键词到错误分类的映射。
type LimitsMapping struct {
	Status    int    `yaml:"status"`
	MatchBody string `yaml:"match_body"`
	Class     string `yaml:"class"`
}

// Load 读取并校验一份档案。
//
// 顺序是有意的：先判代数，再做 schema 校验，最后做跨字段语义校验。代数判定排在形状
// 校验之前，是为了让「档案按更高代数书写」与「字段拼错了」分开报——若先做 schema
// 校验，一份按更高代数书写的档案会先撞上「未知字段」，而使用者真正需要知道的是
// 该升级 nova，不是某个词不认识。
func Load(path string) (*Profile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &Error{File: path, Msg: fmt.Sprintf("读取档案失败：%v", err)}
	}
	return loadProfile(path, data)
}

// loadProfile 校验并解码一份已经在内存里的档案。
//
// 文件读取与内容校验分开：内置档案来自嵌入资源、没有磁盘路径，但必须与使用者提供的
// 档案走同一套约束（禁锚点别名、schema 校验、跨字段语义校验），因此 name 只用于报错
// 定位，加载逻辑只认字节。
func loadProfile(name string, data []byte) (*Profile, error) {
	root, parseErr := parseDocument(name, data)
	if parseErr != nil {
		return nil, parseErr
	}
	if err := checkGeneration(name, root); err != nil {
		return nil, err
	}
	if err := validateProfileSchema(name, root); err != nil {
		return nil, err
	}

	var loaded Profile
	if err := root.Decode(&loaded); err != nil {
		return nil, &Error{File: name, Msg: fmt.Sprintf("档案解码失败：%v", err)}
	}
	if err := loaded.checkSemantics(name, root); err != nil {
		return nil, err
	}
	return &loaded, nil
}

// checkGeneration 校验档案声明的格式代数落在本二进制支持区间内。
func checkGeneration(path string, root *yaml.Node) *Error {
	node := mappingValue(root, "schema")
	// 取值不是整数时不在这里报错：类型问题留给 schema 校验，报错文案更贴切。
	if node == nil || node.Tag != "!!int" {
		return nil
	}
	got, err := strconv.Atoi(node.Value)
	if err != nil {
		return nil
	}
	if got < MinSchema || got > CurrentSchema {
		return errorAt(path, node, "档案格式代数 %d 不被本二进制支持（本二进制支持 %s）%s",
			got, generationRange(), generationAdvice(got))
	}
	return nil
}

// generationRange 把支持区间排成人读文本；区间退化成单点时不写成「1 到 1」。
func generationRange() string {
	if MinSchema == CurrentSchema {
		return strconv.Itoa(CurrentSchema)
	}
	return fmt.Sprintf("%d 到 %d", MinSchema, CurrentSchema)
}

// generationAdvice 给出「该怎么办」的那半句，按代数偏向哪一侧分开写。
//
// 两侧的处置完全不同：代数偏高说明档案太新、nova 太旧，需要升级程序；代数偏低说明
// 档案太旧，需要改档案。写成同一句「版本不匹配」等于把判断该动哪一边的活推回给使用者。
func generationAdvice(got int) string {
	if got > CurrentSchema {
		return "；档案太新、nova 太旧，这份档案按更高的代数书写，请升级 nova，或把档案改回本二进制支持的代数"
	}
	return "；档案太旧，本二进制已不再接受这个旧代数，请把档案改到本二进制支持的代数"
}

// checkSemantics 做 schema 表达不了的跨字段校验。
func (p *Profile) checkSemantics(path string, root *yaml.Node) *Error {
	if p.APIPattern != "" {
		if _, err := regexp.Compile(p.APIPattern); err != nil {
			return pathAtNode(path, root, []string{"api_pattern"}, "api_pattern 不是合法正则：%v", err)
		}
	}
	for i, model := range p.Models {
		base := []string{"models", strconv.Itoa(i)}
		for j, name := range model.Endpoints {
			if _, ok := p.Endpoints[name]; !ok {
				where := append(append([]string{}, base...), "endpoints", strconv.Itoa(j))
				return pathAtNode(path, root, where, "模型 %q 引用了不存在的端点 %q", model.ID, name)
			}
		}
	}
	for i, plan := range p.Plans {
		if plan.ExpiresAt == "" {
			continue
		}
		if _, err := time.Parse(time.RFC3339, plan.ExpiresAt); err != nil {
			return pathAtNode(path, root, []string{"plans", strconv.Itoa(i), "expires_at"},
				"计划 %s 的 expires_at 不是合法的 RFC3339 时刻：%v", plan.ID, err)
		}
	}
	for i, mapping := range p.LimitsMapping {
		if mapping.MatchBody == "" {
			continue
		}
		if _, err := regexp.Compile(mapping.MatchBody); err != nil {
			return pathAtNode(path, root, []string{"limits_mapping", strconv.Itoa(i), "match_body"},
				"limits_mapping 的 match_body 不是合法正则：%v", err)
		}
	}
	return nil
}

// hasModel 报告档案内是否存在某个模型条目；id 与 public 都算命中。
//
// TODO(价格层)：当前不对 price_from 做存在性校验。设计文档 §3.3 的 price_from 可以是
// `namespace/model` 形式的外部价格表引用（如 deepseek/deepseek-v4-flash），与档案内
// 模型 id 不是同一命名空间，用档案自身就判定存在性会拒掉合法档案。价格层落地后，
// 命中范围应并上内置价格表，再由那一层判定引用是否存在。
func (p *Profile) hasModel(name string) bool {
	for _, model := range p.Models {
		if model.ID == name || (model.Public != "" && model.Public == name) {
			return true
		}
	}
	return false
}
