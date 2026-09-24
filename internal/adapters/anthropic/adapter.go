// Package anthropic 实现 Anthropic Messages 协议（POST /v1/messages）与
// internal/domain 统一内部格式之间的双向转换。
//
// 协议差异只能在本包内表达，流水线与计费代码只依赖 domain.Adapter。
// 本包只依赖标准库与 internal/domain。
//
// 关于角色归一化：Anthropic 的 messages 只有 user/assistant 两种角色，工具结果
// 以 tool_result 内容块的形式放在 user 消息里。本适配器保留该角色，用
// domain.PartToolResult 承载语义，domain.RoleTool 则留给以独立消息表达工具结果的协议。
//
// 关于 RequestID：Anthropic 请求体没有 request_id 字段，故 DecodeRequest 返回的
// Request.RequestID 为空，由调用方注入后再调用 Request.Validate。
package anthropic

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/sumwai/nova/internal/domain"
)

// 内容块的 wire 类型名。
const (
	blockTypeText       = "text"
	blockTypeImage      = "image"
	blockTypeToolUse    = "tool_use"
	blockTypeToolResult = "tool_result"
	// blockTypeThinking 是推理内容块：只在响应方向建模，请求方向与redacted_thinking 一样跳过。
	blockTypeThinking = "thinking"
)

// tool_choice 对象形态的 type 取值。本协议用 any 表示「必须调用至少一个工具」。
const (
	toolChoiceTypeAuto = "auto"
	toolChoiceTypeAny  = "any"
	toolChoiceTypeNone = "none"
	toolChoiceTypeTool = "tool"
)

// 流式内容块的内部标记，用于决定何时关闭当前块。
const (
	openKindText    = "text"
	openKindToolUse = "tool_use"
)

const (
	contentTypeJSON = "application/json"
	contentTypeSSE  = "text/event-stream"
)

// messageType 是非流式响应体的 type 取值，也是流式 message_start 内层 message 的 type。
//
// 它同时充当解码侧的判据：只有 type 为该值才说明这是一条 Messages 响应，
// 上游的错误信封或别的协议的报文据此被挡在外面。
const messageType = "message"

// 错误与流式事件的公共字面量。
const (
	// eventError 既是错误响应体的 type，也是 SSE 错误事件的名称。
	eventError = "error"
	// eventContentBlockDelta 是内容块增量事件的名称，SSE 事件名与 data.type 都取该值。
	eventContentBlockDelta = "content_block_delta"
	// deltaTypeInputJSON 是工具参数增量的 delta.type，其 partial_json 承载参数分片。
	deltaTypeInputJSON = "input_json_delta"
	// jsonNullLiteral 是 JSON null 的文本表示，用于判断可选字段是否显式给出。
	jsonNullLiteral = "null"
)

// Adapter 实现 domain.Adapter。
//
// 各非流式编解码与错误编码方法均为无状态实现。
// 流式编码与流式解码都需要跨帧累计状态（内容块下标、是否已发送 message_start、工具参数与用量），
// 因此一次流必须使用一个独立实例，不同并发流不得共享实例。NewStream 用于派生这样一个实例。
//
// 生命周期：一条并发流一个实例，实例不得跨流复用。编码状态在 EncodeStreamEnd 后重置，
// 解码状态在收到 message_start 时重置，故同一实例可顺序用于下一轮流，
// 但绝不能被两条并发流同时持有。内部用互斥锁防止方法被并发误用造成数据竞争。
type Adapter struct {
	mu sync.Mutex

	// decodeMu 保护 decoder 的跨帧累计状态，使并发误用同实例解码时不产生数据竞争。
	// 正确用法仍是每条流调用 NewStream 派生独立实例。
	decodeMu sync.Mutex

	streamStarted bool
	messageID     string
	nextIndex     int
	blockOpen     bool
	blockIndex    int
	blockKind     string
	// toolBuf 按 domain.ToolCall.Index 缓存工具调用的元数据与参数分片。
	// Anthropic 的 tool_use 块在 content_block_start 里一次性定死 id 与 name，后续帧无法补写，
	// 且内容块不能交错。并行工具调用的分片可能交错到达，故先按下标缓存，待流结束时按序成块。
	toolBuf map[int]*bufferedToolCall

	// decoder 保存面向上游的流式解码状态，与编码状态相互独立。
	decoder streamDecoder
}

var _ domain.Adapter = (*Adapter)(nil)

// bufferedToolCall 是流式编码期间按下标缓存的工具调用：元数据取首个非空值，
// 参数分片保序保存，成块时逐个发 input_json_delta，使帧边界与上游分片一一对应。
type bufferedToolCall struct {
	id        string
	name      string
	fragments []string
}

// New 返回一个适配器实例。
func New() *Adapter {
	return &Adapter{}
}

// NewStream 派生一个只服务单次流式响应的新适配器实例。
//
// 本适配器为一次流持有编码状态（内容块下标、是否已发送 message_start、打开的块类型）
// 与流式解码状态（模型、累积的工具调用参数），这些状态会跨帧演进。返回自身会让两条
// 并发流互相覆盖，因此本方法每次返回**全新实例**，不继承任何流式状态。
//
// 生命周期：一条并发流一个实例，实例不得跨流复用。调用方必须为每条流的编码方向
// 与解码方向分别派生实例。
func (a *Adapter) NewStream() domain.Adapter {
	return &Adapter{}
}

// Protocol 返回本适配器对应的协议。
func (a *Adapter) Protocol() domain.Protocol {
	return domain.ProtocolAnthropicMessages
}

// ContentType 返回非流式响应的 Content-Type。
func (a *Adapter) ContentType() string {
	return contentTypeJSON
}

