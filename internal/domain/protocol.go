package domain

import (
	"errors"
	"fmt"
	"strings"
)

// Protocol 标识一个对外或对上游的线协议。
type Protocol string

const (
	ProtocolOpenAIChat        Protocol = "openai_chat"        // POST /v1/chat/completions
	ProtocolOpenAIResponses   Protocol = "openai_responses"   // POST /v1/responses
	ProtocolAnthropicMessages Protocol = "anthropic_messages" // POST /v1/messages
	// ProtocolGemini 是 Google Generative Language 的 generateContent 协议。
	//
	// 它与上面三种协议的形状不同：模型名与动作（是否流式）都写在路径上
	// （/v1beta/models/{model}:generateContent 与 :streamGenerateContent），
	// 请求体里没有这两个字段，因此它的端点路径不是一条定长字面量。
	ProtocolGemini Protocol = "gemini"
)

// Valid 报告协议取值是否受支持。
func (p Protocol) Valid() bool {
	switch p {
	case ProtocolOpenAIChat, ProtocolOpenAIResponses, ProtocolAnthropicMessages, ProtocolGemini:
		return true
	default:
		return false
	}
}

// EndpointPath 返回该协议在客户端侧的端点路径：POST 到该路径的请求按本协议处理。
//
// 这些字面量只有这一处来源，装配层用它把请求路径映射为协议。
// 它不参与上游地址拼接：上游地址由配置的 url 显式写全，
// 因为不同供应商对「版本根 + 端点路径」的拼法并不一致。
//
// 除 Gemini 外，取值恒等于 "/v1" 与 EndpointSegment() 的拼接，两条事实同源，由测试守住这层关系。
// Gemini 是例外：它的模型名写在路径上，返回值是一份带 {model} 占位符的模板，
// 既不是可直接比较的字面量、也不以 /v1 开头（它的版本根是 /v1beta）。
// 装配层因此不能只用等值比较识别它，Gemini 的路径识别由适配器自己的路径解析承担。
func (p Protocol) EndpointPath() string {
	switch p {
	case ProtocolOpenAIChat:
		return "/v1/chat/completions"
	case ProtocolOpenAIResponses:
		return "/v1/responses"
	case ProtocolAnthropicMessages:
		return "/v1/messages"
	case ProtocolGemini:
		return "/v1beta/models/{model}:generateContent"
	default:
		return ""
	}
}

// EndpointSegment 返回该协议端点路径里去掉版本根之后的那一段。
//
// 客户端侧的端点路径形如 /v1/chat/completions，但「版本根 + 端点段」怎么拼由上游供应商自定：
// OpenAI 系把版本写成 /v1，智谱写成 /api/paas/v4 与 /api/coding/paas/v4，
// 火山方舟写成 /api/v3，百度千帆写成 /v2。因此校验上游 url 时只能比对端点段：
// 它既拦得住「漏写了端点路径」，又不会把版本根与 OpenAI 不一致的供应商一并拒掉。
func (p Protocol) EndpointSegment() string {
	segments := p.EndpointSegments()
	if len(segments) == 0 {
		return ""
	}
	return segments[0]
}

// EndpointSegments 返回该协议在地址末段上的全部可识别写法，首项与 EndpointSegment() 相同。
//
// 多数协议只有一种写法；Gemini 有两种：非流式与流式的动作段不同
// （:generateContent 与 :streamGenerateContent），而两者是同一个端点协议。
// 之所以列全部而不是只列一种：配置里的端点地址写哪种动作都应该被认出来，
// 实际发往上游的动作由适配器按本次请求是否流式改写（见 domain.UpstreamURLBuilder）。
func (p Protocol) EndpointSegments() []string {
	switch p {
	case ProtocolOpenAIChat:
		return []string{"/chat/completions"}
	case ProtocolOpenAIResponses:
		return []string{"/responses"}
	case ProtocolAnthropicMessages:
		return []string{"/messages"}
	case ProtocolGemini:
		return []string{":generateContent", ":streamGenerateContent"}
	default:
		return nil
	}
}

