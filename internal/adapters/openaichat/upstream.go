package openaichat

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

// 请求体重建与原始报文改写涉及的顶层字段名。
const (
	modelField               = "model"
	maxTokensField           = "max_tokens"
	maxCompletionTokensField = "max_completion_tokens"
)

// upstreamRequestWire 是重建上游请求体时使用的线上结构。
//
// 与解码用的 requestWire 分开：解码结构允许宽进，重建结构要精确控制字段出现与否。
type upstreamRequestWire struct {
	Model      string                `json:"model"`
	Messages   []upstreamMessageWire `json:"messages"`
	Tools      []upstreamToolWire    `json:"tools,omitempty"`
	ToolChoice json.RawMessage       `json:"tool_choice,omitempty"`
	MaxTokens  *int                  `json:"max_tokens,omitempty"`
	// MaxCompletionTokens 是 max_tokens 的替代字段，新模型只认它。
	MaxCompletionTokens *int     `json:"max_completion_tokens,omitempty"`
	Temperature         *float64 `json:"temperature,omitempty"`
	Stream              bool     `json:"stream"`
}

type upstreamMessageWire struct {
	Role       string                `json:"role"`
	Content    any                   `json:"content"`
	ToolCalls  []requestToolCallWire `json:"tool_calls,omitempty"`
	ToolCallID string                `json:"tool_call_id,omitempty"`
}

// upstreamMessageUnit 是逐条编码与合并之间的中间结果：线上消息、
// 参与合并的内容片段与该条的**原始**消息下标。内容片段单独留存，是因为合并后的 content 必须按
// 「把参与合并各条的片段拼成一个列表再整体编码」的规则重算，
// 而不是把已编码的 content 值拼接——后者会把一条消息内的多个文本片段压成一段。
type upstreamMessageUnit struct {
	message      upstreamMessageWire
	contentParts []domain.Part
	index        int
}