// StreamContentType 返回流式响应的 Content-Type。
func (a *Adapter) StreamContentType() string {
	return contentTypeSSE
}

// ---------------------------------------------------------------------------
// wire 结构
// ---------------------------------------------------------------------------

type wireRequest struct {
	Model       string          `json:"model"`
	MaxTokens   *int            `json:"max_tokens"`
	System      json.RawMessage `json:"system"`
	Messages    []wireMessage   `json:"messages"`
	Tools       []wireTool      `json:"tools"`
	ToolChoice  json.RawMessage `json:"tool_choice"`
	Temperature *float64        `json:"temperature"`
	Stream      bool            `json:"stream"`
	// Metadata 是 Anthropic 的请求元数据；其中 user_id 作为会话标识参与前缀指纹拼装。
	Metadata struct {
		UserID string `json:"user_id"`
	} `json:"metadata"`
}

// decodeToolChoice 把 Anthropic 的 tool_choice 归一化为内部工具选择。
//
// 本协议只定义以下对象形态：
//   - auto：自动决定是否调用工具
//   - any：必须调用至少一个工具
//   - none：禁止调用工具
//   - tool：指定具体工具名
//
// 未识别的对象类型或指定工具时缺少 name 返回 invalid_request，不得静默丢弃客户端的工具选择要求。
func decodeToolChoice(raw json.RawMessage) (domain.ToolChoice, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == jsonNullLiteral {
		return domain.ToolChoice{}, nil
	}
	if trimmed[0] != '{' {
		return domain.ToolChoice{}, invalidRequest("tool_choice 必须是对象")
	}
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return domain.ToolChoice{}, invalidRequest("tool_choice 不是合法对象").WithCause(err)
	}
	switch obj.Type {
	case toolChoiceTypeAuto:
		return domain.ToolChoice{Mode: domain.ToolChoiceAuto}, nil
	case toolChoiceTypeAny:
		return domain.ToolChoice{Mode: domain.ToolChoiceRequired}, nil
	case toolChoiceTypeNone:
		return domain.ToolChoice{Mode: domain.ToolChoiceNone}, nil
	case toolChoiceTypeTool:
		if strings.TrimSpace(obj.Name) == "" {
			return domain.ToolChoice{}, invalidRequest("tool_choice 指定工具时缺少 name")
		}
		return domain.ToolChoice{Mode: domain.ToolChoiceTool, Name: obj.Name}, nil
	default:
		return domain.ToolChoice{}, invalidRequest(fmt.Sprintf("tool_choice 对象类型 %q 不受支持", obj.Type))
	}
}

type wireMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type wireTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type wireResponse struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	Role       string         `json:"role"`
	Model      string         `json:"model"`
	Content    []contentBlock `json:"content"`
	StopReason string         `json:"stop_reason"`
	Usage      *wireUsage     `json:"usage"`
}

