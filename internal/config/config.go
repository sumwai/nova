// Package config 解析 Novafile。
//
// 语法沿用 Caddyfile 的取舍：**指令直接写在顶层、一行一条；只有要把一个地址和它
// 提供的模型绑在一起时，才用花括号。** 块的边界由花括号回答，不靠缩进——缩进对
// 配置文件来说太脆，一次误按 Tab 就换了个含义。
//
// 解析分四步，顺序是有意的：
//   - 词法：把源码切成记号，只关心引号、注释、花括号与占位符；
//   - import 展开：把 import 行原地换成被导入文件的内容；
//   - 代数判定：先找出 version 声明并校验它落在本二进制支持的区间内；
//   - 语法：逐行把指令落到 Config 上。
//
// 代数判定排在语法之前，是为了让「nova 太旧」与「指令拼错了」分开报：
// 若先解析指令，一份按更高代数书写的配置会先撞上「未知指令 base_url」，
// 而使用者真正需要知道的是版本对不上，不是某个词不认识。
package config

import (
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/sumwai/nova/internal/domain"
)

// CurrentSchema 是本二进制实现的最高配置语法代数。
//
// 代数只增不减。新增一个代数意味着发生了一次不兼容的语法变更：旧的写法要么
// 仍然被接受（那个代数留在 MinSchema..CurrentSchema 区间里），要么只能靠报错
// 把人推向新写法。在同一个代数内做兼容性新增（多认一条指令）不改变代数。
const CurrentSchema = 1

// MinSchema 是本二进制仍能接受的配置语法代数下界。
//
// 它与 CurrentSchema 一起构成「支持区间」，而不是一个单点：二进制保留对旧代数的
// 支持，旧配置才不会因为升级 nova 而集体失效——那正是配置代数要解决的问题本身。
const MinSchema = 1

// 各条指令的缺省取值。
//
// 放在这里而不是散在解析代码里，是为了让「缺省是什么」有一个可被引用的单一出处：
// 帮助文本、启动横幅与版本输出都要说这件事。
const (
	defaultLogLevel = "info"
	// 缺省输出人读的文本：nova 的多数实例跑在终端或 systemd 下，两者都要人眼能读；
	// 需要给日志采集器喂结构化数据时显式写 log_format json。
	defaultLogFormat = "text"
	// 缺省只绑回环：本版的数据面没有别的兜底鉴别手段，绑到所有接口就等于
	// 把上游凭据敞给任何能连上这台机器的人。对外服务时显式写 listen 即可。
	defaultListen  = "127.0.0.1:8080"
	defaultAdmin   = "localhost:2026"
	defaultTimeout = 60 * time.Second
)

// Config 是一份 Novafile 的解析结果。
//
// 它只描述「配置说了什么」，不含任何运行时对象：把解析与装配分开，
// 才能让 `nova config check` 在不监听端口、不连接上游的前提下完整校验一份配置。
type Config struct {
	// Path 是这份配置的来源文件路径，用于报错与日志。
	Path string

	// Schema 是这份配置的语法代数：文件声明了就用声明的，没声明就是 CurrentSchema。
	Schema int

	// SchemaDeclared 表示文件里是否显式写了 version。
	// 它与 Schema 是两件事：「没写」与「写了当前代数」解析结果相同，但前者值得提醒一句。
	SchemaDeclared bool

	// LogLevel 是 debug / info / warn / error，缺省 info。
	LogLevel string

	// LogFormat 是 text / json，缺省 text。
	//
	// 它与 LogLevel 是两件事：级别决定「哪些记录被输出」，格式决定「输出的记录长什么样」。
	// 两者都做成配置指令而不是环境变量，是因为它们同属「输出怎么呈现」这一类意图；
	// 而「终端能不能显示颜色」是环境事实而不是意图，因此不进配置（见 gateway 的渲染层）。
	LogFormat string

	// Listen 是数据面监听地址，缺省 127.0.0.1:8080（只绑回环）。
	Listen string

	// Admin 是管理端点地址，缺省 localhost:2026。
	Admin string

	// ClientKeys 是允许调用网关的凭据，按声明顺序；为空表示不鉴权。
	ClientKeys []string

	// Providers 是上游渠道，按声明顺序。这个顺序就是同名模型之间的回退顺序。
	Providers []Provider

	// Warnings 是解析期攒下的、不阻止加载的提醒。
	// 收集起来统一输出，让一次加载里的全部问题一次说完，而不是报一条改一条。
	Warnings []Warning
}