// ProtocolForEndpointPath 由上游地址的路径推导它属于哪种协议。
//
// 判据是路径末尾是否等于某个协议的端点段（见 EndpointSegments），而不是完整端点路径：
// 各供应商把版本根写在路径的哪一段并不一致，能确定协议的只有末尾那一段。
// 只有唯一命中才算推导成功：命中不了时调用方要报「地址写错了」，命中多个时同样返回 false，
// 因为随手挑一个会把「两个协议都说得通」的配置静默解释成其中一个。
func ProtocolForEndpointPath(path string) (Protocol, bool) {
	matched := Protocol("")
	for _, protocol := range []Protocol{
		ProtocolOpenAIChat,
		ProtocolOpenAIResponses,
		ProtocolAnthropicMessages,
		ProtocolGemini,
	} {
		hit := false
		for _, segment := range protocol.EndpointSegments() {
			if strings.HasSuffix(path, segment) {
				hit = true
				break
			}
		}
		if !hit {
			continue
		}
		if matched != "" {
			return "", false
		}
		matched = protocol
	}
	return matched, matched != ""
}

// CrossProtocolRebuildable 报告本协议能否与其它协议互相重建请求与响应。
//
// 四种会话式协议在内部统一协议里有完整字段可以互相表达，选路因此允许把一个协议的
// 请求交给另一个协议的渠道，由适配器重建。
func (p Protocol) CrossProtocolRebuildable() bool {
	switch p {
	case ProtocolOpenAIChat, ProtocolOpenAIResponses, ProtocolAnthropicMessages, ProtocolGemini:
		return true
	default:
		return false
	}
}

// FilterRoutesForProtocol 返回本次请求协议下可用的候选渠道。
//
// 请求协议与渠道协议一致时一律保留；不一致时只有两侧协议都可跨协议重建才保留。
// 返回的切片是新分配的结果，不修改入参，调用方可以安全地继续使用原切片。
//
// 本函数会丢掉跨协议的候选，丢掉它们就等于关掉跨协议重建这条路；只想改变尝试顺序、
// 保留跨协议候选作为后备时用 PreferRoutesForProtocol。
func FilterRoutesForProtocol(protocol Protocol, routes []Route) []Route {
	filtered := make([]Route, 0, len(routes))
	for _, route := range routes {
		if route.Protocol == protocol {
			filtered = append(filtered, route)
			continue
		}
		if protocol.CrossProtocolRebuildable() && route.Protocol.CrossProtocolRebuildable() {
			filtered = append(filtered, route)
		}
	}
	return filtered
}

// PreferRoutesForProtocol 把与本次请求协议一致的候选排到前面，其余候选按原顺序留在后面。
//
// 它只调顺序，一条候选都不丢：同协议的候选排在前面，请求因此按原协议的字段原样上发，
// 省掉跨协议重建；跨协议的候选仍留在链尾，前面那些都失败且可重试时才轮到它们。
// 这正是它与 FilterRoutesForProtocol 的差别——后者按协议过滤掉跨协议的候选，
// 客户端发 anthropic_messages 而配置里只有 openai_chat 端点时就会一条候选都不剩。
//
// 排序是稳定划分：同协议的那一组与其余的那一组各自保持原有的相对顺序，
// 因此候选表里写下的回退顺序（谁先谁后）不被搅乱，被打乱的只是「谁在最前」。
// 返回的切片是新分配的结果，不修改也不共享入参的底层数组；空表、单条候选、
// 全部同协议三种输入都原样保持顺序。
func PreferRoutesForProtocol(protocol Protocol, routes []Route) []Route {
	preferred := make([]Route, 0, len(routes))
	fallback := make([]Route, 0, len(routes))
	for _, route := range routes {
		if route.Protocol == protocol {
			preferred = append(preferred, route)
			continue
		}
		fallback = append(fallback, route)
	}
	// preferred 是本次调用新分配的切片，这里把回退候选接在它的空位上不会碰到入参。
	return append(preferred, fallback...)
}

// Role 是消息角色，已归一化到各协议的交集。
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// PartKind 标识消息片段类型。
type PartKind string

