package anthropic

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/sumwai/nova/internal/domain"
)

// 编译期断言：本适配器实现上游请求构建、上游请求头与流式错误帧三项能力。
var (
	_ domain.UpstreamRequestBuilder = (*Adapter)(nil)
	_ domain.UpstreamHeaderProvider = (*Adapter)(nil)
	_ domain.StreamErrorEncoder     = (*Adapter)(nil)
)

const (
	// anthropicVersionHeader 是 Anthropic Messages 必需的协议版本头，
	// anthropicVersionValue 是其取值。渠道配置可以覆盖。
	anthropicVersionHeader = "anthropic-version"
	anthropicVersionValue  = "2023-06-01"

	// maxTokensField 是本协议输出上限的字段名。
	maxTokensField = "max_tokens"

	// modelField 是本协议模型名的字段名。
	modelField = "model"

	// dataURIPrefix 与 dataURIBase64Suffix 用于识别并拆解 base64 图片的 data URI。
	dataURIPrefix       = "data:"
	dataURIBase64Suffix = ";base64"
	emptyToolInputJSON  = "{}"
	userRole            = "user"

	// jsonFieldType 是内容块与图片 source 里的类型字段名。
	jsonFieldType = "type"
	// imageSourceTypeURL 是 Anthropic url 图片 source 的类型取值，
	// imageURLField 是图片 source 里的 URL 字段名。
	imageSourceTypeURL = "url"
	imageURLField      = "url"
)

// upstreamRequestWire 是重建上游请求体时使用的线上结构。
type upstreamRequestWire struct {
	Model       string                `json:"model"`
	MaxTokens   *int                  `json:"max_tokens,omitempty"`
	System      string                `json:"system,omitempty"`
	Messages    []upstreamMessageWire `json:"messages"`
	Tools       []upstreamToolWire    `json:"tools,omitempty"`
	ToolChoice  json.RawMessage       `json:"tool_choice,omitempty"`
	Temperature *float64              `json:"temperature,omitempty"`
	Stream      bool                  `json:"stream"`
}

type upstreamMessageWire struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// upstreamMessageBlocks 是合并阶段用的折叠结果：一条消息的角色与可拼接的块列表。
type upstreamMessageBlocks struct {
	role   string
	blocks []any
}

type upstreamToolWire struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// EncodeRequest 按内部统一格式重建 Anthropic Messages 请求体，用于跨协议转发。
//
// 本协议的 messages 只有 user 与 assistant 两种角色：内部 system 消息上提为顶层
// system 字符串，内部 tool 角色消息折叠为 user 消息里的 tool_result 块。
// max_tokens 为本协议必填，按输出上限的统一口径计算：
//   - 客户端声明且未超渠道上限时保持
//   - 超限时钳制，未声明时用渠道上限补齐
//
// 渠道未配置上限时不设置该字段，此时上游必然拒绝，属渠道配置缺失
// （Anthropic 渠道必须配置默认输出上限），不再由本适配器报 CodeInternal。
//
// 第二个返回值报告本次重建相对客户端请求实际改过的改写项：模型名被替换或补齐记request_model，
// 输出上限被钳制或补齐记 request_output_limit。本协议没有索取用量开关，该改写项不产生标注。
func (a *Adapter) EncodeRequest(req *domain.Request, options domain.RewriteOptions) ([]byte, domain.RewriteParts, error) {
	if req == nil {
		return nil, nil, domain.NewError(domain.CodeInternal, "请求为空")
	}
	system, err := encodeAnthropicSystem(req.Messages)
	if err != nil {
		return nil, nil, err
	}
	messages, err := encodeAnthropicMessages(req.Messages)
	if err != nil {
		return nil, nil, err
	}
	if len(messages) == 0 {
		return nil, nil, domain.NewError(domain.CodeInternal, "Anthropic Messages 要求至少一条消息")
	}
	toolChoice, err := encodeAnthropicToolChoice(req.ToolChoice)
	if err != nil {
		return nil, nil, err
	}
	wire := upstreamRequestWire{
		Model:       domain.UpstreamModelName(req.Model, options),
		System:      system,
		Messages:    messages,
		Tools:       encodeAnthropicTools(req.Tools),
		ToolChoice:  toolChoice,
		Temperature: req.Temperature,
		Stream:      req.Stream,
	}
	var parts domain.RewriteParts
	if wire.Model != req.Model {
		parts = append(parts, domain.RewritePartRequestModel)
	}
	if maxTokens := domain.OutputLimit(req.MaxTokens, options.MaxOutputTokens); maxTokens > 0 {
		wire.MaxTokens = &maxTokens
		if maxTokens != req.MaxTokens {
			parts = append(parts, domain.RewritePartRequestOutputLimit)
		}
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, nil, domain.NewError(domain.CodeInternal, "编码上游请求失败").WithCause(err)
	}
	return body, parts, nil
}

