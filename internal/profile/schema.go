package profile

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"gopkg.in/yaml.v3"
)

// schema 目录随代码进版本历史：go:embed 让 schema 成为二进制的一部分，
// 因此不依赖外部文件、也不需要为它改 .gitignore。
//
//go:embed schema/profile-1.yaml schema/index-1.yaml schema/limits-1.yaml
var schemaFS embed.FS

const (
	profileSchemaURL = "https://nova.dev/schema/profile-1.yaml"
	indexSchemaURL   = "https://nova.dev/schema/index-1.yaml"
	limitsSchemaURL  = "https://nova.dev/schema/limits-1.yaml"
)

// schemaFiles 是嵌入的 schema 文件名与编译时使用的 URL。
//
// 全部先注册再编译：profile 通过 $ref 引用 limits 的条目定义，注册次序不影响解析，
// 但编译必须发生在全部资源就位之后。
var schemaFiles = []struct {
	file string
	url  string
}{
	{"profile-1.yaml", profileSchemaURL},
	{"index-1.yaml", indexSchemaURL},
	{"limits-1.yaml", limitsSchemaURL},
}

var (
	schemasOnce sync.Once
	schemas     map[string]*jsonschema.Schema
	schemasErr  error
)

// compiledSchema 返回嵌入 schema 编译结果；编译只发生一次，结果在进程内复用。
func compiledSchema(url string) (*jsonschema.Schema, error) {
	schemasOnce.Do(compileSchemas)
	if schemasErr != nil {
		return nil, schemasErr
	}
	schema, ok := schemas[url]
	if !ok {
		return nil, fmt.Errorf("内置 schema 缺少 %s", url)
	}
	return schema, nil
}

// compileSchemas 把嵌入的三份 schema 注册并编译。
//
// 编译失败是程序自身的错误（schema 文件写错），不是档案问题，因此不构造带位置的
// 档案错误，由调用方包装成普通错误上抛。
func compileSchemas() {
	compiler := jsonschema.NewCompiler()
	for _, entry := range schemaFiles {
		raw, err := schemaFS.ReadFile("schema/" + entry.file)
		if err != nil {
			schemasErr = fmt.Errorf("读取内置 schema %s 失败：%w", entry.file, err)
			return
		}
		doc, err := decodeSchema(raw)
		if err != nil {
			schemasErr = fmt.Errorf("解析内置 schema %s 失败：%w", entry.file, err)
			return
		}
		if err := compiler.AddResource(entry.url, doc); err != nil {
			schemasErr = fmt.Errorf("注册内置 schema %s 失败：%w", entry.file, err)
			return
		}
	}
	compiled := make(map[string]*jsonschema.Schema, len(schemaFiles))
	for _, entry := range schemaFiles {
		schema, err := compiler.Compile(entry.url)
		if err != nil {
			schemasErr = fmt.Errorf("编译内置 schema %s 失败：%w", entry.file, err)
			return
		}
		compiled[entry.url] = schema
	}
	schemas = compiled
}

// decodeSchema 把嵌入的 YAML schema 转成 jsonschema 要求的通用值。
//
// schema 写成 YAML 是为了与档案本身同一份书写习惯；jsonschema 只接受 JSON 形状的取值，
// 因此经一次 YAML → JSON 的转换。
func decodeSchema(raw []byte) (any, error) {
	var value any
	if err := yaml.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
}

// validateProfileSchema 用嵌入的 profile schema 校验档案节点，返回第一条可定位的错误。
func validateProfileSchema(file string, root *yaml.Node) *Error {
	return validateWithSchema(file, root, profileSchemaURL, "档案")
}

// validateIndexSchema 用嵌入的 index schema 校验索引节点，返回第一条可定位的错误。
func validateIndexSchema(file string, root *yaml.Node) *Error {
	return validateWithSchema(file, root, indexSchemaURL, "索引")
}

// validateWithSchema 用指定内嵌 schema 校验节点树，返回第一条可定位的错误。
//
// 档案与索引走同一条校验路径：两者的解析约束（禁锚点、别名、多文档与重复键）与
// 错误定位机制都由 parseDocument 与 schemaErrors 提供，这里只把 schema 与文案换掉。
func validateWithSchema(file string, root *yaml.Node, schemaURL, what string) *Error {
	schema, err := compiledSchema(schemaURL)
	if err != nil {
		return &Error{File: file, Msg: err.Error()}
	}
	instance, err := instanceFromNode(root)
	if err != nil {
		return &Error{File: file, Msg: fmt.Sprintf("%s无法转换为校验输入：%v", what, err)}
	}
	if err := schema.Validate(instance); err != nil {
		var validationErr *jsonschema.ValidationError
		if errors.As(err, &validationErr) {
			if errs := schemaErrors(file, root, validationErr); len(errs) > 0 {
				return errs[0]
			}
		}
		return &Error{File: file, Msg: fmt.Sprintf("%s不符合 schema：%v", what, err)}
	}
	return nil
}