// Provider 是一条上游渠道。
//
// 名字只用于日志与错误定位，不参与选路——选路的唯一依据是对外模型名。
// 凭据是本块共用的：同一个渠道下的每条端点都走同一份密钥。
type Provider struct {
	Name string

	// APIKey 是这条渠道的凭据，由 api_key 指令给出。
	APIKey string

	// Endpoints 是这条渠道的端点，按声明位置排序。
	// 顺序有意义：同一个对外模型名落在多条端点上时，它就是回退顺序。
	Endpoints []Endpoint

	// File / Line / Col 指向 provider 名字的位置，用于报错时指回块头。
	File string
	Line int
	Col  int
}

// Endpoint 是一条上游端点：一个地址、一种线协议、一组对外模型名。
//
// 「哪些模型属于哪条端点」必须由结构表达出来。把 endpoint 与 model 平铺在同一层
// 会丢掉这层归属，于是「两条端点各自提供什么」无从判断，请求可能被发到一条
// 用错线格式的端点上，拿到一个语法诡异的错误而配置看起来毫无问题。
type Endpoint struct {
	// URL 是完整地址，含协议段与端点路径。
	URL string

	// Protocol 是这条端点使用的线协议。
	Protocol domain.Protocol

	// Timeout 是单次上游调用的超时。
	Timeout time.Duration

	// Models 是这条端点提供的模型映射，按声明顺序。
	Models []Model

	// Default 表示这是写在 provider 一级的那条默认端点，而不是 endpoint 子块。
	// 两者在转发行为上没有区别，这个标志只用于报错与日志里把它叫对名字。
	Default bool

	// File / Line / Col 指向这条端点的声明处：默认端点指它第一条端点指令，
	// endpoint 子块指块头那一行。
	File string
	Line int
	Col  int
}

// Model 是一条模型映射：客户端请求里的对外名，与发往上游时使用的名字。
type Model struct {
	// Name 是对外名，客户端请求里的 model 要匹配它。
	Name string

	// Upstream 是发往上游时写进请求体的模型名。未显式给出时等于 Name。
	Upstream string
}

// Route 是一个对外模型名落到某条端点上的结果。
type Route struct {
	// Provider 是端点所属渠道的名字，仅用于日志与错误定位。
	Provider string

	// Endpoint 是命中的端点。它是配置内部对象的指针，调用方只读。
	Endpoint *Endpoint

	// Upstream 是发往上游时使用的模型名。
	Upstream string
}

// Routes 返回一个对外模型名下的全部候选，按声明顺序。
//
// 顺序就是回退顺序：调用方依次尝试，前一条失败且可重试时才换下一条。
// 返回新切片，调用方改它不影响配置本身。
func (c *Config) Routes(name string) []Route {
	var routes []Route
	for i := range c.Providers {
		provider := &c.Providers[i]
		for j := range provider.Endpoints {
			endpoint := &provider.Endpoints[j]
			for _, model := range endpoint.Models {
				if model.Name == name {
					routes = append(routes, Route{
						Provider: provider.Name,
						Endpoint: endpoint,
						Upstream: model.Upstream,
					})
				}
			}
		}
	}
	return routes
}

// ModelNames 返回全部对外模型名，按首次声明的顺序去重。
//
// 顺序取首次声明而不是字典序：这个列表会被用于展示与诊断，让它的顺序与配置里
// 读到的顺序一致，使用者对着配置排查时不必来回换算位置。
func (c *Config) ModelNames() []string {
	var names []string
	seen := map[string]bool{}
	for i := range c.Providers {
		for j := range c.Providers[i].Endpoints {
			for _, model := range c.Providers[i].Endpoints[j].Models {
				if seen[model.Name] {
					continue
				}
				seen[model.Name] = true
				names = append(names, model.Name)
			}
		}
	}
	return names
}