// RewriteRawBody 对同协议透传的原始报文做字段级改写。
//
// 改写项有模型名与 max_tokens（按输出上限的统一口径）。本协议的用量随
// message_start 与 message_delta 下发、没有「索取用量」开关，故 IncludeUsage
// 打开时保持原样。第二个返回值由本适配器按实际发生的变化填报，无改动时为空。
func (a *Adapter) RewriteRawBody(body []byte, options domain.RewriteOptions) ([]byte, domain.RewriteParts, error) {
	if options.MaxOutputTokens == nil && options.UpstreamModel == "" {
		return body, nil, nil
	}
	fields, err := domain.DecodeRawFields(body)
	if err != nil {
		return nil, nil, err
	}
	var parts domain.RewriteParts
	if options.UpstreamModel != "" && domain.SetRawStringField(fields, modelField, options.UpstreamModel) {
		parts = append(parts, domain.RewritePartRequestModel)
	}
	if options.MaxOutputTokens != nil &&
		domain.SetRawOutputLimit(fields, []string{maxTokensField}, maxTokensField, options.MaxOutputTokens) {
		parts = append(parts, domain.RewritePartRequestOutputLimit)
	}
	if len(parts) == 0 {
		return body, nil, nil
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return nil, nil, domain.NewError(domain.CodeInternal, "编码改写后的请求失败").WithCause(err)
	}
	return encoded, parts, nil
}

// UpstreamHeaders 返回调用上游 Messages 时应携带的请求头。
//
// 内置 anthropic-version。渠道配置同名时覆盖内置值，其余原样补充。
func (a *Adapter) UpstreamHeaders(channelHeaders http.Header) http.Header {
	merged := domain.CloneHeader(channelHeaders)
	if merged.Get(anthropicVersionHeader) == "" {
		merged.Set(anthropicVersionHeader, anthropicVersionValue)
	}
	return merged
}

// EncodeStreamError 把错误编码为 Anthropic 的流式错误帧。
//
// 帧形如 event: error 加 data: {"type":"error","error":{...}}，与面向客户端的
// 非流式错误体共用同一份正文。
func (a *Adapter) EncodeStreamError(err error) ([]byte, bool) {
	_, body := a.EncodeError(err)
	var buf bytes.Buffer
	if writeErr := writeSSE(&buf, eventError, json.RawMessage(body)); writeErr != nil {
		return nil, false
	}
	return buf.Bytes(), true
}

// encodeAnthropicSystem 把 system 角色的消息上提为顶层 system 字符串。
func encodeAnthropicSystem(messages []domain.Message) (string, error) {
	var texts []string
	for i, message := range messages {
		if message.Role != domain.RoleSystem {
			continue
		}
		for j, part := range message.Parts {
			if part.Kind != domain.PartText {
				return "", domain.NewError(domain.CodeInternal,
					fmt.Sprintf("第 %d 条 system 消息第 %d 个片段不是文本，Anthropic 无法表达", i, j))
			}
			texts = append(texts, part.Text)
		}
	}
	return strings.Join(texts, "\n"), nil
}

