package gemini

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// geminiSchemaAllowedFields 是 Gemini 的 Schema 对象接受的字段集合。
//
// Gemini 不接受完整的 JSON Schema：additionalProperties、$schema、$defs、oneOf、allOf
// 一类字段会让上游把整条请求判为非法（OpenAI 的函数声明里 additionalProperties 很常见）。
// 重建方向因此按白名单裁剪，把「上游不认识的写法」挡在网关这一侧，
// 而不是让一次工具调用因为 Schema 方言差异直接失败。
//
// 白名单取自 Gemini 的 OpenAPI 子集（与 new-api 的实现一致）：
// 未列入的字段一律丢弃，已列入的字段原样保留、递归处理。
var geminiSchemaAllowedFields = map[string]bool{
	"anyOf":            true,
	"default":          true,
	"description":      true,
	"enum":             true,
	"example":          true,
	"format":           true,
	"items":            true,
	"maxItems":         true,
	"maxLength":        true,
	"maxProperties":    true,
	"maximum":          true,
	"minItems":         true,
	"minLength":        true,
	"minProperties":    true,
	"minimum":          true,
	"nullable":         true,
	"pattern":          true,
	"properties":       true,
	"propertyOrdering": true,
	"required":         true,
	"title":            true,
	"type":             true,
}

// geminiSchemaMaxDepth 是 Schema 递归裁剪的深度上限。
//
// 上游送来的 Schema 理论上可以是任意深的嵌套；超过上限时不再深入内层，
// 只保留浅层字段，避免一份构造出来的 Schema 把装配期或请求期拖进深递归。
const geminiSchemaMaxDepth = 64

// cleanFunctionSchema 把 domain.ToolSpec.ParametersJSON 裁剪为 Gemini 能接受的 Schema。
//
// 入参是 JSON Schema 原文：空串或 JSON null 表示没有参数，返回 nil；
// 不是 JSON 对象时按「没有参数」处理（统一内部格式里该字段的可信来源只有各协议适配器，
// 走到这里的非对象取值说明上游的声明本身就不可用，不带它比带一份坏 Schema 更好）。
func cleanFunctionSchema(parameters string) (any, error) {
	trimmed := strings.TrimSpace(parameters)
	if trimmed == "" || trimmed == jsonNullLiteral {
		return nil, nil
	}
	var decoded any
	if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
		return nil, invalidRequest("工具参数不是合法 JSON")
	}
	object, ok := decoded.(map[string]any)
	if !ok {
		return nil, nil
	}
	return cleanSchemaValue(object, 0), nil
}

// cleanSchemaValue 递归裁剪一份 Schema 取值。
//
// 只保留白名单字段，并把 type 归一化为 Gemini 的枚举写法（大写、null 折叠为 nullable）。
// 深度到顶后只做浅层裁剪，不再深入 properties / items。
func cleanSchemaValue(value any, depth int) any {
	switch typed := value.(type) {
	case map[string]any:
		cleaned := make(map[string]any, len(typed))
		for key, item := range typed {
			if !geminiSchemaAllowedFields[key] {
				continue
			}
			cleaned[key] = item
		}
		normalizeSchemaType(cleaned)
		if depth >= geminiSchemaMaxDepth {
			delete(cleaned, "properties")
			delete(cleaned, "items")
			delete(cleaned, "anyOf")
			return cleaned
		}
		if properties, ok := cleaned["properties"].(map[string]any); ok && properties != nil {
			next := make(map[string]any, len(properties))
			for name, nested := range properties {
				next[name] = cleanSchemaValue(nested, depth+1)
			}
			cleaned["properties"] = next
		}
		if items, ok := cleaned["items"].(map[string]any); ok && items != nil {
			cleaned["items"] = cleanSchemaValue(items, depth+1)
		}
		if items, ok := cleaned["items"].([]any); ok && len(items) > 0 {
			// Gemini 的 items 只接受单个 Schema；元组式声明取第一项，丢弃其余。
			cleaned["items"] = cleanSchemaValue(items[0], depth+1)
		}
		if nested, ok := cleaned["anyOf"].([]any); ok && nested != nil {
			next := make([]any, len(nested))
			for i, item := range nested {
				next[i] = cleanSchemaValue(item, depth+1)
			}
			cleaned["anyOf"] = next
		}
		return cleaned
	case []any:
		next := make([]any, len(typed))
		for i, item := range typed {
			next[i] = cleanSchemaValue(item, depth+1)
		}
		return next
	default:
		return value
	}
}

