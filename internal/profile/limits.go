package profile

import (
	"fmt"
	"sort"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

// LimitsDoc 是一份额度文档：由探测输出、响应头或档案静态声明提供。
//
// 作用域在结构层表达：Account 为跨模型共享，Models 只影响所列模型。条目类型复用
// Limit，因此 LoadLimits 与档案加载共用同一套解析约束（禁锚点、别名、多文档与重复键）
// 与同一份嵌入的 limits-1.yaml，取值与校验不会各解析一次。
//
// limit / used / remaining 在 Limit 里是指针，显式区分「缺失」与「零值」：
// remaining: 0 表示已耗尽，缺失表示没有这项数据，两者不得混为一谈。
type LimitsDoc struct {
	Schema     int                `yaml:"schema"`
	ObservedAt string             `yaml:"observed_at"`
	Source     string             `yaml:"source"`
	ExpiresAt  string             `yaml:"expires_at"`
	Account    []Limit            `yaml:"account"`
	Models     map[string][]Limit `yaml:"models"`
}

// LoadLimits 解析并校验一份额度文档。
//
// 走与档案加载同一套机制：parseDocument 施加禁锚点、别名、多文档与重复键的约束，
// 再用嵌入的 limits schema 校验形状，最后做 schema 表达不了的语义校验。报错带文件
// 路径与行号，可直接当跳转目标；name 只用于报错定位，加载逻辑只认字节，探测脚本的
// 输出因此不必先落盘。
func LoadLimits(name string, data []byte) (*LimitsDoc, error) {
	root, parseErr := parseDocument(name, data)
	if parseErr != nil {
		return nil, parseErr
	}
	if err := validateLimitsSchema(name, root); err != nil {
		return nil, err
	}

	var loaded LimitsDoc
	if err := root.Decode(&loaded); err != nil {
		return nil, &Error{File: name, Msg: fmt.Sprintf("额度文档解码失败：%v", err)}
	}
	if err := loaded.checkSemantics(name, root); err != nil {
		return nil, err
	}
	return &loaded, nil
}

// ParseLimitsYAML 是取字节、不关心来源名的额度文档入口，供探测脚本的输出直接使用。
//
// 与 LoadLimits 是同一套校验，只把报错定位用的名字固定为 "<limits>"：探测脚本没有
// 文件路径可报，硬编一个占位名比让调用方先造一个假路径诚实。
func ParseLimitsYAML(data []byte) (*LimitsDoc, error) {
	return LoadLimits("<limits>", data)
}

// checkSemantics 做 schema 表达不了的跨字段校验。
//
// 覆盖三点：时刻字段必须是带时区的绝对时刻；绝对窗口不得配 resets_at（给它安排一个
// 未来时刻自动复活是错的）；自然周期必须带 tz。前两点 schema 无法表达，第三点在
// schema 里有同名约束，这里再判一次是为了把报错指向具体条目并说清原因。
func (d *LimitsDoc) checkSemantics(file string, root *yaml.Node) *Error {
	for _, field := range []struct {
		name  string
		value string
	}{
		{"observed_at", d.ObservedAt},
		{"expires_at", d.ExpiresAt},
	} {
		if field.value == "" {
			continue
		}
		if _, err := time.Parse(time.RFC3339, field.value); err != nil {
			return pathAtNode(file, root, []string{field.name},
				"%s 不是合法的 RFC3339 时刻：%v", field.name, err)
		}
	}

	accountPath := func(index int) []string {
		return []string{"account", strconv.Itoa(index)}
	}
	modelPath := func(model string, index int) []string {
		return []string{"models", model, strconv.Itoa(index)}
	}

	for i, limit := range d.Account {
		if err := checkLimitSemantics(file, root, accountPath(i), limit); err != nil {
			return err
		}
	}
	for _, model := range sortedModelNames(d.Models) {
		for i, limit := range d.Models[model] {
			if err := checkLimitSemantics(file, root, modelPath(model, i), limit); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkLimitSemantics 校验单条额度条目的语义。
func checkLimitSemantics(file string, root *yaml.Node, path []string, limit Limit) *Error {
	if limit.ResetsAt != "" {
		if _, err := time.Parse(time.RFC3339, limit.ResetsAt); err != nil {
			return pathAtNode(file, root, path, "resets_at 不是合法的 RFC3339 时刻：%v", err)
		}
	}
	if limit.Window == "absolute" && limit.ResetsAt != "" {
		return pathAtNode(file, root, path,
			"window: absolute 的条目不得携带 resets_at：absolute 表示总量或余额，安排一个未来时刻自动复活是错的，这类条目只能显式清除")
	}
	if isNaturalWindow(limit.Window) && limit.TZ == "" {
		return pathAtNode(file, root, path,
			"自然周期 %s 必须带 tz：自然周期按声明的时区切分，没有时区就无法确定窗口边界", limit.Window)
	}
	return nil
}

// isNaturalWindow 报告窗口是否为需要时区的自然周期。
func isNaturalWindow(window string) bool {
	switch window {
	case "day", "week", "month":
		return true
	default:
		return false
	}
}

// sortedModelNames 按字典序返回模型名，让语义校验的报错顺序稳定。
func sortedModelNames(models map[string][]Limit) []string {
	names := make([]string, 0, len(models))
	for name := range models {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// validateLimitsSchema 用嵌入的 limits schema 校验额度文档节点。
func validateLimitsSchema(file string, root *yaml.Node) *Error {
	return validateWithSchema(file, root, limitsSchemaURL, "额度文档")
}