type upstreamToolWire struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"function"`
}

// toolChoiceFunctionWire 是 Chat Completions 指定具体工具的对象形态：
// {"type":"function","function":{"name":"…"}}。
type toolChoiceFunctionWire struct {
	Type     string `json:"type"`
	Function struct {
		Name string `json:"name"`
	} `json:"function"`
}

// EncodeRequest 按内部统一格式重建 Chat Completions 请求体，用于跨协议转发。
//
// 只重建内部协议已建模的字段；未建模字段（top_p、stop 等）在同协议透传路径上由
// RewriteRawBody 保留，跨协议时按设计丢失。
//
// 第二个返回值报告本次重建相对客户端请求实际改过的改写项：模型名被替换或补齐记request_model，
// 输出上限被钳制或补齐记 request_output_limit。
func (a *Adapter) EncodeRequest(req *domain.Request, options domain.RewriteOptions) ([]byte, domain.RewriteParts, error) {
	if req == nil {
		return nil, nil, domain.NewError(domain.CodeInternal, "请求为空")
	}
	messages, err := encodeUpstreamMessages(req.Messages)
	if err != nil {
		return nil, nil, err
	}
	toolChoice, err := encodeUpstreamToolChoice(req.ToolChoice)
	if err != nil {
		return nil, nil, err
	}
	wire := upstreamRequestWire{
		Model:       domain.UpstreamModelName(req.Model, options),
		Messages:    messages,
		Tools:       encodeUpstreamTools(req.Tools),
		ToolChoice:  toolChoice,
		Temperature: req.Temperature,
		Stream:      req.Stream,
	}
	var parts domain.RewriteParts
	if wire.Model != req.Model {
		parts = append(parts, domain.RewritePartRequestModel)
	}
	if limit := domain.OutputLimit(req.MaxTokens, options.MaxOutputTokens); limit > 0 {
		// 客户端未声明上限时补替代字段：新模型只认 max_completion_tokens，
		// 补已废弃的 max_tokens 会被上游拒绝。
		wire.MaxCompletionTokens = &limit
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
// 没有任何改写项启用时逐字节返回入参；启用改写时只改动以下两处字段，其余字段取值保持原样：
//   - model：替换或补齐上游模型名
//   - max_tokens / max_completion_tokens：按统一口径处理输出上限
//     （客户端未声明时补 max_completion_tokens，已存在时按 max_tokens 优先钳制）
//
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
		domain.SetRawOutputLimit(fields, []string{maxTokensField, maxCompletionTokensField}, maxCompletionTokensField, options.MaxOutputTokens) {
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

// UpstreamHeaders 返回调用上游 Chat Completions 时应携带的请求头。
//
// 本协议没有内置的必需请求头（凭据由流水线按 Route.CredentialRef 注入），
// 因此只回传渠道配置；nil 入参返回非 nil 的空 map，保证调用方可以直接写入。
func (a *Adapter) UpstreamHeaders(channelHeaders http.Header) http.Header {
	return domain.CloneHeader(channelHeaders)
}

// EncodeStreamError 报告本协议不支持流式错误帧。
//
// Chat Completions 的 SSE 流只有 data: 帧、没有独立的错误事件；data: {"error":{...}}
// 是部分兼容上游的非规范形态，网关解码时宽容接受，但面向客户端不发明该形态。
// 因此返回 supported=false，调用方必须降级（结束后关闭连接，或改用 EncodeError）。
func (a *Adapter) EncodeStreamError(err error) ([]byte, bool) {
	return nil, false
}

// encodeUpstreamMessages 把内部消息编码为 Chat Completions 的 messages 数组。
//
// 工具结果在内部可能挂在 user 角色的消息上（Anthropic 形态）或 tool 角色的消息上（OpenAI 形态），
// 本协议用独立的 tool 消息承载：一条消息里的工具结果片段按片段顺序逐个展开、不合并，
// 全部排在残留消息之前；其余片段为空时不产出残留消息。
// 工具结果无法对应到此前已出现的 assistant 工具调用时报 invalid_request。
//
// 逐条编码之后对结果做一趟相邻同角色合并：源协议把每次函数调用表达为独立条目，逐条编码会把
// 「一次 assistant 回合里的多个并行调用」错译成多条各带一个调用的相邻 assistant 消息，
// 而 Chat 要求每条 tool 消息紧跟声明该次调用的 assistant 消息，错译后的序列必然违反该要求。
// 先逐条编码、后合并是刻意的顺序：逐条消息的校验、错误码与错误文案（含原始下标）因此逐字不变。
func encodeUpstreamMessages(messages []domain.Message) ([]upstreamMessageWire, error) {
	units := make([]upstreamMessageUnit, 0, len(messages))
	// 此前已出现的 assistant 工具调用 id；工具结果必须能找到对应的前置调用。
	seenToolCalls := make(map[string]struct{})
	for i, message := range messages {
		switch message.Role {
		case domain.RoleSystem, domain.RoleAssistant:
			encoded, parts, err := encodeUpstreamMessage(i, message)
			if err != nil {
				return nil, err
			}
			units = append(units, upstreamMessageUnit{message: encoded, contentParts: parts, index: i})
			addAssistantToolCallIDs(seenToolCalls, message)
		case domain.RoleUser:
			toolResults, rest, err := splitUpstreamToolResults(i, message.Parts, seenToolCalls, true)
			if err != nil {
				return nil, err
			}
			units = appendToolResultUnits(units, toolResults, i)
			if len(rest) > 0 {
				encoded, parts, err := encodeUpstreamMessage(i, domain.Message{Role: domain.RoleUser, Parts: rest})
				if err != nil {
					return nil, err
				}
				units = append(units, upstreamMessageUnit{message: encoded, contentParts: parts, index: i})
			}
		case domain.RoleTool:
			toolResults, _, err := splitUpstreamToolResults(i, message.Parts, seenToolCalls, false)
			if err != nil {
				return nil, err
			}
			units = appendToolResultUnits(units, toolResults, i)
		default:
			return nil, domain.NewError(domain.CodeInternal,
				fmt.Sprintf("第 %d 条消息角色 %q 无法编码为 OpenAI Chat 请求", i, string(message.Role)))
		}
	}
	return mergeUpstreamUnits(units)
}

// appendToolResultUnits 把展开出的 tool 消息单元追加到合并序列。
func appendToolResultUnits(units []upstreamMessageUnit, toolResults []upstreamMessageWire, index int) []upstreamMessageUnit {
	for _, toolResult := range toolResults {
		units = append(units, upstreamMessageUnit{message: toolResult, index: index})
	}
	return units
}

// mergeUpstreamUnits 对逐条编码结果做一趟相邻同角色合并，并重算合并后的 content。
//
// 只合并 user 与 assistant：Chat 的 tool 消息用单个 tool_call_id 指出它应答哪次调用，
// 一条 tool 消息无法表达多次调用
// （这与 Anthropic 的 tool_result 块可多条并列于一条user 消息正好相反）；system 若参与合并，
// 既不修任何翻译错误、又改变发给上游的报文形态，故不参与。因此 tool 与 system 直接进入结果、
// 充当合并的边界，合并不跨越任何tool 消息搬动消息。合并只改消息条数与字段归属，
// 不改写 tool_calls 与 tool 消息的字段。
func mergeUpstreamUnits(units []upstreamMessageUnit) ([]upstreamMessageWire, error) {
	merged := make([]upstreamMessageUnit, 0, len(units))
	for _, unit := range units {
		if n := len(merged); n > 0 && mergeableUpstreamRole(merged[n-1].message.Role) && merged[n-1].message.Role == unit.message.Role {
			previous := &merged[n-1]
			previous.contentParts = append(previous.contentParts, unit.contentParts...)
			previous.message.ToolCalls = append(previous.message.ToolCalls, unit.message.ToolCalls...)
			continue
		}
		merged = append(merged, unit)
	}
	out := make([]upstreamMessageWire, 0, len(merged))
	for _, unit := range merged {
		message := unit.message
		if message.Role != string(domain.RoleTool) {
			// tool 消息的内容是工具结果字符串，不是内部片段；其余角色按合并后的片段整体重算。
			// 逐条编码阶段已对同一批片段做过校验，这里的重算不引入新的错误分支。
			content, err := encodeUpstreamContent(unit.index, unit.contentParts, message.Role == string(domain.RoleAssistant))
			if err != nil {
				return nil, err
			}
			message.Content = content
		}
		out = append(out, message)
	}
	return out, nil
}

// mergeableUpstreamRole 报告一个已编码角色是否参与相邻同角色合并。
func mergeableUpstreamRole(role string) bool {
	return role == string(domain.RoleUser) || role == string(domain.RoleAssistant)
}

// splitUpstreamToolResults 把一条消息的片段拆成工具结果单元与其余片段。
//
// 工具结果片段按原顺序逐个展开为独立的 tool 消息，不合并；其余片段按原相对顺序
// 返回给调用方合成残留消息。allowRest 为 false（tool 角色）时，非工具结果片段按
// 内部错误处理，保持「tool 消息只承载工具结果」的既有约束。工具结果必须非 nil 且
// 带非空的 ToolCallID，且该 id 必须在此前的 assistant 工具调用里出现过，否则报错。
func splitUpstreamToolResults(index int, parts []domain.Part, seenToolCalls map[string]struct{}, allowRest bool) ([]upstreamMessageWire, []domain.Part, error) {
	units := make([]upstreamMessageWire, 0, len(parts))
	rest := make([]domain.Part, 0, len(parts))
	for j, part := range parts {
		if part.Kind != domain.PartToolResult {
			if !allowRest {
				return nil, nil, domain.NewError(domain.CodeInternal,
					fmt.Sprintf("第 %d 条消息第 %d 个片段不是工具结果", index, j))
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
		units = append(units, upstreamMessageWire{
			Role:       string(domain.RoleTool),
			Content:    part.ToolResult.Content,
			ToolCallID: part.ToolResult.ToolCallID,
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

func encodeUpstreamMessage(index int, message domain.Message) (upstreamMessageWire, []domain.Part, error) {
	contentParts := make([]domain.Part, 0, len(message.Parts))
	toolCalls := make([]requestToolCallWire, 0)
	for _, part := range message.Parts {
		switch part.Kind {
		case domain.PartToolCall:
			if message.Role != domain.RoleAssistant {
				return upstreamMessageWire{}, nil, domain.NewError(domain.CodeInternal,
					fmt.Sprintf("第 %d 条消息不是 assistant，不能携带工具调用", index))
			}
			if part.ToolCall == nil {
				return upstreamMessageWire{}, nil, domain.NewError(domain.CodeInternal,
					fmt.Sprintf("第 %d 条消息的工具调用片段缺少内容", index))
			}
			call := requestToolCallWire{ID: part.ToolCall.ID, Type: toolTypeFunction}
			call.Function.Name = part.ToolCall.Name
			call.Function.Arguments = part.ToolCall.Arguments
			toolCalls = append(toolCalls, call)
		case domain.PartToolResult:
			// 允许承载工具结果的角色在 encodeUpstreamMessages 里已拆分处理，
			// 走到这里说明是 assistant / system 角色，属无法编码的形态。
			return upstreamMessageWire{}, nil, domain.NewError(domain.CodeInternal,
				fmt.Sprintf("第 %d 条消息的工具结果必须用 tool 角色承载", index))
		default:
			if message.Role == domain.RoleAssistant && part.Kind != domain.PartText {
				return upstreamMessageWire{}, nil, domain.NewError(domain.CodeInternal,
					fmt.Sprintf("第 %d 条 assistant 消息包含无法编码的片段类型 %q", index, string(part.Kind)))
			}
			contentParts = append(contentParts, part)
		}
	}
	content, err := encodeUpstreamContent(index, contentParts, message.Role == domain.RoleAssistant)
	if err != nil {
		return upstreamMessageWire{}, nil, err
	}
	return upstreamMessageWire{Role: string(message.Role), Content: content, ToolCalls: toolCalls}, contentParts, nil
}

// encodeUpstreamContent 把片段编码为 content：纯文本合成为字符串，含图片时用数组。
// allowEmpty 为 true（assistant 消息）时允许内容为空，以便只带工具调用的消息编码为 null。
func encodeUpstreamContent(index int, parts []domain.Part, allowEmpty bool) (any, error) {
	if len(parts) == 0 {
		if allowEmpty {
			return nil, nil
		}
		return nil, domain.NewError(domain.CodeInternal, fmt.Sprintf("第 %d 条消息内容为空", index))
	}
	onlyText := true
	for _, part := range parts {
		if part.Kind != domain.PartText {
			onlyText = false
			break
		}
	}
	if onlyText {
		var text strings.Builder
		for _, part := range parts {
			text.WriteString(part.Text)
		}
		return text.String(), nil
	}
	items := make([]any, 0, len(parts))
	for _, part := range parts {
		switch part.Kind {
		case domain.PartText:
			items = append(items, map[string]any{"type": contentPartText, "text": part.Text})
		case domain.PartImage:
			if part.ImageURL == "" {
				return nil, domain.NewError(domain.CodeInternal,
					fmt.Sprintf("第 %d 条消息的图片片段缺少 image_url", index))
			}
			items = append(items, map[string]any{
				"type":      contentPartImageURL,
				"image_url": map[string]string{"url": part.ImageURL},
			})
		default:
			return nil, domain.NewError(domain.CodeInternal,
				fmt.Sprintf("第 %d 条消息包含无法编码的片段类型 %q", index, string(part.Kind)))
		}
	}
	return items, nil
}

func encodeUpstreamTools(tools []domain.ToolSpec) []upstreamToolWire {
	if len(tools) == 0 {
		return nil
	}
	out := make([]upstreamToolWire, 0, len(tools))
	for _, tool := range tools {
		item := upstreamToolWire{Type: toolTypeFunction}
		item.Function.Name = tool.Name
		item.Function.Description = tool.Description
		if trimmed := strings.TrimSpace(tool.ParametersJSON); trimmed != "" {
			item.Function.Parameters = json.RawMessage(trimmed)
		}
		out = append(out, item)
	}
	return out
}

// encodeUpstreamToolChoice 把内部工具选择编码为 Chat Completions 的取值：
//   - 字符串形态只允许 auto/none/required
//   - 指定具体工具必须用对象形态 {type:function,function:{name:…}}
//
// 未指定时不携带该字段；无法映射时返错。
func encodeUpstreamToolChoice(choice domain.ToolChoice) (json.RawMessage, error) {
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
		wire := toolChoiceFunctionWire{Type: toolTypeFunction}
		wire.Function.Name = name
		encoded, err := json.Marshal(wire)
		if err != nil {
			return nil, domain.NewError(domain.CodeInternal, "编码 tool_choice 失败").WithCause(err)
		}
		return encoded, nil
	default:
		return nil, domain.NewError(domain.CodeInternal, fmt.Sprintf("无法编码的工具选择模式 %q", string(choice.Mode)))
	}
}