// normalizeSchemaType 把 JSON Schema 的 type 取值归一化为 Gemini 的枚举。
//
// JSON Schema 允许小写与数组两种写法（["string","null"]），Gemini 只认单个大写枚举；
// null 没有对应枚举，折叠成 nullable。无法归一化的取值原样保留，交由上游判定。
func normalizeSchemaType(schema map[string]any) {
	raw, ok := schema["type"]
	if !ok || raw == nil {
		return
	}
	switch typed := raw.(type) {
	case string:
		normalized, isNull := normalizeSchemaTypeName(typed)
		if isNull {
			schema["nullable"] = true
			delete(schema, "type")
			return
		}
		if normalized != "" {
			schema["type"] = normalized
		}
	case []any:
		nullable := false
		chosen := ""
		for _, item := range typed {
			name, ok := item.(string)
			if !ok {
				continue
			}
			normalized, isNull := normalizeSchemaTypeName(name)
			if isNull {
				nullable = true
				continue
			}
			if chosen == "" {
				chosen = normalized
			}
		}
		if nullable {
			schema["nullable"] = true
		}
		if chosen != "" {
			schema["type"] = chosen
		} else {
			delete(schema, "type")
		}
	}
}

// normalizeSchemaTypeName 把一个小写的 JSON Schema 类型名归一化为 Gemini 枚举。
//
// 第二个返回值报告该类型是否为 null：null 没有 Gemini 枚举，由调用方折叠成 nullable。
// 未识别的类型名原样返回，让上游去拒绝一个它确实不认识的类型。
func normalizeSchemaTypeName(name string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "object":
		return "OBJECT", false
	case "array":
		return "ARRAY", false
	case "string":
		return "STRING", false
	case "integer":
		return "INTEGER", false
	case "number":
		return "NUMBER", false
	case "boolean":
		return "BOOLEAN", false
	case "null":
		return "", true
	default:
		return name, false
	}
}

// ---------------------------------------------------------------------------
// 流式工具调用的参数分片
// ---------------------------------------------------------------------------

// geminiPartialCall 是一条尚未完成的流式工具调用。
//
// Gemini 的新版模型把函数参数按 JSONPath 分片下发（partialArgs），最后一个分片把
// willContinue 置为 false。分片到达时先按路径写进参数对象，等调用完成再产出一个
// 携带完整参数的内部工具调用分片：内部格式的工具调用是「一次给全参数」的，
// 拆成多个分片下发会让重建侧拼不出合法 JSON。
type geminiPartialCall struct {
	id   string
	name string
	args map[string]any
}

// partialPathSegment 是 JSONPath 里的一段：对象成员或数组下标。
type partialPathSegment struct {
	member  string
	index   int
	isIndex bool
}

// maxPartialArgArrayIndex 是数组下标的上限。
//
// 分片路径由上游给出，一个形如 $[1000000] 的下标会把参数对象撑成百万级空数组；
// 拒绝超限下标而不是按它分配内存。
const maxPartialArgArrayIndex = 4095

// applyPartialArgs 把一批参数分片写进累计的参数对象。
//
// 只有明确给出类型的分片才算写入：四种取值都不给的分片按「本片不携带取值」处理，
// 与上游把一片只用来推进 willContinue 的写法兼容。
func (c *geminiPartialCall) applyPartialArgs(partials []wirePartialArg) error {
	if len(partials) == 0 {
		return nil
	}
	if c.args == nil {
		c.args = make(map[string]any)
	}
	for _, partial := range partials {
		path, err := parsePartialArgPath(partial.JSONPath)
		if err != nil {
			return err
		}
		value, present := partialArgValue(partial)
		if !present {
			continue
		}
		updated, err := setPathValue(c.args, path, value, partial.StringValue != nil)
		if err != nil {
			return err
		}
		object, ok := updated.(map[string]any)
		if !ok {
			return fmt.Errorf("参数路径 %q 覆盖了整个参数对象", partial.JSONPath)
		}
		c.args = object
	}
	return nil
}

// partialArgValue 取出一个分片携带的取值，并报告它是否真的携带取值。
func partialArgValue(partial wirePartialArg) (any, bool) {
	switch {
	case partial.StringValue != nil:
		return *partial.StringValue, true
	case partial.NumberValue != nil:
		return *partial.NumberValue, true
	case partial.BoolValue != nil:
		return *partial.BoolValue, true
	case partial.NullValue != nil:
		return nil, true
	default:
		return nil, false
	}
}