// Warning 是一条带位置的提醒。
//
// Line 为 0 表示这条提醒不指向某一行（例如「缺省值来自内置」）。
type Warning struct {
	File string
	Line int
	Msg  string
}

// String 排版成与 *Error 一致的位置前缀，便于日志里两类消息可以一起 grep。
func (w Warning) String() string {
	if w.Line == 0 {
		return fmt.Sprintf("%s: %s", w.File, w.Msg)
	}
	return fmt.Sprintf("%s:%d: %s", w.File, w.Line, w.Msg)
}

// Options 是解析期的外部依赖。
//
// Getenv 作为入参注入而不是就地调用 os.Getenv，是为了让「占位符展开」这类依赖
// 外部环境的行为能在测试里被完全控制，不必去改测试进程的环境变量——
// 那样不仅会让测试之间互相干扰，还测不到「变量未设置」这条分支。
type Options struct {
	// Getenv 读环境变量，用于展开 {env.NAME}。为零值时用 os.Getenv。
	Getenv func(string) string
}

// Load 读取并解析一份 Novafile。
//
// 读取失败也返回 *Error（File 为路径、Line 为 0）：调用方因此只需处理一种错误类型，
// 不必把「文件不存在」与「文件写错」分成两条路径，退出码与输出格式也就自然一致。
func Load(path string) (*Config, error) {
	return LoadWith(path, Options{})
}

// LoadWith 在 Load 的基础上接受解析选项。
func LoadWith(path string, opts Options) (*Config, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, &Error{File: path, Msg: fmt.Sprintf("读取配置文件失败：%v", err)}
	}
	return ParseWith(src, path, opts)
}

// Parse 解析配置源码。filename 只用于报错定位，不参与解析，也不要求文件真实存在。
func Parse(src []byte, filename string) (*Config, error) {
	return ParseWith(src, filename, Options{})
}

// ParseWith 在 Parse 的基础上接受解析选项。
func ParseWith(src []byte, filename string, opts Options) (*Config, error) {
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}

	tokens, err := lex(src, filename)
	if err != nil {
		return nil, err
	}
	cfg := &Config{
		Path:      filename,
		Schema:    CurrentSchema,
		LogLevel:  defaultLogLevel,
		LogFormat: defaultLogFormat,
		Listen:    defaultListen,
		Admin:     defaultAdmin,
	}

	// import 展开排在语法解析之前：它产出的行要参与同一次语法分析，
	// 因此必须在建 parser 之前把行集定下来。
	im := &importer{}
	lines, err := im.expand(groupLines(tokens))
	if err != nil {
		return nil, err
	}
	cfg.Warnings = append(cfg.Warnings, im.warnings...)

	p := &parser{lines: lines, cfg: cfg, seen: map[string]token{}, getenv: getenv}
	if err := p.run(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// InstructionNames 返回本二进制接受的配置指令名，已去重并按字典序升序排列。
//
// 用途是让「这个二进制到底认哪些写法」成为可由二进制自己回答的事实：
// 排障时不必翻源码，也不必对着一串版本号猜，直接问二进制。
//
// 清单从解析器真正使用的那几张指令表派生，而不是另写一份字面量副本：另抄一份
// 必然与解析能力漂移，而漂移后的清单会变成一句更危险的错话——它声称支持某个
// 已被删掉的写法。每次返回新切片，调用方改它不影响解析行为，也不污染下一次查询。
func InstructionNames() []string {
	var names []string
	seen := map[string]bool{}
	for _, table := range [][]string{globalDirectives, providerDirectives, endpointDirectives, blockDirectives} {
		for _, name := range table {
			// 同一个名字可能出现在多个层级（timeout、model 等既能在 provider 一级
			// 也能落在 endpoint 块里），对外声明的是名字集合，因此按名字去重。
			if seen[name] {
				continue
			}
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// SupportedSchemas 返回本二进制支持的配置语法代数，升序。
//
// 与 InstructionNames 一样每次返回新切片：调用方拿到的是一份快照，不是内部状态。
func SupportedSchemas() []int {
	schemas := make([]int, 0, CurrentSchema-MinSchema+1)
	for s := MinSchema; s <= CurrentSchema; s++ {
		schemas = append(schemas, s)
	}
	return schemas
}
