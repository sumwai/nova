package openairesponses

import (
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
	// maxOutputTokensField 是本协议输出上限的字段名。
	maxOutputTokensField = "max_output_tokens"
	// modelField 是本协议模型名的字段名。
	modelField = "model"
	// inputTextType 与 inputImageType 是 input 消息内容片段的类型取值。
	inputTextType  = "input_text"
	inputImageType = "input_image"
	// functionToolType 是本协议工具定义的 type 取值。
	functionToolType = "function"
	// streamErrorEvent 是流式错误事件名。
	streamErrorEvent = "error"
)

// upstreamRequestWire 是重建上游请求体时使用的线上结构。
type upstreamRequestWire struct {
	Model           string             `json:"model"`
	Input           []any              `json:"input"`
	Instructions    string             `json:"instructions,omitempty"`
	MaxOutputTokens *int               `json:"max_output_tokens,omitempty"`
	Temperature     *float64           `json:"temperature,omitempty"`
	Tools           []upstreamToolWire `json:"tools,omitempty"`
	ToolChoice      json.RawMessage    `json:"tool_choice,omitempty"`
	Stream          bool               `json:"stream"`
}

type upstreamMessageItem struct {
	Type    string            `json:"type"`
	Role    string            `json:"role"`
	Content []upstreamContent `json:"content"`
}

type upstreamContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