// parsePartialArgPath 解析 Gemini 的 JSONPath 子集：`$.a.b[0]['c']`。
//
// 只支持对象成员与数组下标两种选择器；其余写法（通配、切片、过滤表达式）返回错误，
// 因为按它们写进参数对象没有确定含义。
func parsePartialArgPath(jsonPath string) ([]partialPathSegment, error) {
	path := strings.TrimSpace(jsonPath)
	if path == "" || path[0] != '$' {
		return nil, fmt.Errorf("不支持的参数路径 %q", jsonPath)
	}
	var segments []partialPathSegment
	for offset := 1; offset < len(path); {
		switch path[offset] {
		case '.':
			offset++
			start := offset
			for offset < len(path) && path[offset] != '.' && path[offset] != '[' {
				offset++
			}
			if start == offset {
				return nil, fmt.Errorf("参数路径 %q 里有空的成员名", jsonPath)
			}
			member := path[start:offset]
			if strings.ContainsAny(member, "]*?") {
				return nil, fmt.Errorf("参数路径 %q 的成员名 %q 含不支持的字符", jsonPath, member)
			}
			segments = append(segments, partialPathSegment{member: member})
		case '[':
			offset++
			if offset >= len(path) {
				return nil, fmt.Errorf("参数路径 %q 的选择器没有收尾", jsonPath)
			}
			if path[offset] == '\'' || path[offset] == '"' {
				member, next, err := parseQuotedMember(path, offset)
				if err != nil {
					return nil, fmt.Errorf("参数路径 %q 非法：%w", jsonPath, err)
				}
				offset = next
				if offset >= len(path) || path[offset] != ']' {
					return nil, fmt.Errorf("参数路径 %q 的成员选择器没有收尾", jsonPath)
				}
				offset++
				segments = append(segments, partialPathSegment{member: member})
				continue
			}
			start := offset
			for offset < len(path) && path[offset] >= '0' && path[offset] <= '9' {
				offset++
			}
			if start == offset || offset >= len(path) || path[offset] != ']' {
				return nil, fmt.Errorf("参数路径 %q 的数组选择器不受支持", jsonPath)
			}
			index, err := strconv.Atoi(path[start:offset])
			if err != nil {
				return nil, fmt.Errorf("参数路径 %q 的数组下标非法：%w", jsonPath, err)
			}
			if index > maxPartialArgArrayIndex {
				return nil, fmt.Errorf("参数路径 %q 的数组下标 %d 超过上限 %d",
					jsonPath, index, maxPartialArgArrayIndex)
			}
			offset++
			segments = append(segments, partialPathSegment{index: index, isIndex: true})
		default:
			return nil, fmt.Errorf("参数路径 %q 在偏移 %d 处有不受支持的选择器", jsonPath, offset)
		}
	}
	if len(segments) == 0 {
		return nil, fmt.Errorf("参数路径 %q 指向参数对象本身", jsonPath)
	}
	return segments, nil
}

// parseQuotedMember 解析 `['name']` 或 `["name"]` 形式的成员选择器，返回成员名与下一处偏移。
func parseQuotedMember(path string, offset int) (string, int, error) {
	quote := path[offset]
	start := offset
	offset++
	for offset < len(path) {
		if path[offset] == '\\' {
			offset += 2
			continue
		}
		if path[offset] == quote {
			raw := path[start : offset+1]
			if quote == '\'' {
				// 单引号不是合法 JSON 字符串定界符，先换成双引号再交给标准库解码，
				// 保证转义语义与 JSON 一致。
				raw = `"` + strings.ReplaceAll(strings.ReplaceAll(raw[1:len(raw)-1], `"`, `\"`), `\'`, `'`) + `"`
			}
			var member string
			if err := json.Unmarshal([]byte(raw), &member); err != nil {
				return "", 0, err
			}
			return member, offset + 1, nil
		}
		offset++
	}
	return "", 0, fmt.Errorf("成员选择器没有收尾的引号")
}

// setPathValue 按路径把取值写进当前对象或数组，返回写入后的根取值。
//
// appendString 为真表示这次写入是对同一路径上已有字符串的续写：Gemini 把长字符串
// 拆成多个分片时每一片都是 stringValue，续写而不是覆盖才能拼回原文。
func setPathValue(current any, path []partialPathSegment, value any, appendString bool) (any, error) {
	if len(path) == 0 {
		if appendString {
			if existing, ok := current.(string); ok {
				if text, ok := value.(string); ok {
					return existing + text, nil
				}
			}
		}
		return value, nil
	}
	segment := path[0]
	if segment.isIndex {
		var array []any
		switch typed := current.(type) {
		case nil:
			array = make([]any, segment.index+1)
		case []any:
			array = typed
			if len(array) <= segment.index {
				array = append(array, make([]any, segment.index-len(array)+1)...)
			}
		default:
			return nil, fmt.Errorf("数组下标 %d 落在 %T 上", segment.index, current)
		}
		updated, err := setPathValue(array[segment.index], path[1:], value, appendString)
		if err != nil {
			return nil, err
		}
		array[segment.index] = updated
		return array, nil
	}

	var object map[string]any
	switch typed := current.(type) {
	case nil:
		object = make(map[string]any)
	case map[string]any:
		object = typed
	default:
		return nil, fmt.Errorf("成员 %q 落在 %T 上", segment.member, current)
	}
	updated, err := setPathValue(object[segment.member], path[1:], value, appendString)
	if err != nil {
		return nil, err
	}
	object[segment.member] = updated
	return object, nil
}

// compactJSON 返回 JSON 的紧凑原文；非对象或非法 JSON 时返回空串。
//
// 用于把工具调用的参数对象编码为内部格式里的字符串：统一内部格式保留参数原文，
// 空对象与缺失都归为 "{}"。
func compactJSON(raw []byte) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == jsonNullLiteral {
		return ""
	}
	var buffer bytes.Buffer
	if err := json.Compact(&buffer, trimmed); err != nil {
		return ""
	}
	return buffer.String()
}