// schemaErrors 把 jsonschema 的校验错误摊平成带位置的中文错误。
//
// 组合关键字（Group / Ref / AllOf）递归展开到具体失败项；AnyOf 与 OneOf 不展开，
// 因为展开后只会得到一串「某条分支为何不成立」的噪声，而使用者需要知道的是
// 该位置允许哪些取值组合。
func schemaErrors(file string, root *yaml.Node, ve *jsonschema.ValidationError) []*Error {
	switch k := ve.ErrorKind.(type) {
	case *kind.Group, *kind.Schema, *kind.Reference, *kind.AllOf:
		if len(ve.Causes) > 0 {
			var out []*Error
			for _, cause := range ve.Causes {
				out = append(out, schemaErrors(file, root, cause)...)
			}
			if len(out) > 0 {
				return out
			}
		}
		return []*Error{pathAtNode(file, root, ve.InstanceLocation, "%s", describe(ve))}
	case *kind.AdditionalProperties:
		var out []*Error
		for _, property := range k.Properties {
			path := append(append([]string{}, ve.InstanceLocation...), property)
			out = append(out, unknownFieldError(file, root, ve.InstanceLocation, property, path))
		}
		if len(out) == 0 {
			out = append(out, pathAtNode(file, root, ve.InstanceLocation, "存在未知字段"))
		}
		return out
	case *kind.AnyOf:
		// limits-1.yaml 只在额度条目上使用 anyOf，用于表达「必须能确定 remaining」。
		return []*Error{pathAtNode(file, root, ve.InstanceLocation,
			"额度条目无法确定 remaining：需给出 remaining，或同时给出 limit 与 used，或同时给出 limit 与 remaining；真无限额度应声明 unbounded: true")}
	case *kind.OneOf:
		// profile-1.yaml 只在 usage 上使用 oneOf，用于表达 probe 与 exec 二选一。
		return []*Error{pathAtNode(file, root, ve.InstanceLocation,
			"usage 必须且只能给出 probe 或 exec 之一")}
	default:
		return []*Error{pathAtNode(file, root, ve.InstanceLocation, "%s", describe(ve))}
	}
}

// unknownFieldError 把未知字段的报错指向字段名本身。
func unknownFieldError(file string, root *yaml.Node, parentPath []string, property string, path []string) *Error {
	if key := mappingKeyNode(nodeAt(root, parentPath), property); key != nil {
		return errorAt(file, key, "未知字段 %q", property)
	}
	return pathAtNode(file, root, path, "未知字段 %q", property)
}

// describe 把一条叶子校验错误翻成中文。
//
// 只覆盖本仓 schema 实际会用到的关键字；其余落回「不满足约束 <关键字路径>」，
// 保留可定位性，不把英文原文当作文案抛给使用者。
func describe(ve *jsonschema.ValidationError) string {
	switch k := ve.ErrorKind.(type) {
	case *kind.Required:
		return fmt.Sprintf("缺少必填字段 %s", quoteList(k.Missing))
	case *kind.Type:
		return fmt.Sprintf("字段类型应为 %s，实际为 %s", strings.Join(k.Want, " 或 "), k.Got)
	case *kind.Enum:
		return fmt.Sprintf("取值必须是 %s 之一", quoteAnyList(k.Want))
	case *kind.Const:
		return fmt.Sprintf("取值必须是 %v", k.Want)
	case *kind.Pattern:
		return fmt.Sprintf("取值 %q 不符合模式 %q", k.Got, k.Want)
	case *kind.Format:
		return fmt.Sprintf("取值不符合格式 %q", k.Want)
	case *kind.MinProperties:
		return fmt.Sprintf("字段数不得少于 %d，实际为 %d", k.Want, k.Got)
	case *kind.MaxProperties:
		return fmt.Sprintf("字段数不得多于 %d，实际为 %d", k.Want, k.Got)
	case *kind.MinItems:
		return fmt.Sprintf("元素数不得少于 %d，实际为 %d", k.Want, k.Got)
	case *kind.MaxItems:
		return fmt.Sprintf("元素数不得多于 %d，实际为 %d", k.Want, k.Got)
	case *kind.MinLength:
		return fmt.Sprintf("长度不得少于 %d，实际为 %d", k.Want, k.Got)
	case *kind.MaxLength:
		return fmt.Sprintf("长度不得多于 %d，实际为 %d", k.Want, k.Got)
	case *kind.UniqueItems:
		return "元素不得重复"
	case *kind.Minimum:
		return fmt.Sprintf("取值不得小于 %s", ratString(k.Want))
	case *kind.Maximum:
		return fmt.Sprintf("取值不得大于 %s", ratString(k.Want))
	case *kind.ExclusiveMinimum:
		return fmt.Sprintf("取值必须大于 %s", ratString(k.Want))
	case *kind.ExclusiveMaximum:
		return fmt.Sprintf("取值必须小于 %s", ratString(k.Want))
	case *kind.MultipleOf:
		return fmt.Sprintf("取值必须是 %s 的整数倍", ratString(k.Want))
	default:
		return fmt.Sprintf("不满足约束 %s", strings.Join(ve.ErrorKind.KeywordPath(), "/"))
	}
}

// quoteList 把字段名列表排成「"a"、"b"」形式。
func quoteList(names []string) string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = strconv.Quote(name)
	}
	return strings.Join(quoted, "、")
}

// quoteAnyList 把取值候选项排成人读形式。
func quoteAnyList(values []any) string {
	quoted := make([]string, len(values))
	for i, value := range values {
		quoted[i] = fmt.Sprintf("%v", value)
	}
	return strings.Join(quoted, "、")
}

// ratString 把校验用的有理数排成人读形式，整数不带小数点。
func ratString(rat interface{ Float64() (float64, bool) }) string {
	value, _ := rat.Float64()
	return strconv.FormatFloat(value, 'f', -1, 64)
}
