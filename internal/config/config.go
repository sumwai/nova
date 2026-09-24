// Package config 解析 Novafile。
//
// 语法是「全局指令直接写在顶层、一行一条」：没有全局花括号块，因此不存在
// 「指令写在块外还是块内」这一类需要解释的问题。
//
// 解析分三步，顺序是有意的：
//   - 词法：把源码切成记号，只关心引号、注释与花括号；
//   - 代数判定：先找出 version 声明并校验它是否落在本二进制支持的区间内；
//   - 语法：逐行把指令落到 Config 上。
//
// 代数判定排在语法之前，是为了让「你的 nova 太旧」与「你把指令拼错了」分开报：
// 若先解析指令，一份按更高代数书写的配置会先撞上「未知指令」，而使用者真正
// 需要知道的是版本对不上，不是某个词不认识。
package config

import (
	"fmt"
	"os"
	"sort"
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

// 各条指令的缺省取值。放在这里而不是散在解析代码里，是为了让「缺省是什么」
// 有一个可被引用的单一出处（帮助文本与版本输出都要说这件事）。
const (
	defaultLogLevel = "info"
	defaultListen   = ":8080"
	defaultAdmin    = "localhost:2026"
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

	// Listen 是数据面监听地址，缺省 :8080。
	Listen string

	// Admin 是管理端点地址，缺省 localhost:2026。
	Admin string

	// Warnings 是解析期攒下的、不阻止加载的提醒。
	// 收集起来统一输出，让一次加载里的全部问题一次说完，而不是报一条改一条。
	Warnings []Warning
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

// Load 读取并解析一份 Novafile。
//
// 读取失败也返回 *Error（File 为路径、Line 为 0）：调用方因此只需处理一种错误类型，
// 不必把「文件不存在」与「文件写错」分成两条路径，退出码与输出格式也就自然一致。
func Load(path string) (*Config, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, &Error{File: path, Msg: fmt.Sprintf("读取配置文件失败：%v", err)}
	}
	return Parse(src, path)
}

// Parse 解析配置源码。filename 只用于报错定位，不参与解析，也不要求文件真实存在。
func Parse(src []byte, filename string) (*Config, error) {
	tokens, err := lex(src, filename)
	if err != nil {
		return nil, err
	}
	cfg := &Config{
		Path:     filename,
		Schema:   CurrentSchema,
		LogLevel: defaultLogLevel,
		Listen:   defaultListen,
		Admin:    defaultAdmin,
	}
	p := &parser{file: filename, lines: groupLines(tokens), cfg: cfg, seen: map[string]token{}}
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
// 清单从解析器真正使用的那张指令表派生，而不是另写一份字面量副本：另抄一份
// 必然与解析能力漂移，而漂移后的清单会变成一句更危险的错话——它声称支持某个
// 已被删掉的写法。每次返回新切片，调用方改它不影响解析行为，也不污染下一次查询。
func InstructionNames() []string {
	names := make([]string, len(globalDirectives))
	copy(names, globalDirectives)
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