const (
	PartText       PartKind = "text"
	PartImage      PartKind = "image"
	PartToolCall   PartKind = "tool_call"
	PartToolResult PartKind = "tool_result"
	// PartReasoning 是模型的推理内容片段（完整响应方向），例如 Anthropic Messages 的
	// thinking 块、部分 OpenAI Chat 兼容渠道的 reasoning_content。文本复用 Text 字段，
	// 不新增字段：Kind 决定哪些字段有效的既有约定可以表达它。
	//
	// 它只在解码方向建模：编码方向（EncodeResponse 与跨协议请求重建）不下发推理内容，
	// 因为各协议对可回传形态的要求不同——Anthropic 的 thinking 块要求签名、
	// OpenAI Responses 的 reasoning 条目要求上下文标识，从别的协议取到的纯文本无法重建。
	PartReasoning PartKind = "reasoning"
)

// ToolCall 是一次工具调用。Arguments 保留上游返回的原始 JSON 字符串，不在此处解析。
//
// Index 是同一响应内工具调用的下标，用于正确拼接流式增量帧
// （OpenAI 的并行工具调用会在同一帧里给出多个调用）。不支持下标的协议固定填 0。
type ToolCall struct {
	Index     int    `json:"index"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolResult 是对一次工具调用的结果回填。
type ToolResult struct {
	ToolCallID string `json:"tool_call_id"`
	Content    string `json:"content"`
}

// Part 是消息的一个片段。Kind 决定哪些字段有效；其余字段必须为零值。
type Part struct {
	Kind       PartKind    `json:"kind"`
	Text       string      `json:"text,omitempty"`
	ImageURL   string      `json:"image_url,omitempty"`
	ToolCall   *ToolCall   `json:"tool_call,omitempty"`
	ToolResult *ToolResult `json:"tool_result,omitempty"`
}

// Message 是一条归一化消息。
type Message struct {
	Role  Role   `json:"role"`
	Parts []Part `json:"parts"`
}

// ToolSpec 是一个可调用工具的定义。ParametersJSON 保留 JSON Schema 原文。
type ToolSpec struct {
	Name           string `json:"name"`
	Description    string `json:"description,omitempty"`
	ParametersJSON string `json:"parameters_json,omitempty"`
}

// ToolChoiceMode 是归一化后的工具选择模式。
//
// 各协议的工具选择字面量与对象形态各不相同，
// 映射到本枚举由各适配器负责：`auto`、`none`、`required` 三种字符串取值在各协议间
// 通用，指定具体工具在各协议上是对象形态。
type ToolChoiceMode string

const (
	// ToolChoiceUnset 是零值：不指定工具选择，是否调用工具由上游按默认策略决定。
	ToolChoiceUnset ToolChoiceMode = ""
	// ToolChoiceAuto 表示由模型自行决定是否调用工具。
	ToolChoiceAuto ToolChoiceMode = "auto"
	// ToolChoiceNone 表示禁止调用任何工具。
	ToolChoiceNone ToolChoiceMode = "none"
	// ToolChoiceRequired 表示必须调用至少一个工具。
	ToolChoiceRequired ToolChoiceMode = "required"
	// ToolChoiceTool 表示必须调用 ToolChoice.Name 指定的那个工具。
	ToolChoiceTool ToolChoiceMode = "tool"
)

// ToolChoice 是归一化后的工具选择。Name 仅在 Mode 为 ToolChoiceTool 时有效。
type ToolChoice struct {
	Mode ToolChoiceMode `json:"mode"`
	// Name 是要求模型调用的工具名，仅在 Mode 为 ToolChoiceTool 时非空。
	Name string `json:"name,omitempty"`
}

// ParseToolChoiceMode 把协议字符串形态的工具选择（`auto` / `none` / `required`）
// 解析为内部模式。其余取值（含空串）返回错误，调用方必须按 `CodeInvalidRequest`
// 拒绝，不得静默丢弃客户端的工具选择要求。
func ParseToolChoiceMode(value string) (ToolChoiceMode, error) {
	trimmed := ToolChoiceMode(strings.TrimSpace(value))
	switch trimmed {
	case ToolChoiceAuto, ToolChoiceNone, ToolChoiceRequired:
		return trimmed, nil
	default:
		return ToolChoiceUnset, fmt.Errorf("工具选择字符串取值 %q 不受支持", value)
	}
}

// ModeString 返回字符串形态的协议取值，第二个返回值报告 Mode 是否为字符串形态。
// 指定工具与不指定都返回 false，由各适配器按自身协议编码。
func (t ToolChoice) ModeString() (string, bool) {
	switch t.Mode {
	case ToolChoiceAuto, ToolChoiceNone, ToolChoiceRequired:
		return string(t.Mode), true
	default:
		return "", false
	}
}

// validate 校验工具选择的内部一致性：模式必须已登记，指定工具时必须有工具名。
func (t ToolChoice) validate() error {
	switch t.Mode {
	case ToolChoiceUnset, ToolChoiceAuto, ToolChoiceNone, ToolChoiceRequired:
		return nil
	case ToolChoiceTool:
		if strings.TrimSpace(t.Name) == "" {
			return errors.New("指定工具时必须给出工具名")
		}
		return nil
	default:
		return fmt.Errorf("未知工具选择模式 %q", string(t.Mode))
	}
}

// Request 是归一化后的内部请求，三种对外协议都转换为此结构。
type Request struct {
	RequestID   string     `json:"request_id"`
	Protocol    Protocol   `json:"protocol"`
	Model       string     `json:"model"`
	Messages    []Message  `json:"messages"`
	Tools       []ToolSpec `json:"tools,omitempty"`
	ToolChoice  ToolChoice `json:"tool_choice,omitempty"`
	MaxTokens   int        `json:"max_tokens,omitempty"`
	Temperature *float64   `json:"temperature,omitempty"`
	Stream      bool       `json:"stream"`
	// RawBody 保留客户端原始请求体，供协议回退时按原协议重放。
	RawBody []byte `json:"-"`
}

// Validate 校验内部请求的最小必要条件。它不检查模型是否存在，后者由选路负责。
func (r *Request) Validate() error {
	if r == nil {
		return NewError(CodeInvalidRequest, "请求为空")
	}
	if r.RequestID == "" {
		return NewError(CodeInvalidRequest, "缺少 request_id")
	}
	if !r.Protocol.Valid() {
		return NewError(CodeInvalidRequest, fmt.Sprintf("不支持的协议 %q", string(r.Protocol)))
	}
	if r.Model == "" {
		return NewError(CodeInvalidRequest, "缺少 model")
	}
	if r.MaxTokens < 0 {
		return NewError(CodeInvalidRequest, "max_tokens 不得为负")
	}
	if r.Temperature != nil && (*r.Temperature < 0 || *r.Temperature > 2) {
		return NewError(CodeInvalidRequest, "temperature 必须在 [0, 2] 区间")
	}
	if err := r.ToolChoice.validate(); err != nil {
		return NewError(CodeInvalidRequest, fmt.Sprintf("tool_choice 非法：%s", err))
	}
	if len(r.Messages) == 0 {
		return NewError(CodeInvalidRequest, "缺少 messages")
	}
	for i, m := range r.Messages {
		if len(m.Parts) == 0 {
			return NewError(CodeInvalidRequest, fmt.Sprintf("第 %d 条消息内容为空", i))
		}
		for j, p := range m.Parts {
			if err := p.validate(); err != nil {
				return NewError(CodeInvalidRequest, fmt.Sprintf("第 %d 条消息第 %d 个片段：%s", i, j, err))
			}
		}
	}
	return nil
}

// validate 校验片段的必填字段。Kind 决定哪些字段有效；缺少必填字段的片段
// 会在转发时被上游拒绝，因此提前在解码后拦截。
func (p Part) validate() error {
	switch p.Kind {
	case PartText:
		if p.Text == "" {
			return errors.New("文本片段内容为空")
		}
	case PartImage:
		if p.ImageURL == "" {
			return errors.New("图片片段缺少 image_url")
		}
	case PartToolCall:
		if p.ToolCall == nil || p.ToolCall.ID == "" || p.ToolCall.Name == "" {
			return errors.New("工具调用片段缺少 id 或 name")
		}
	case PartToolResult:
		if p.ToolResult == nil || p.ToolResult.ToolCallID == "" {
			return errors.New("工具结果片段缺少 tool_call_id")
		}
	case PartReasoning:
		// 取不到文本的推理块（例如只有密文载荷的 redacted_thinking）在解码时跳过，避免产出空片段。
		if p.Text == "" {
			return errors.New("推理片段内容为空")
		}
	default:
		return fmt.Errorf("未知片段类型 %q", string(p.Kind))
	}
	return nil
}

// FinishReason 是归一化后的结束原因，取各协议语义的交集。
//
// 各上游的具体字面量（如 `end_turn`、`max_output_tokens`）由各自的适配器
// 负责映射到本枚举：字面量属于协议差异，放在这里会迫使新增协议修改本包。
type FinishReason string

const (
	FinishStop          FinishReason = "stop"           // 正常结束
	FinishLength        FinishReason = "length"         // 达到长度上限
	FinishToolCalls     FinishReason = "tool_calls"     // 请求调用工具
	FinishContentFilter FinishReason = "content_filter" // 被内容策略拦截
	FinishUnknown       FinishReason = "unknown"        // 上游给出了无法识别的取值
)

// Response 是归一化后的完整响应（非流式）。
type Response struct {
	Model             string       `json:"model"`
	Message           Message      `json:"message"`
	FinishReason      FinishReason `json:"finish_reason"`
	Usage             Usage        `json:"usage"`
	UpstreamRequestID string       `json:"upstream_request_id,omitempty"`
}

// ChunkKind 标识流式分片类型。
type ChunkKind string

const (
	ChunkTextDelta     ChunkKind = "text_delta"
	ChunkToolCallDelta ChunkKind = "tool_call_delta"
	// ChunkReasoningDelta 是推理内容的流式增量，复用 TextDelta 承载文本。
	//
	// 与 PartReasoning 一样只在解码方向产出：编码方向不下发推理内容，重建下沉目标
	// 对推理分片只记账、不编码。
	ChunkReasoningDelta ChunkKind = "reasoning_delta"
	ChunkUsage          ChunkKind = "usage"
	ChunkFinish         ChunkKind = "finish"
	// ChunkStreamEnd 表示一次流式响应结束，携带用量与结束原因。
	//
	// 它与协议自身的结束信号一一对应，而不是与单一的终止帧一一对应：
	//
	//   - OpenAI Chat：choices[0].finish_reason（必有）与 [DONE]（可选的哨兵帧）；
	//   - OpenAI Responses：response.completed 与 response.incomplete；
	//   - Anthropic Messages：message_stop。
	//
	// 解码侧每条正常结束的流恰好产出一个结束分片，被截断的流产出 0 个；
	// 该有无是调用方判定「上游是否在结束信号出现前断开」的唯一信号。
	// OpenAI Chat 缺失真实终止帧时，结束分片由 Adapter.FinishStream 在 EOF 处补齐。
	//
	// 解码侧产出的结束分片必须携带非 nil 的用量对象：未取得用量时置零值，
	// 其 Source 为 UsageSourceUnknown。留空会让调用方无法区分「未取得用量」与「本分片不携带用量」。
	// 编码侧（EncodeStreamEnd）仍接受不带用量的结束分片。
	ChunkStreamEnd ChunkKind = "stream_end"
)

// Chunk 是一个归一化流式分片。Kind 决定哪些字段有效。
type Chunk struct {
	Kind ChunkKind `json:"kind"`
	// Model 是对外模型别名，供流式帧回填模型名；为空时适配器可省略该字段。
	Model        string       `json:"model,omitempty"`
	TextDelta    string       `json:"text_delta,omitempty"`
	ToolCall     *ToolCall    `json:"tool_call,omitempty"`
	Usage        *Usage       `json:"usage,omitempty"`
	FinishReason FinishReason `json:"finish_reason,omitempty"`
}