type upstreamFunctionCallItem struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type upstreamFunctionCallOutputItem struct {
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

type upstreamToolWire struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// upstreamToolChoiceFunctionWire 是 Responses 指定具体函数的对象形态：
// {"type":"function","name":"…"}。
type upstreamToolChoiceFunctionWire struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// upstreamStreamErrorWire 是 error 流式事件的载荷。
type upstreamStreamErrorWire struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// EncodeRequest 按内部统一格式重建 Responses 请求体，用于跨协议转发。
//
// 重建规则包括：
//   - instructions：system 消息上提为顶层字符串
//   - input：assistant 文本用 output_text、其余角色文本用 input_text
//   - function_call / function_call_output：工具调用与工具结果各自独立成条
//
// 第二个返回值报告本次重建相对客户端请求实际改过的改写项：模型名被替换或补齐记 request_model，
// 输出上限被钳制或补齐记 request_output_limit；本协议没有索取用量开关，该改写项不产生标注。
func (a *Adapter) EncodeRequest(req *domain.Request, options domain.RewriteOptions) ([]byte, domain.RewriteParts, error) {
	if req == nil {
		return nil, nil, domain.NewError(domain.CodeInternal, "请求为空")
	}
	instructions, err := encodeInstructions(req.Messages)
	if err != nil {
		return nil, nil, err
	}
	input, err := encodeResponsesInput(req.Messages)
	if err != nil {
		return nil, nil, err
	}
	if len(input) == 0 {
		return nil, nil, domain.NewError(domain.CodeInternal, "Responses 要求至少一条 input 条目")
	}
	toolChoice, err := encodeResponsesToolChoice(req.ToolChoice)
	if err != nil {
		return nil, nil, err
	}
	wire := upstreamRequestWire{
		Model:        domain.UpstreamModelName(req.Model, options),
		Input:        input,
		Instructions: instructions,
		Temperature:  req.Temperature,
		Tools:        encodeResponsesTools(req.Tools),
		ToolChoice:   toolChoice,
		Stream:       req.Stream,
	}
	var parts domain.RewriteParts
	if wire.Model != req.Model {
		parts = append(parts, domain.RewritePartRequestModel)
	}
	if limit := domain.OutputLimit(req.MaxTokens, options.MaxOutputTokens); limit > 0 {
		wire.MaxOutputTokens = &limit
		if limit != req.MaxTokens {
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
// 改写项有模型名与 max_output_tokens（按输出上限的统一口径）；本协议的用量随
// response.completed 下发、没有「索取用量」开关，故 IncludeUsage 打开时保持原样。
// 第二个返回值由本适配器按实际发生的变化填报，无改动时为空。
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
		domain.SetRawOutputLimit(fields, []string{maxOutputTokensField}, maxOutputTokensField, options.MaxOutputTokens) {
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

// UpstreamHeaders 返回调用上游 Responses 时应携带的请求头。
//
// 本协议没有内置的必需请求头（凭据由流水线按 Route.CredentialRef 注入），
// 因此只回传渠道配置；nil 入参返回非 nil 的空 map，保证调用方可以直接写入。
func (a *Adapter) UpstreamHeaders(channelHeaders http.Header) http.Header {
	return domain.CloneHeader(channelHeaders)
}

// EncodeStreamError 把错误编码为 Responses 的 error 流式事件。
func (a *Adapter) EncodeStreamError(err error) ([]byte, bool) {
	code := domain.CodeInternal
	message := "内部错误"
	if domainErr := domain.AsError(err); domainErr != nil {
		code = domainErr.Code
		if domainErr.Message != "" {
			message = domainErr.Message
		}
	}
	frame, marshalErr := encodeFrameE(streamErrorEvent, upstreamStreamErrorWire{
		Type:    streamErrorEvent,
		Code:    string(code),
		Message: message,
	})
	if marshalErr != nil {
		return nil, false
	}
	return frame, true
}

// encodeInstructions 把 system 角色的消息上提为顶层 instructions 字符串。
func encodeInstructions(messages []domain.Message) (string, error) {
	var texts []string
	for i, message := range messages {
		if message.Role != domain.RoleSystem {
			continue
		}
		for j, part := range message.Parts {
			if part.Kind != domain.PartText {
				return "", domain.NewError(domain.CodeInternal,
					fmt.Sprintf("第 %d 条 system 消息第 %d 个片段不是文本，Responses 无法表达", i, j))
			}
			texts = append(texts, part.Text)
		}
	}
	return strings.Join(texts, "\n"), nil
}

// encodeResponsesInput 把消息编码为 Responses 的 input 条目数组。
//
// 工具结果在内部可能挂在 user 角色的消息上（Anthropic 形态）或 tool 角色的消息上（OpenAI 形态），
// 本协议用独立的 function_call_output 条目承载：一条消息里的工具结果片段按片段顺序逐个展开、
// 不合并，全部排在残留 message 条目之前。其余片段为空时不产出残留条目。
// 工具结果无法对应到此前已出现的 assistant 工具调用时报 invalid_request。
func encodeResponsesInput(messages []domain.Message) ([]any, error) {
	items := make([]any, 0, len(messages))
	// 此前已出现的 assistant 工具调用 id；工具结果必须能找到对应的前置调用。
	seenToolCalls := make(map[string]struct{})
	for i, message := range messages {
		switch message.Role {
		case domain.RoleSystem:
			continue
		case domain.RoleAssistant:
			encoded, err := encodeResponsesMessage(i, message)
			if err != nil {
				return nil, err
			}
			items = append(items, encoded...)
			addAssistantToolCallIDs(seenToolCalls, message)
		case domain.RoleUser:
			toolResults, rest, err := splitResponsesToolResults(i, message.Parts, seenToolCalls, true)
			if err != nil {
				return nil, err
			}
			items = append(items, toolResults...)
			if len(rest) > 0 {
				encoded, err := encodeResponsesMessage(i, domain.Message{Role: domain.RoleUser, Parts: rest})
				if err != nil {
					return nil, err
				}
				items = append(items, encoded...)
			}
		case domain.RoleTool:
			toolResults, _, err := splitResponsesToolResults(i, message.Parts, seenToolCalls, false)
			if err != nil {
				return nil, err
			}
			items = append(items, toolResults...)
		default:
			return nil, domain.NewError(domain.CodeInternal,
				fmt.Sprintf("第 %d 条消息角色 %q 无法编码为 Responses 请求", i, string(message.Role)))
		}
	}
	return items, nil
}

// encodeResponsesMessage 编码一条 user / assistant 消息：文本与图片合成 message 条目，
// 工具调用在出现位置先结束当前 message 条目再单独成条，保持片段顺序。
func encodeResponsesMessage(index int, message domain.Message) ([]any, error) {
	items := make([]any, 0, len(message.Parts))
	content := make([]upstreamContent, 0, len(message.Parts))
	flush := func() {
		if len(content) == 0 {
			return
		}
		items = append(items, upstreamMessageItem{Type: itemTypeMessage, Role: string(message.Role), Content: content})
		content = nil
	}
	for _, part := range message.Parts {
		switch part.Kind {
		case domain.PartText:
			partType := inputTextType
			if message.Role == domain.RoleAssistant {
				partType = contentTypeOutputText
			}
			content = append(content, upstreamContent{Type: partType, Text: part.Text})
		case domain.PartImage:
			if message.Role != domain.RoleUser {
				return nil, domain.NewError(domain.CodeInternal,
					fmt.Sprintf("第 %d 条消息不是 user，不能携带图片", index))
			}
			if part.ImageURL == "" {
				return nil, domain.NewError(domain.CodeInternal,
					fmt.Sprintf("第 %d 条消息的图片片段缺少 image_url", index))
			}
			content = append(content, upstreamContent{Type: inputImageType, ImageURL: part.ImageURL})
		case domain.PartToolCall:
			if message.Role != domain.RoleAssistant {
				return nil, domain.NewError(domain.CodeInternal,
					fmt.Sprintf("第 %d 条消息不是 assistant，不能携带工具调用", index))
			}
			if part.ToolCall == nil {
				return nil, domain.NewError(domain.CodeInternal,
					fmt.Sprintf("第 %d 条消息的工具调用片段缺少内容", index))
			}
			flush()
			items = append(items, upstreamFunctionCallItem{
				Type:      itemTypeFunctionCall,
				CallID:    part.ToolCall.ID,
				Name:      part.ToolCall.Name,
				Arguments: part.ToolCall.Arguments,
			})
		case domain.PartToolResult:
			// 允许承载工具结果的角色在 encodeResponsesInput 里已拆分处理，
			// 走到这里说明是 assistant 角色，属无法编码的形态。
			return nil, domain.NewError(domain.CodeInternal,
				fmt.Sprintf("第 %d 条消息的工具结果必须用 tool 角色承载", index))
		default:
			return nil, domain.NewError(domain.CodeInternal,
				fmt.Sprintf("第 %d 条消息包含无法编码的片段类型 %q", index, string(part.Kind)))
		}
	}
	flush()
	if len(items) == 0 {
		return nil, domain.NewError(domain.CodeInternal, fmt.Sprintf("第 %d 条消息内容为空", index))
	}
	return items, nil
}

// splitResponsesToolResults 把一条消息的片段拆成工具结果单元与其余片段。
//
// 工具结果片段按原顺序逐个展开为独立的 function_call_output 条目，不合并；
// 其余片段按原相对顺序返回给调用方合成残留 message 条目。
// allowRest 为 false（tool 角色）时，非工具结果片段按内部错误处理，保持「tool 消息只承载工具结果」的既有约束。
// 工具结果必须非 nil 且带非空的 ToolCallID，且该 id 必须在此前的 assistant 工具调用里出现过。
// 片段为空时返回平台内部错误，保持此类消息此前被拒绝、不被静默丢弃的行为。
func splitResponsesToolResults(index int, parts []domain.Part, seenToolCalls map[string]struct{}, allowRest bool) ([]any, []domain.Part, error) {
	if len(parts) == 0 {
		return nil, nil, domain.NewError(domain.CodeInternal, fmt.Sprintf("第 %d 条消息内容为空", index))
	}
	units := make([]any, 0, len(parts))
	rest := make([]domain.Part, 0, len(parts))
	for j, part := range parts {
		if part.Kind != domain.PartToolResult {
			if !allowRest {
				return nil, nil, domain.NewError(domain.CodeInternal,
					fmt.Sprintf("第 %d 条 tool 消息包含非工具结果片段 %q", index, string(part.Kind)))
			}
			rest = append(rest, part)
			continue
		}
		if part.ToolResult == nil {
			return nil, nil, domain.NewError(domain.CodeInternal,
				fmt.Sprintf("第 %d 条消息第 %d 个片段缺少工具结果内容", index, j))
		}
		if part.ToolResult.ToolCallID == "" {
			return nil, nil, domain.NewError(domain.CodeInvalidRequest,
				fmt.Sprintf("第 %d 条消息第 %d 个工具结果缺少工具调用 id", index, j))
		}
		if _, ok := seenToolCalls[part.ToolResult.ToolCallID]; !ok {
			return nil, nil, domain.NewError(domain.CodeInvalidRequest,
				fmt.Sprintf("第 %d 条消息的工具结果找不到对应的工具调用 %q", index, part.ToolResult.ToolCallID))
		}
		units = append(units, upstreamFunctionCallOutputItem{
			Type:   itemTypeFunctionCallOutput,
			CallID: part.ToolResult.ToolCallID,
			Output: part.ToolResult.Content,
		})
	}
	return units, rest, nil
}

// addAssistantToolCallIDs 把 assistant 消息里的工具调用 id 加入已出现集合。
func addAssistantToolCallIDs(seen map[string]struct{}, message domain.Message) {
	for _, part := range message.Parts {
		if part.Kind == domain.PartToolCall && part.ToolCall != nil {
			seen[part.ToolCall.ID] = struct{}{}
		}
	}
}

func encodeResponsesTools(tools []domain.ToolSpec) []upstreamToolWire {
	if len(tools) == 0 {
		return nil
	}
	out := make([]upstreamToolWire, 0, len(tools))
	for _, tool := range tools {
		item := upstreamToolWire{Type: functionToolType, Name: tool.Name, Description: tool.Description}
		if trimmed := strings.TrimSpace(tool.ParametersJSON); trimmed != "" {
			item.Parameters = json.RawMessage(trimmed)
		}
		out = append(out, item)
	}
	return out
}

// encodeResponsesToolChoice 把内部工具选择编码为 Responses 的取值：
//   - 字符串形态只允许 auto/none/required
//   - 指定具体函数用对象形态 {type:function,name:…}
//
// 未指定时不携带该字段；无法映射时返错。
func encodeResponsesToolChoice(choice domain.ToolChoice) (json.RawMessage, error) {
	if value, ok := choice.ModeString(); ok {
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, domain.NewError(domain.CodeInternal, "编码 tool_choice 失败").WithCause(err)
		}
		return encoded, nil
	}
	switch choice.Mode {
	case domain.ToolChoiceUnset:
		return nil, nil
	case domain.ToolChoiceTool:
		name := strings.TrimSpace(choice.Name)
		if name == "" {
			return nil, domain.NewError(domain.CodeInternal, "指定工具时缺少工具名")
		}
		encoded, err := json.Marshal(upstreamToolChoiceFunctionWire{Type: functionToolType, Name: name})
		if err != nil {
			return nil, domain.NewError(domain.CodeInternal, "编码 tool_choice 失败").WithCause(err)
		}
		return encoded, nil
	default:
		return nil, domain.NewError(domain.CodeInternal, fmt.Sprintf("无法编码的工具选择模式 %q", string(choice.Mode)))
	}
}