// encodeAnthropicMessages 把非 system 消息编码为 user / assistant 消息。
//
// 内部 tool 角色的消息（工具结果）折叠为一条 user 消息，工具结果块按原顺序排列。
// 折叠后对相邻同角色消息做一趟合并：Anthropic Messages 要求消息角色严格交替，且同一
// 条 assistant 的多个 tool_use 必须由同一条后续 user 的 tool_result 逐一对齐，否则
// 上游会以「tool call result does not follow tool call」拒绝。合并只改变消息条数与
// 块的归属，不改写任何块的字段。user 侧把 tool_result 块前移到前部、其余片段保持
// 原相对顺序，assistant 侧按原相对顺序排列。
func encodeAnthropicMessages(messages []domain.Message) ([]upstreamMessageWire, error) {
	merged := make([]upstreamMessageBlocks, 0, len(messages))
	for i, message := range messages {
		var current upstreamMessageBlocks
		switch message.Role {
		case domain.RoleSystem:
			continue
		case domain.RoleUser, domain.RoleAssistant:
			blocks, err := encodeAnthropicBlocks(i, message.Role, message.Parts)
			if err != nil {
				return nil, err
			}
			current = upstreamMessageBlocks{role: string(message.Role), blocks: blocks}
		case domain.RoleTool:
			blocks, err := encodeAnthropicToolResults(i, message.Parts)
			if err != nil {
				return nil, err
			}
			current = upstreamMessageBlocks{role: userRole, blocks: blocks}
		default:
			return nil, domain.NewError(domain.CodeInternal,
				fmt.Sprintf("第 %d 条消息角色 %q 无法编码为 Anthropic Messages 请求", i, string(message.Role)))
		}
		if n := len(merged); n > 0 && merged[n-1].role == current.role {
			merged[n-1].blocks = append(merged[n-1].blocks, current.blocks...)
			continue
		}
		merged = append(merged, current)
	}

	out := make([]upstreamMessageWire, 0, len(merged))
	for _, message := range merged {
		blocks := message.blocks
		if message.role == userRole {
			blocks = toolResultsFirst(blocks)
		}
		encoded, err := json.Marshal(blocks)
		if err != nil {
			return nil, domain.NewError(domain.CodeInternal, "编码消息内容失败").WithCause(err)
		}
		out = append(out, upstreamMessageWire{Role: message.role, Content: encoded})
	}
	return out, nil
}

// toolResultsFirst 把 user 消息里的 tool_result 块前移到前部，其余片段保持原相对顺序：
// 工具结果必须让上游能明确地对上它应答的那条工具调用。
func toolResultsFirst(blocks []any) []any {
	results := make([]any, 0, len(blocks))
	rest := make([]any, 0, len(blocks))
	for _, block := range blocks {
		if isToolResultBlock(block) {
			results = append(results, block)
			continue
		}
		rest = append(rest, block)
	}
	if len(results) == 0 {
		return blocks
	}
	return append(results, rest...)
}

// isToolResultBlock 报告一个已编码的内容块是否是 tool_result。
func isToolResultBlock(block any) bool {
	fields, ok := block.(map[string]any)
	if !ok {
		return false
	}
	return fields[jsonFieldType] == blockTypeToolResult
}

func encodeAnthropicBlocks(index int, role domain.Role, parts []domain.Part) ([]any, error) {
	if len(parts) == 0 {
		return nil, domain.NewError(domain.CodeInternal, fmt.Sprintf("第 %d 条消息内容为空", index))
	}
	blocks := make([]any, 0, len(parts))
	for _, part := range parts {
		block, err := encodeAnthropicBlock(index, role, part)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, block)
	}
	return blocks, nil
}

func encodeAnthropicBlock(index int, role domain.Role, part domain.Part) (any, error) {
	switch part.Kind {
	case domain.PartText:
		return map[string]any{jsonFieldType: blockTypeText, "text": part.Text}, nil
	case domain.PartImage:
		return encodeAnthropicImage(index, part.ImageURL)
	case domain.PartToolCall:
		if role != domain.RoleAssistant {
			return nil, domain.NewError(domain.CodeInternal,
				fmt.Sprintf("第 %d 条消息不是 assistant，不能携带 tool_use 块", index))
		}
		if part.ToolCall == nil {
			return nil, domain.NewError(domain.CodeInternal, fmt.Sprintf("第 %d 条消息的工具调用片段缺少内容", index))
		}
		input := strings.TrimSpace(part.ToolCall.Arguments)
		if input == "" {
			input = emptyToolInputJSON
		}
		if input[0] != '{' || !json.Valid([]byte(input)) {
			return nil, domain.NewError(domain.CodeInternal,
				fmt.Sprintf("第 %d 条消息的工具调用参数不是合法 JSON 对象", index))
		}
		return map[string]any{
			jsonFieldType: blockTypeToolUse,
			"id":          part.ToolCall.ID,
			"name":        part.ToolCall.Name,
			"input":       json.RawMessage(input),
		}, nil
	case domain.PartToolResult:
		if part.ToolResult == nil {
			return nil, domain.NewError(domain.CodeInternal, fmt.Sprintf("第 %d 条消息的工具结果片段缺少内容", index))
		}
		return map[string]any{
			jsonFieldType: blockTypeToolResult,
			"tool_use_id": part.ToolResult.ToolCallID,
			"content":     part.ToolResult.Content,
		}, nil
	default:
		return nil, domain.NewError(domain.CodeInternal,
			fmt.Sprintf("第 %d 条消息包含无法编码的片段类型 %q", index, string(part.Kind)))
	}
}