// contentBlock 同时用于解码请求内容块与编解码响应内容块。
// 字段按块类型可选，解码时忽略与本类型无关的字段。
type contentBlock struct {
	Type string `json:"type"`

	// text
	Text string `json:"text,omitempty"`

	// thinking（响应方向的推理内容块）
	Thinking string `json:"thinking,omitempty"`

	// image
	Source *imageSource `json:"source,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type imageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type wireUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	// OutputTokensDetails 是输出计数的子项嵌套对象；为零时不写出。
	OutputTokensDetails *outputTokensDetails `json:"output_tokens_details,omitempty"`
}

// outputTokensDetails 是 usage.output_tokens_details 的解码结构，内层只含 thinking_tokens。
//
// 官方该对象的形状在仓库内没有可核对的文档快照（未核实），故宽容解析：整体不是JSON 对象时把自身清成
// 「未给出」并返回 nil 错误，不让上游的一次字段扩展把整帧或整个响应判为解码失败。
type outputTokensDetails struct {
	ThinkingTokens thinkingTokens `json:"thinking_tokens"`
}

// UnmarshalJSON 宽容解析嵌套对象：非 JSON 对象一律降级为「未给出」。
func (d *outputTokensDetails) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		*d = outputTokensDetails{}
		return nil
	}
	// 用别名解码，避免递归调用本方法；内层叶子的宽容解析由其自身 UnmarshalJSON 负责。
	type plain outputTokensDetails
	var parsed plain
	if err := json.Unmarshal(trimmed, &parsed); err != nil {
		*d = outputTokensDetails{}
		return nil //nolint:nilerr // 宽容解码：形状非预期时降级为「未给出」，不把错误上抛给调用方。
	}
	*d = outputTokensDetails(parsed)
	return nil
}

// thinkingTokens 是 usage.output_tokens_details.thinking_tokens 的解码类型。
//
// 叶子形状同样未核实，故宽容解析：非 JSON 数字（数组、字符串、对象等）时把自身清成
// 「未给出」并返回 nil 错误。valid 区分「上游给出 0」与「上游未给出该叶子」：
// 流式累计只在前者为真时覆盖已累计的子项，后者保留已有值不清零。
type thinkingTokens struct {
	value int
	valid bool
}

// UnmarshalJSON 解析叶子：只有 JSON 数字算「已给出」；其它形状降级为「未给出」。
func (t *thinkingTokens) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || string(trimmed) == jsonNullLiteral {
		*t = thinkingTokens{}
		return nil
	}
	var n int
	if err := json.Unmarshal(trimmed, &n); err != nil {
		*t = thinkingTokens{}
		return nil //nolint:nilerr // 宽容解码：叶子类型不符时降级为「未给出」，不把错误上抛给调用方。
	}
	*t = thinkingTokens{value: n, valid: true}
	return nil
}

// MarshalJSON 把叶子写回 JSON 数字，供跨协议重建的编码方向回填该子项。
func (t thinkingTokens) MarshalJSON() ([]byte, error) {
	return json.Marshal(t.value)
}

type wireError struct {
	Type  string          `json:"type"`
	Error wireErrorDetail `json:"error"`
}

type wireErrorDetail struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// ---------------------------------------------------------------------------
// 请求解码
// ---------------------------------------------------------------------------

// DecodeRequest 把 Anthropic Messages 请求体解码为统一内部请求。
func (a *Adapter) DecodeRequest(body []byte) (*domain.Request, error) {
	var wire wireRequest
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, invalidRequest("请求体不是合法 JSON")
	}
	if wire.Model == "" {
		return nil, invalidRequest("缺少必填字段 model")
	}
	if wire.MaxTokens == nil {
		return nil, invalidRequest("缺少必填字段 max_tokens")
	}
	if *wire.MaxTokens <= 0 {
		return nil, invalidRequest("max_tokens 必须为正整数")
	}
	if wire.Temperature != nil && (*wire.Temperature < 0 || *wire.Temperature > 1) {
		return nil, invalidRequest("temperature 必须在 [0, 1] 区间")
	}
	if len(wire.Messages) == 0 {
		return nil, invalidRequest("缺少必填字段 messages")
	}
	toolChoice, err := decodeToolChoice(wire.ToolChoice)
	if err != nil {
		return nil, err
	}

	messages := make([]domain.Message, 0, len(wire.Messages)+1)
	systemParts, err := decodeSystem(wire.System)
	if err != nil {
		return nil, err
	}
	decoded := make([]domain.Message, 0, len(wire.Messages))
	for i, m := range wire.Messages {
		role, ok := decodeRole(m.Role)
		if !ok {
			// 未识别的消息角色：跳过该条消息。
			continue
		}
		blocks, err := decodeContentBlocks(m.Content)
		if err != nil {
			return nil, err
		}
		parts, skipped, err := convertBlocks(blocks, false)
		if err != nil {
			return nil, err
		}
		if len(parts) == 0 {
			if skipped {
				// 内容全为未识别类型：内部表达不了，跳过该条消息。
				continue
			}
			return nil, invalidRequest(fmt.Sprintf("第 %d 条消息内容为空", i))
		}
		decoded = append(decoded, domain.Message{Role: role, Parts: parts})
	}
	if len(decoded) == 0 {
		return nil, invalidRequest("messages 中没有可解码的消息")
	}
	if len(systemParts) > 0 {
		messages = append(messages, domain.Message{Role: domain.RoleSystem, Parts: systemParts})
	}
	messages = append(messages, decoded...)

	tools := make([]domain.ToolSpec, 0, len(wire.Tools))
	for i, t := range wire.Tools {
		if t.Name == "" {
			return nil, invalidRequest(fmt.Sprintf("第 %d 个工具缺少 name", i))
		}
		params, err := rawJSONObject(t.InputSchema)
		if err != nil {
			return nil, err
		}
		tools = append(tools, domain.ToolSpec{
			Name:           t.Name,
			Description:    t.Description,
			ParametersJSON: params,
		})
	}

	req := &domain.Request{
		Protocol:    domain.ProtocolAnthropicMessages,
		Model:       wire.Model,
		Messages:    messages,
		Tools:       tools,
		ToolChoice:  toolChoice,
		MaxTokens:   *wire.MaxTokens,
		Temperature: wire.Temperature,
		Stream:      wire.Stream,
		// RawBody 必须拷贝：所有权归 Request，调用方后续复用或改写传入的 body
		// 不得影响已解码请求（RawBody 会被协议回退重放）。
		RawBody: bytes.Clone(body),
	}

	a.mu.Lock()
	a.resetStreamLocked()
	a.mu.Unlock()

	return req, nil
}

// decodeSystem 把顶层 system 归一化为 system 消息的文本片段。
// Anthropic 允许 system 为字符串或文本块数组。
func decodeSystem(raw json.RawMessage) ([]domain.Part, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == jsonNullLiteral {
		return nil, nil
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return nil, invalidRequest("system 不是合法字符串")
		}
		if text == "" {
			return nil, invalidRequest("system 内容为空")
		}
		return []domain.Part{{Kind: domain.PartText, Text: text}}, nil
	}

	var blocks []contentBlock
	if err := json.Unmarshal(trimmed, &blocks); err != nil {
		return nil, invalidRequest("system 必须是字符串或文本块数组")
	}
	if len(blocks) == 0 {
		return nil, invalidRequest("system 内容为空")
	}
	parts := make([]domain.Part, 0, len(blocks))
	for i, b := range blocks {
		switch b.Type {
		case blockTypeText:
			if b.Text == "" {
				return nil, invalidRequest(fmt.Sprintf("system 第 %d 个文本块内容为空", i))
			}
			parts = append(parts, domain.Part{Kind: domain.PartText, Text: b.Text})
		case blockTypeImage, blockTypeToolUse, blockTypeToolResult:
			return nil, invalidRequest(fmt.Sprintf("system 第 %d 个块类型 %q 不支持", i, b.Type))
		default:
			// 未识别的块类型：跳过。
		}
	}
	return parts, nil
}

// decodeRole 只接受 Anthropic messages 允许的 user / assistant。
// 第二个返回值为 false 表示角色未识别，调用方应跳过该条消息。
func decodeRole(role string) (domain.Role, bool) {
	switch role {
	case string(domain.RoleUser):
		return domain.RoleUser, true
	case string(domain.RoleAssistant):
		return domain.RoleAssistant, true
	default:
		return "", false
	}
}

// decodeContentBlocks 把 message.content 归一化为内容块数组。
// Anthropic 允许 content 直接是字符串，这里统一转成单个文本块。
func decodeContentBlocks(raw json.RawMessage) ([]contentBlock, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == jsonNullLiteral {
		return nil, nil
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return nil, invalidRequest("content 不是合法字符串")
		}
		if text == "" {
			return nil, invalidRequest("content 字符串为空")
		}
		return []contentBlock{{Type: blockTypeText, Text: text}}, nil
	}

	var blocks []contentBlock
	if err := json.Unmarshal(trimmed, &blocks); err != nil {
		return nil, invalidRequest("content 必须是字符串或内容块数组")
	}
	return blocks, nil
}

// convertBlocks 把内容块归一化为统一片段。未识别类型的块跳过；
// 第二个返回值报告是否发生了跳过，供上层区分「结构上的空消息」与
// 「只含未识别类型块、内部表示不了的消息」。已知类型缺少必填字段仍返回错误。
//
// decodeReasoning 是显式方向参数：
//   - 为 true 时把 thinking 块的文本建模为 PartReasoning（响应方向）
//   - 为 false 时与 redacted_thinking 一样跳过（请求方向）
//
// 请求方向的 thinking 块必须连同签名一并回传上游，建模反而会让跨协议重建产出缺签名的
// 请求，因此请求侧一律不建模。方向差异由该参数表达，不另复制一份块映射实现。
func convertBlocks(blocks []contentBlock, decodeReasoning bool) ([]domain.Part, bool, error) {
	parts := make([]domain.Part, 0, len(blocks))
	skipped := false
	for _, b := range blocks {
		switch b.Type {
		case blockTypeText:
			if b.Text == "" {
				return nil, false, invalidRequest("文本块内容为空")
			}
			parts = append(parts, domain.Part{Kind: domain.PartText, Text: b.Text})
		case blockTypeThinking:
			if !decodeReasoning || b.Thinking == "" {
				// 请求方向不建模推理；响应方向取不到文本（如只带密文）时也不产出空片段。
				skipped = true
				continue
			}
			parts = append(parts, domain.Part{Kind: domain.PartReasoning, Text: b.Thinking})
		case blockTypeImage:
			url, ok, err := imageURL(b.Source)
			if err != nil {
				return nil, false, err
			}
			if !ok {
				skipped = true
				continue
			}
			parts = append(parts, domain.Part{Kind: domain.PartImage, ImageURL: url})
		case blockTypeToolUse:
			if b.ID == "" || b.Name == "" {
				return nil, false, invalidRequest("tool_use 块缺少 id 或 name")
			}
			input, err := rawJSONObject(b.Input)
			if err != nil {
				return nil, false, err
			}
			if input == "" {
				input = "{}"
			}
			parts = append(parts, domain.Part{
				Kind:     domain.PartToolCall,
				ToolCall: &domain.ToolCall{ID: b.ID, Name: b.Name, Arguments: input},
			})
		case blockTypeToolResult:
			if b.ToolUseID == "" {
				return nil, false, invalidRequest("tool_result 块缺少 tool_use_id")
			}
			content, err := toolResultContent(b.Content)
			if err != nil {
				return nil, false, err
			}
			parts = append(parts, domain.Part{
				Kind:       domain.PartToolResult,
				ToolResult: &domain.ToolResult{ToolCallID: b.ToolUseID, Content: content},
			})
		default:
			// 未识别的内容块类型（redacted_thinking 及上游新增块）：跳过该块并继续，不判整条请求或响应失败。
			skipped = true
		}
	}
	return parts, skipped, nil
}

// rawJSONObject 返回 JSON 对象的紧凑原文；空值返回空串，非对象或非法 JSON 报错。
// 紧凑化保证同一对象在往返编解码后得到稳定的字符串，便于比较与重放。
func rawJSONObject(raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == jsonNullLiteral {
		return "", nil
	}
	if trimmed[0] != '{' {
		return "", invalidRequest("JSON 字段必须是对象")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, trimmed); err != nil {
		return "", invalidRequest("JSON 对象不是合法 JSON")
	}
	return compact.String(), nil
}

// imageURL 把 Anthropic 图片 source 归一化为单一 URL：
//   - base64 图片转为 data URI
//   - url 图片原样保留
//
// 第二个返回值为 false 表示 source 类型未识别，调用方应跳过该图片块。
func imageURL(src *imageSource) (string, bool, error) {
	if src == nil {
		return "", false, invalidRequest("图片块缺少 source")
	}
	switch src.Type {
	case "url":
		if src.URL == "" {
			return "", false, invalidRequest("图片块缺少 url")
		}
		return src.URL, true, nil
	case "base64":
		if src.MediaType == "" || src.Data == "" {
			return "", false, invalidRequest("base64 图片块缺少 media_type 或 data")
		}
		return "data:" + src.MediaType + ";base64," + src.Data, true, nil
	default:
		// 未识别的 source 类型：跳过该图片块。
		return "", false, nil
	}
}

// toolResultContent 把 tool_result.content 归一化为字符串。
// Anthropic 允许字符串或文本块数组；数组按顺序拼接文本。
func toolResultContent(raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == jsonNullLiteral {
		return "", nil
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return "", invalidRequest("tool_result.content 不是合法字符串")
		}
		return text, nil
	}

	var blocks []contentBlock
	if err := json.Unmarshal(trimmed, &blocks); err != nil {
		return "", invalidRequest("tool_result.content 必须是字符串或内容块数组")
	}
	var sb strings.Builder
	for _, b := range blocks {
		switch b.Type {
		case blockTypeText:
			sb.WriteString(b.Text)
		case blockTypeImage, blockTypeToolUse, blockTypeToolResult:
			// 已知块类型但不在 tool_result.content 允许的范围内：仍然拒绝。
			return "", invalidRequest(fmt.Sprintf("tool_result.content 块类型 %q 不支持", b.Type))
		default:
			// 未识别的块类型：跳过。
		}
	}
	return sb.String(), nil
}

// ---------------------------------------------------------------------------
// 响应编解码
// ---------------------------------------------------------------------------

// DecodeResponse 把 Anthropic Messages 非流式响应体归一化为统一响应。
func (a *Adapter) DecodeResponse(body []byte) (*domain.Response, error) {
	var wire wireResponse
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, invalidRequest("响应体不是合法 JSON")
	}
	// 「能解成 JSON 对象」不等于「这是本协议的响应」。上游常常用 2xx 回错误信封
	// （例如 {"code":500,"msg":"404 NOT_FOUND","success":false}），或返回别的协议的报文；
	// 若在此放过，网关会把一个空响应当成成功履约，向客户端回一条内容为空的完成响应。
	// 使用者拿到的是「看起来成功但什么都没生成」，比直接报错更难定位，因此必须校验判据字段。
	if wire.Type != messageType {
		return nil, invalidRequest(fmt.Sprintf("响应 type 为 %q，期望 %q", wire.Type, messageType))
	}
	if wire.Content == nil {
		return nil, invalidRequest("响应缺少 content")
	}
	parts, _, err := convertBlocks(wire.Content, true)
	if err != nil {
		return nil, err
	}
	return &domain.Response{
		Model:             wire.Model,
		Message:           domain.Message{Role: domain.RoleAssistant, Parts: parts},
		FinishReason:      decodeFinishReason(wire.StopReason),
		Usage:             usageFromWire(wire.Usage),
		UpstreamRequestID: wire.ID,
	}, nil
}

// DecodeStreamFrame 把上游一帧 SSE 解码为内部统一分片。
//
// event 为事件名，data 为 data 字段正文。返回空切片表示该帧不产生分片
// （如 message_start、content_block_start、ping 与 message_stop）。
// 解码需要跨帧累计状态（模型、工具参数与用量），故一次上游流必须使用一个独立实例（经 NewStream 派生）。
// 该状态与编码方向相互独立，但实例同样不得在多个并发流之间共享。
// decodeMu 仅用于兜底：即使调用方误把同实例用于并发解码，也不产生数据竞争。
func (a *Adapter) DecodeStreamFrame(event string, data []byte) ([]domain.Chunk, error) {
	a.decodeMu.Lock()
	defer a.decodeMu.Unlock()
	return a.decoder.decode(event, data)
}

// FinishStream 恒返回空。
//
// Anthropic Messages 的结束信号是真实帧 message_stop，
// Decoder 已在DecodeStreamFrame 的 message_stop 分支产出唯一的 ChunkStreamEnd
// （携带累计用量与结束原因）；EOF 处不需要也不必再补收尾分片，
// 缺 message_stop 的流因此仍按截断处理。
func (a *Adapter) FinishStream() []domain.Chunk { return nil }

// ReportedUsage 返回本流已从上游陈述事实中记录的输入侧与缓存侧计数。
//
// 上游在 message_start 与 message_delta 里陈述输入侧与缓存侧计数，而输出计数要到
// message_stop 才是最终值，因此本方法不返回输出侧。第二值为 false 表示上游尚未陈述
// 任何输入侧事实。
//
// 该方法与解码方向共用同一把锁，避免与并发误用同一实例解码产生数据竞争。
func (a *Adapter) ReportedUsage() (domain.Usage, bool) {
	a.decodeMu.Lock()
	defer a.decodeMu.Unlock()
	return a.decoder.reportedUsage()
}

// EncodeResponse 把统一响应编码为 Anthropic Messages 非流式响应体。
func (a *Adapter) EncodeResponse(resp *domain.Response) ([]byte, error) {
	if resp == nil {
		return nil, domain.NewError(domain.CodeInternal, "响应为空")
	}
	blocks := make([]contentBlock, 0, len(resp.Message.Parts))
	for _, p := range resp.Message.Parts {
		switch p.Kind {
		case domain.PartText:
			blocks = append(blocks, contentBlock{Type: blockTypeText, Text: p.Text})
		case domain.PartToolCall:
			if p.ToolCall == nil {
				return nil, domain.NewError(domain.CodeInternal, "工具调用片段缺少 ToolCall")
			}
			args := strings.TrimSpace(p.ToolCall.Arguments)
			if args == "" {
				args = "{}"
			}
			if args[0] != '{' || !json.Valid([]byte(args)) {
				return nil, domain.NewError(domain.CodeInternal, "工具调用参数不是合法 JSON 对象")
			}
			blocks = append(blocks, contentBlock{
				Type:  blockTypeToolUse,
				ID:    p.ToolCall.ID,
				Name:  p.ToolCall.Name,
				Input: json.RawMessage(args),
			})
		case domain.PartReasoning:
			// 推理内容不下发：Anthropic 的 thinking 块要求签名且纯文本无法重建，此处显式跳过。
		default:
			return nil, domain.NewError(domain.CodeInternal, fmt.Sprintf("响应中出现不支持的内容块类型 %q", string(p.Kind)))
		}
	}

	usage := wireUsageFromDomain(resp.Usage)
	// 响应 id 优先取上游标识（同协议透传时客户端可见），上游未给出时由本适配器生成器兜底。
	id := resp.UpstreamRequestID
	if id == "" {
		id = newMessageID()
	}
	out := wireResponse{
		ID:         id,
		Type:       messageType,
		Role:       "assistant",
		Model:      resp.Model,
		Content:    blocks,
		StopReason: encodeFinishReason(resp.FinishReason),
		Usage:      &usage,
	}
	return json.Marshal(out)
}

// usageFromWire 把上游 usage 归一化为内部用量。
//
// Anthropic 的线格式 input_tokens 不含缓存 token，一次调用的输入总量是 input_tokens、
// cache_read_input_tokens 与 cache_creation_input_tokens 三者之和。内部统一口径是
// 「子项 ⊆ 主计数」，因此这里做加法，两个缓存计数同时作为子项保留。
//
// `output_tokens_details.thinking_tokens` 是 output_tokens 的子项：写入 ReasoningTokens，
// OutputTokens 仍取线格式 output_tokens 原值、**不相加**。
//
// 上游未给出 usage 时返回零值，即来源为 domain.UsageSourceUnknown 的「未取得用量」；
// 调用方必须据此走兜底分支，不得按零结算。
//
// 思考计数由上游单独给出，可能与 output_tokens 倒挂，故返回前把子项钳回主计数内
// （见 domain.Usage.BoundSubitemsToMain）。
func usageFromWire(u *wireUsage) domain.Usage {
	if u == nil {
		return domain.Usage{}
	}
	usage := domain.Usage{
		Source:           domain.UsageSourceUpstream,
		InputTokens:      u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadInputTokens,
		CacheWriteTokens: u.CacheCreationInputTokens,
	}
	if u.OutputTokensDetails != nil && u.OutputTokensDetails.ThinkingTokens.valid {
		usage.ReasoningTokens = u.OutputTokensDetails.ThinkingTokens.value
	}
	return usage.BoundSubitemsToMain()
}

// wireUsageFromDomain 把内部用量还原为 Anthropic 线格式。
//
// 线格式的 input_tokens 不含缓存 token，故由内部输入总数减去两个缓存子项得到。
// 内部计数与子项不一致（子项之和大于主计数）时差值落负，钳制为 0。
// 输出与两个缓存字段原样写出；思考 token 子项非零时回填 output_tokens_details，为零时省略该嵌套对象。
func wireUsageFromDomain(usage domain.Usage) wireUsage {
	inputTokens := usage.InputTokens - usage.CacheReadTokens - usage.CacheWriteTokens
	if inputTokens < 0 {
		inputTokens = 0
	}
	wire := wireUsage{
		InputTokens:              inputTokens,
		OutputTokens:             usage.OutputTokens,
		CacheReadInputTokens:     usage.CacheReadTokens,
		CacheCreationInputTokens: usage.CacheWriteTokens,
	}
	if usage.ReasoningTokens != 0 {
		wire.OutputTokensDetails = &outputTokensDetails{
			ThinkingTokens: thinkingTokens{value: usage.ReasoningTokens, valid: true},
		}
	}
	return wire
}

// decodeFinishReason 把 Anthropic 结束原因字面量映射到统一枚举。
// 未识别字面量必须映射为 FinishUnknown，不得当成正常结束。
func decodeFinishReason(literal string) domain.FinishReason {
	switch literal {
	case "end_turn", "stop_sequence":
		return domain.FinishStop
	case "max_tokens":
		return domain.FinishLength
	case "tool_use":
		return domain.FinishToolCalls
	case "refusal":
		return domain.FinishContentFilter
	default:
		return domain.FinishUnknown
	}
}

// encodeFinishReason 把统一结束原因映射回 Anthropic 字面量。
// FinishUnknown 没有对应字面量，返回空串（JSON 中省略 stop_reason）。
func encodeFinishReason(reason domain.FinishReason) string {
	switch reason {
	case domain.FinishStop:
		return "end_turn"
	case domain.FinishLength:
		return "max_tokens"
	case domain.FinishToolCalls:
		return "tool_use"
	case domain.FinishContentFilter:
		return "refusal"
	default:
		return ""
	}
}

// ---------------------------------------------------------------------------
// 错误编码
// ---------------------------------------------------------------------------

// EncodeError 把错误编码为 Anthropic 错误响应体与 HTTP 状态码。
func (a *Adapter) EncodeError(err error) (int, []byte) {
	status := domain.HTTPStatus(err)
	errorType := "api_error"
	message := "服务内部错误"
	if e := domain.AsError(err); e != nil {
		errorType = errorTypeForCode(e.Code)
		if e.Message != "" {
			message = e.Message
		}
	}
	body, marshalErr := json.Marshal(wireError{
		Type:  eventError,
		Error: wireErrorDetail{Type: errorType, Message: message},
	})
	if marshalErr != nil {
		// 结构体只含字符串且序列化不会失败，此处兜底避免返回空 body。
		return status, []byte(`{"type":"error","error":{"type":"api_error","message":"服务内部错误"}}`)
	}
	return status, body
}

// errorTypeForCode 把统一错误码映射为 Anthropic 错误类型。
func errorTypeForCode(code domain.Code) string {
	switch code {
	case domain.CodeInvalidRequest:
		return "invalid_request_error"
	case domain.CodeUnauthorized:
		return "authentication_error"
	case domain.CodeForbidden:
		return "permission_error"
	case domain.CodeRateLimited, domain.CodeUpstreamRateLimited:
		return "rate_limit_error"
	case domain.CodeModelNotFound:
		return "not_found_error"
	default:
		return "api_error"
	}
}

// ---------------------------------------------------------------------------
// 流式编码状态
// ---------------------------------------------------------------------------

// resetStreamLocked 清空一轮流的编码进度。
func (a *Adapter) resetStreamLocked() {
	a.streamStarted = false
	a.messageID = ""
	a.nextIndex = 0
	a.blockOpen = false
	a.blockIndex = 0
	a.blockKind = ""
	a.toolBuf = nil
}

// EncodeStreamStart 发出 Anthropic 的 message_start 帧，开始一轮流。
//
// 适配器需要为一次流保存内容块下标与消息标识，故一次流必须使用一个独立实例，不同并发流不得共享实例。
// 上一轮流尚未结束时重复调用返回错误，避免产生两段 message_start。
//
// model 是本次重建要写入 message_start.message.model 的上游模型名，由重建侧传入。
// 开始帧的模型名是响应侧决策，不再依赖实例上由 DecodeRequest 记录的客户端模型名。
func (a *Adapter) EncodeStreamStart(model string) ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.streamStarted {
		return nil, domain.NewError(domain.CodeInternal, "流已经开始，不能重复发送 message_start")
	}
	a.resetStreamLocked()
	a.streamStarted = true
	a.messageID = newMessageID()

	var buf bytes.Buffer
	if err := writeSSE(&buf, "message_start", messageStartEvent{
		Type: "message_start",
		Message: messageStartMessage{
			ID:      a.messageID,
			Type:    messageType,
			Role:    "assistant",
			Model:   model,
			Content: []contentBlock{},
			Usage:   wireUsage{},
		},
	}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// EncodeChunk 把一个内容分片编码为 Anthropic 的 content_block_* 帧。
//
// 必须在 EncodeStreamStart 之后调用。用量与结束原因不由本方法编码：它们随
// Kind 为 ChunkStreamEnd 的结束分片交给 EncodeStreamEnd，统一发成 message_delta
// 与 message_stop；传入 ChunkUsage/ChunkFinish/ChunkStreamEnd 属调用方错误，返回错误。
func (a *Adapter) EncodeChunk(chunk domain.Chunk) ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if !a.streamStarted {
		return nil, domain.NewError(domain.CodeInternal, "流尚未开始，编码分片前必须调用 EncodeStreamStart")
	}

	var buf bytes.Buffer
	switch chunk.Kind {
	case domain.ChunkTextDelta:
		if !a.blockOpen || a.blockKind != openKindText {
			if err := a.closeBlockLocked(&buf); err != nil {
				return nil, err
			}
			if err := a.openBlockLocked(&buf, openKindText, nil); err != nil {
				return nil, err
			}
		}
		err := writeSSE(&buf, eventContentBlockDelta, contentBlockDeltaEvent{
			Type:  eventContentBlockDelta,
			Index: a.blockIndex,
			Delta: deltaBody{Type: "text_delta", Text: chunk.TextDelta},
		})
		if err != nil {
			return nil, err
		}

	case domain.ChunkToolCallDelta:
		if chunk.ToolCall == nil {
			return nil, domain.NewError(domain.CodeInternal, "工具调用分片缺少 ToolCall")
		}
		// 不在此处开块：并行调用的参数分片可能交错到达，而 Anthropic 的内容块不能交错、
		// tool_use 块的 id 与 name 又只能随 content_block_start 给出。先按下标缓存，
		// 流结束时由 EncodeStreamEnd 按序成块。因此与文本分片先到、工具分片后到的输入不同，
		// 工具块可能相对到达顺序后移，客户端看到的块顺序以本编码器为准。
		a.bufferToolCallLocked(chunk.ToolCall)
		return nil, nil

	case domain.ChunkReasoningDelta:
		// 推理增量在编码方向不下发，显式跳过而不报错。
		return nil, nil

	default:
		return nil, domain.NewError(domain.CodeInternal, fmt.Sprintf("流式分片类型 %q 不是内容分片；用量与结束原因请通过 EncodeStreamEnd 编码", string(chunk.Kind)))
	}

	return buf.Bytes(), nil
}

// EncodeStreamEnd 把结束分片编码为 Anthropic 的 message_delta 与 message_stop 两帧。
//
// Anthropic 规定 message_delta 承载 stop_reason 与 usage，message_stop 标记流结束。
// 本方法把两帧拼接后一起返回。结束前若仍有打开的内容块，本方法补发 content_block_stop。
// 流未开始或已结束时调用返回错误，避免重复发出 message_stop。结束后重置编码状态。
func (a *Adapter) EncodeStreamEnd(chunk domain.Chunk) ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if !a.streamStarted {
		return nil, domain.NewError(domain.CodeInternal, "流尚未开始或已结束，不能重复发送 message_stop")
	}

	var buf bytes.Buffer
	if err := a.closeBlockLocked(&buf); err != nil {
		return nil, err
	}
	if err := a.flushToolCallsLocked(&buf); err != nil {
		return nil, err
	}
	var usage domain.Usage
	if chunk.Usage != nil {
		usage = *chunk.Usage
	}
	if err := a.writeMessageDeltaLocked(&buf, encodeFinishReason(chunk.FinishReason), usage); err != nil {
		return nil, err
	}
	if err := writeSSE(&buf, "message_stop", messageStopEvent{Type: "message_stop"}); err != nil {
		return nil, err
	}
	a.resetStreamLocked()
	return buf.Bytes(), nil
}

func (a *Adapter) openBlockLocked(buf *bytes.Buffer, kind string, call *domain.ToolCall) error {
	index := a.nextIndex
	event := contentBlockStartEvent{Type: "content_block_start", Index: index}
	if kind == openKindToolUse {
		event.ContentBlock = toolUseStartBlock{
			Type:  blockTypeToolUse,
			ID:    call.ID,
			Name:  call.Name,
			Input: json.RawMessage("{}"),
		}
	} else {
		event.ContentBlock = textStartBlock{Type: blockTypeText, Text: ""}
	}
	if err := writeSSE(buf, "content_block_start", event); err != nil {
		return err
	}
	a.blockOpen = true
	a.blockIndex = index
	a.blockKind = kind
	a.nextIndex++
	return nil
}

func (a *Adapter) closeBlockLocked(buf *bytes.Buffer) error {
	if !a.blockOpen {
		return nil
	}
	index := a.blockIndex
	a.blockOpen = false
	a.blockKind = ""
	return writeSSE(buf, "content_block_stop", contentBlockStopEvent{Type: "content_block_stop", Index: index})
}

// bufferToolCallLocked 把一条工具调用分片按下标缓存：元数据取首个非空值
// （首个分片通常带id 与 name，续片只有参数），参数分片保序保存。
func (a *Adapter) bufferToolCallLocked(call *domain.ToolCall) {
	if a.toolBuf == nil {
		a.toolBuf = make(map[int]*bufferedToolCall)
	}
	buffered := a.toolBuf[call.Index]
	if buffered == nil {
		buffered = &bufferedToolCall{}
		a.toolBuf[call.Index] = buffered
	}
	if buffered.id == "" {
		buffered.id = call.ID
	}
	if buffered.name == "" {
		buffered.name = call.Name
	}
	if call.Arguments != "" {
		buffered.fragments = append(buffered.fragments, call.Arguments)
	}
}

// flushToolCallsLocked 在流结束前把缓存的工具调用按下标升序成块：每个调用一块，
// 逐片发 input_json_delta。下标顺序确定，使同一请求的成块顺序可复现。
//
// 缺少 id 或 name 的调用无法写出合法块，返回平台内部错误而不是写出客户端必然拒绝的坏帧。
func (a *Adapter) flushToolCallsLocked(buf *bytes.Buffer) error {
	if len(a.toolBuf) == 0 {
		return nil
	}
	indexes := make([]int, 0, len(a.toolBuf))
	for index := range a.toolBuf {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	for _, index := range indexes {
		buffered := a.toolBuf[index]
		if buffered.id == "" || buffered.name == "" {
			return domain.NewError(domain.CodeInternal, fmt.Sprintf(
				"工具调用下标 %d 缺少 id 或 name；Anthropic 的 tool_use 块只能在 content_block_start 里给出 id 与 name，无法后续补写",
				index))
		}
		if err := a.openBlockLocked(buf, openKindToolUse, &domain.ToolCall{ID: buffered.id, Name: buffered.name}); err != nil {
			return err
		}
		for _, fragment := range buffered.fragments {
			if err := writeSSE(buf, eventContentBlockDelta, contentBlockDeltaEvent{
				Type:  eventContentBlockDelta,
				Index: a.blockIndex,
				Delta: deltaBody{Type: deltaTypeInputJSON, PartialJSON: fragment},
			}); err != nil {
				return err
			}
		}
		if err := a.closeBlockLocked(buf); err != nil {
			return err
		}
	}
	return nil
}

// writeMessageDeltaLocked 写出 message_delta。
//
// Anthropic 的 message_delta 总带 usage。用量未知时按零值上报（仅影响面向客户端的显示），
// 结算方必须以 domain.Usage 的来源判定而非依赖零值。线格式的 input_tokens 已由 wireUsageFromDomain 减去缓存子项。
func (a *Adapter) writeMessageDeltaLocked(buf *bytes.Buffer, stopReason string, usage domain.Usage) error {
	wire := wireUsageFromDomain(usage)
	event := messageDeltaEvent{
		Type:  "message_delta",
		Delta: messageDeltaBody{StopReason: stopReason},
		Usage: &wire,
	}
	return writeSSE(buf, "message_delta", event)
}

// ---------------------------------------------------------------------------
// SSE 帧与事件结构
// ---------------------------------------------------------------------------

type messageStartEvent struct {
	Type    string              `json:"type"`
	Message messageStartMessage `json:"message"`
}

type messageStartMessage struct {
	ID      string         `json:"id"`
	Type    string         `json:"type"`
	Role    string         `json:"role"`
	Model   string         `json:"model"`
	Content []contentBlock `json:"content"`
	Usage   wireUsage      `json:"usage"`
}

type contentBlockStartEvent struct {
	Type         string `json:"type"`
	Index        int    `json:"index"`
	ContentBlock any    `json:"content_block"`
}

type textStartBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolUseStartBlock struct {
	Type  string          `json:"type"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type contentBlockDeltaEvent struct {
	Type  string    `json:"type"`
	Index int       `json:"index"`
	Delta deltaBody `json:"delta"`
}

type deltaBody struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
}

type contentBlockStopEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
}

type messageDeltaEvent struct {
	Type  string           `json:"type"`
	Delta messageDeltaBody `json:"delta"`
	Usage *wireUsage       `json:"usage,omitempty"`
}

type messageDeltaBody struct {
	StopReason string `json:"stop_reason,omitempty"`
}

type messageStopEvent struct {
	Type string `json:"type"`
}

// writeSSE 按 `event: <name>\ndata: <json>\n\n` 写出一帧。
func writeSSE(buf *bytes.Buffer, event string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return domain.NewError(domain.CodeInternal, fmt.Sprintf("序列化 SSE 事件 %s 失败: %s", event, err))
	}
	buf.WriteString("event: ")
	buf.WriteString(event)
	buf.WriteString("\n")
	buf.WriteString("data: ")
	buf.Write(data)
	buf.WriteString("\n\n")
	return nil
}

// newMessageID 为流式 message_start 生成消息标识。
// Anthropic 上游的消息 id 不进入 domain.Chunk，故此处生成一个本地唯一值填充。
func newMessageID() string {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "msg_stream"
	}
	return "msg_" + hex.EncodeToString(raw[:])
}

// invalidRequest 构造 invalid_request 统一错误。
func invalidRequest(message string) *domain.Error {
	return domain.NewError(domain.CodeInvalidRequest, message)
}