// encodeAnthropicToolResults 把 tool 角色的消息折叠为可拼接的 tool_result 块切片。
func encodeAnthropicToolResults(index int, parts []domain.Part) ([]any, error) {
	if len(parts) == 0 {
		return nil, domain.NewError(domain.CodeInternal, fmt.Sprintf("第 %d 条消息内容为空", index))
	}
	blocks := make([]any, 0, len(parts))
	for _, part := range parts {
		if part.Kind != domain.PartToolResult || part.ToolResult == nil {
			return nil, domain.NewError(domain.CodeInternal,
				fmt.Sprintf("第 %d 条 tool 消息包含非工具结果片段 %q", index, string(part.Kind)))
		}
		blocks = append(blocks, map[string]any{
			jsonFieldType: blockTypeToolResult,
			"tool_use_id": part.ToolResult.ToolCallID,
			"content":     part.ToolResult.Content,
		})
	}
	return blocks, nil
}

// encodeAnthropicImage 把内部图片 URL 编码为 Anthropic 图片块：
//   - data URI 还原为 base64 source
//   - 其余按 url source 原样传递
func encodeAnthropicImage(index int, imageURL string) (any, error) {
	if imageURL == "" {
		return nil, domain.NewError(domain.CodeInternal, fmt.Sprintf("第 %d 条消息的图片片段缺少 image_url", index))
	}
	mediaType, data, ok := splitDataURI(imageURL)
	if ok {
		return map[string]any{
			jsonFieldType: blockTypeImage,
			"source": map[string]string{
				jsonFieldType: "base64",
				"media_type":  mediaType,
				"data":        data,
			},
		}, nil
	}
	return map[string]any{
		jsonFieldType: blockTypeImage,
		"source":      map[string]string{jsonFieldType: imageSourceTypeURL, imageURLField: imageURL},
	}, nil
}

// splitDataURI 拆解 base64 图片的 data URI，返回 media type 与 base64 数据。
// 非 data URI 或缺少 base64 标记时第二个返回值报告失败。
func splitDataURI(uri string) (string, string, bool) {
	rest, ok := strings.CutPrefix(uri, dataURIPrefix)
	if !ok {
		return "", "", false
	}
	header, data, ok := strings.Cut(rest, ",")
	if !ok {
		return "", "", false
	}
	mediaType, ok := strings.CutSuffix(header, dataURIBase64Suffix)
	if !ok || mediaType == "" {
		return "", "", false
	}
	return mediaType, data, true
}

func encodeAnthropicTools(tools []domain.ToolSpec) []upstreamToolWire {
	if len(tools) == 0 {
		return nil
	}
	out := make([]upstreamToolWire, 0, len(tools))
	for _, tool := range tools {
		item := upstreamToolWire{Name: tool.Name, Description: tool.Description}
		parameters := strings.TrimSpace(tool.ParametersJSON)
		if parameters == "" {
			parameters = emptyToolInputJSON
		}
		item.InputSchema = json.RawMessage(parameters)
		out = append(out, item)
	}
	return out
}

// encodeAnthropicToolChoice 把内部工具选择映射为 Anthropic 的 tool_choice：
//   - auto/none 原样
//   - 必须调用映射为 any
//   - 指定工具映射为 {type:tool,name:…}
//
// 未指定时不携带该字段。无法映射的模式返回内部错误，不得静默丢弃。
func encodeAnthropicToolChoice(choice domain.ToolChoice) (json.RawMessage, error) {
	switch choice.Mode {
	case domain.ToolChoiceUnset:
		return nil, nil
	case domain.ToolChoiceAuto:
		return json.RawMessage(`{"type":"auto"}`), nil
	case domain.ToolChoiceRequired:
		return json.RawMessage(`{"type":"any"}`), nil
	case domain.ToolChoiceNone:
		return json.RawMessage(`{"type":"none"}`), nil
	case domain.ToolChoiceTool:
		name := strings.TrimSpace(choice.Name)
		if name == "" {
			return nil, domain.NewError(domain.CodeInternal, "指定工具时缺少工具名")
		}
		encoded, err := json.Marshal(map[string]string{jsonFieldType: toolChoiceTypeTool, "name": name})
		if err != nil {
			return nil, domain.NewError(domain.CodeInternal, "编码 tool_choice 失败").WithCause(err)
		}
		return encoded, nil
	default:
		return nil, domain.NewError(domain.CodeInternal, fmt.Sprintf("无法编码的工具选择模式 %q", string(choice.Mode)))
	}
}
