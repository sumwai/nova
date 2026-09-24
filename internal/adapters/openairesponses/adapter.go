// Package openairesponses 实现 OpenAI Responses 协议（POST /v1/responses）
// 与内部统一格式的双向转换。
//
// 本适配器同时覆盖两个方向：
//   - 面向客户端：DecodeRequest 解码请求；EncodeResponse、EncodeStreamStart、
//     EncodeChunk、EncodeStreamEnd、EncodeError 生成 Responses 响应体与 SSE 帧；
//   - 面向上游：DecodeResponse 与 DecodeStreamFrame 把上游 Responses 响应归一化为
//     内部统一响应与分片，状态字面量（completed、incomplete 等）映射到
//     domain.FinishReason，未识别取值一律映射为 domain.FinishUnknown。
//
// 非流式方法（DecodeRequest、EncodeResponse、DecodeResponse、EncodeError）无状态；
// 流式解码需要缓存上游 output_item.added 带出的工具函数名，流式编码需要维护条目状态：
//   - 已上屏条目列表
//   - 网关分配的 output_index
//   - 条目 id
//   - 累计文本与参数
//
// 因此一次流必须使用一个独立实例，不同并发流不得共享实例，由 NewStream 为每条流派生。
package openairesponses

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/sumwai/nova/internal/domain"
)

// Responses 协议的条目类型、内容类型与状态字面量。
const (
	// itemTypeMessage、itemTypeFunctionCall 与 itemTypeFunctionCallOutput 是 input/output 条目的 type 取值。
	itemTypeMessage            = "message"
	itemTypeFunctionCall       = "function_call"
	itemTypeFunctionCallOutput = "function_call_output"
	// itemTypeReasoning 是输出中的推理条目类型，只在解码方向建模。
	itemTypeReasoning = "reasoning"

	// contentTypeOutputText 是消息条目里模型输出文本片段的内容类型。
	contentTypeOutputText = "output_text"

	// objectResponse 是响应对象的 object 取值。
	objectResponse = "response"

	// statusCompleted 与 statusIncomplete 是响应状态取值，分别映射完成与长度截断。
	statusCompleted  = "completed"
	statusIncomplete = "incomplete"

	// statusInProgress 是条目上屏时的状态取值。
	statusInProgress = "in_progress"

	// eventOutputItemAdded 是条目上屏事件名，编码方向的文本与工具条目共用。
	eventOutputItemAdded = "response.output_item.added"

	// jsonNullLiteral 是 JSON null 的文本表示，用于判断可选字段是否显式给出。
	jsonNullLiteral = "null"

	// itemIDPrefixMessage 与 itemIDPrefixFunctionCall 是适配器生成的条目 id 前缀。
	// message 条目用 `msg_`、function_call 条目用 `fc_`，与响应级 id 的 `resp_` 区分。
	itemIDPrefixMessage      = "msg"
	itemIDPrefixFunctionCall = "fc"
)

// sseFrameOverheadBytes 是 Responses SSE 帧除事件名与 JSON 负载外的固定开销：
// "event: " 7 字节、"\ndata: " 7 字节与结尾 "\n\n" 2 字节。
const sseFrameOverheadBytes = 16

// toolCallMeta 是 output_item.added 缓存下来、供后续参数增量回填的工具调用信息。
//
// 上游的 function_call_arguments.delta 只带 item_id（条目标识）与 output_index，不带 call_id；
// 函数名与工具调用 id 都只出现在条目对象里，因此 callID 只能从 output_item.added 的 item 上记录。
type toolCallMeta struct {
	index  int
	name   string
	callID string
}

// Adapter 实现 domain.Adapter，负责 OpenAI Responses 协议的编解码。
//
// 非流式方法无状态；流式编解码各自持有跨帧状态，故一次流必须用独立实例
// （见 NewStream）。互斥锁防止同一实例的流式方法被并发误用。
type Adapter struct {
	mu sync.Mutex

	// decodeCalls 缓存本轮流 response.output_item.added 带出的元数据，键为条目对象上的 id（item_id）
	// 与 call_id（两者都非空时各登记一次）。参数增量帧只带 item_id、没有 call_id 与函数名，据此回填。
	decodeCalls map[string]toolCallMeta
	// decodeSawToolCall 记录本轮流是否确实向客户端下发了工具调用分片：Responses 没有专门的工具调用结束状态，
	// 结束原因必须在流结束时结合这一事实推断（完成状态且下发了工具调用 → FinishToolCalls）。
	// 故在产出 ChunkToolCallDelta 的参数增量帧置位；条目上屏帧只是控制帧、不产出分片，不在此置位，
	// 以免出现「结束原因判为工具调用、客户端流里却没有工具块」的自相矛盾形态。
	// 它与 decodeCalls 同属单轮流状态，随流结束清空，不得污染下一轮流。
	decodeSawToolCall bool
	// streamResponseID 是本轮流式响应的 id：EncodeStreamStart 生成一次，
	// 同一轮流的 response.created 与结束帧共用；流结束时清空，下一轮换新 id。
	// 它与条目级 id（encodeItem.id）是两类标识，取值不相等。
	streamResponseID string
	// nextOutputIndex 是下一个待分配的条目 output_index。网关按条目首次上屏的顺序
	// 从 0 起逐条分配，不沿用上游的 domain.ToolCall.Index（其口径随上游协议而变）。
	nextOutputIndex int
	// openItem 是当前处于打开状态的条目（至多一个）；开启新条目前先关闭它。
	openItem *encodeItem
	// callItems 按内部 domain.ToolCall.Index 记录 function_call 条目，含已关闭的条目：
	// 已关闭条目再次收到同下标参数增量时沿用原 id 与 output_index。
	callItems map[int]*encodeItem
}

// encodeItem 是流式编码期间一个已上屏的条目（message 或 function_call）。
//
// 条目 id 由适配器生成（`msg_`／`fc_` 前缀），同一条目跨帧复用、流结束时清空；
// 它不是工具调用的 call_id（call_id 原样保留上游给出的取值）。
type encodeItem struct {
	kind        string
	outputIndex int
	id          string
	// text 是 message 条目的累计文本；arguments 是 function_call 条目的累计参数。
	text      string
	arguments string
	callID    string
	name      string
	// named 报告是否已下发过带函数名的 output_item.added，供函数名晚到时判断补发。
	named bool
	// done 报告条目已关闭（已给出 .done 帧）。已关闭的 function_call 条目再次收到
	// 同下标增量时只给 delta，不重发 added 与 done，也不改写已给出的 .done 载荷。
	done bool
}

// New 返回一个新的 OpenAI Responses 适配器。
func New() *Adapter { return &Adapter{} }

// newResponseID 为跨协议重建兜底生成 Responses 响应标识。
//
// 仅在上游未给出 id 时使用；上游给出 id 时原样保留。
func newResponseID() string {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "resp_local"
	}
	return "resp_" + hex.EncodeToString(raw[:])
}

// newItemID 为跨协议重建的条目生成标识：<prefix>_<随机十六进制>。
//
// 条目 id 每个条目一个、同一轮流的该条目各帧共用、流结束时随其余流式状态一并清空，
// 同一实例的下一轮流全部换新；它与响应级 id（resp_）是两类标识。
// function_call 条目的 item.id 不是 item.call_id：call_id 必须原样保留上游给出的工具调用 id（客户端靠它回放工具结果）。
func newItemID(prefix string) string {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return prefix + "_local"
	}
	return prefix + "_" + hex.EncodeToString(raw[:])
}

var _ domain.Adapter = (*Adapter)(nil)

// Protocol 返回本适配器对应的对外协议。
func (a *Adapter) Protocol() domain.Protocol { return domain.ProtocolOpenAIResponses }

// NewStream 为单次流式响应派生一个独立实例。
//
// 流式解码与流式编码各自维护单条流的跨帧状态：
//   - 解码方向：缓存 output_item.added 带出的 item_id 与 call_id 到元数据映射
//   - 编码方向：维护当前打开的条目、已上屏的 function_call 条目以及下一个待分配的 output_index
//
// 这些状态属于单条流，不能跨并发流共享。故这里每次返回全新实例，而非复用当前实例。
func (a *Adapter) NewStream() domain.Adapter { return &Adapter{} }

// ContentType 返回非流式响应的 Content-Type。
func (a *Adapter) ContentType() string { return "application/json" }

// StreamContentType 返回流式响应的 Content-Type。
func (a *Adapter) StreamContentType() string { return "text/event-stream" }

// wireRequest 是 Responses 请求体的线上结构。未列出的字段按未知字段忽略。
type wireRequest struct {
	Model           string          `json:"model"`
	Input           json.RawMessage `json:"input"`
	Instructions    string          `json:"instructions"`
	MaxOutputTokens *int            `json:"max_output_tokens"`
	Temperature     *float64        `json:"temperature"`
	Tools           []wireTool      `json:"tools"`
	ToolChoice      json.RawMessage `json:"tool_choice"`
	Stream          bool            `json:"stream"`
	// RequestID 是网关扩展字段：请求体若携带则沿用，否则由调用方注入。
	RequestID string `json:"request_id"`
	// User 是 OpenAI 客户端自带的终端用户标识；非空时作为会话标识参与前缀指纹拼装。
	User string `json:"user"`
}

// wireTool 是 Responses 的函数工具定义。
//
// 兼容两种写法：Responses 的扁平写法（type/name/parameters）与
// Chat Completions 的嵌套写法（type/function）。
type wireTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Function    *wireFunction   `json:"function"`
}

type wireFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// wireInputItem 是 input 数组里的一种条目。字段按条目类型取用。
type wireInputItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Output    json.RawMessage `json:"output"`
}

// wireContentPart 是消息 content 数组里的一种内容片段。
type wireContentPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	ImageURL json.RawMessage `json:"image_url"`
}

// wireImageURL 兼容 image_url 为对象（{"url": "..."}）的写法。
type wireImageURL struct {
	URL string `json:"url"`
}

// DecodeRequest 把客户端 Responses 请求体解码为内部统一请求。
//
//  1. 未知字段忽略；
//  2. 未识别的条目类型、角色与内容类型跳过，跳过全部条目时仍返回domain.CodeInvalidRequest；
//  3. 已知类型缺少必填字段也返回 domain.CodeInvalidRequest。
//
// temperature 区间（[0, 2]）在解码期先拦一次，与 domain.Request.Validate 构成有意的双点校验：
// 适配器负责「它解析的字段它自己拦」，Validate 负责「归一化后的请求整体拦」，两处状态码同为 400。
// Validate 的调用点在共享转发流水线内，与本解码期各执行一次。适配器这一份长期保留，不为「消除重复」
// 删除：早拦一次成本极低，漏拦则会白费一次上游尝试，并把客户端参数错误记成上游错误。
func (a *Adapter) DecodeRequest(body []byte) (*domain.Request, error) {
	var wire wireRequest
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, invalidRequest("请求体不是合法的 JSON", err)
	}
	if wire.Model == "" {
		return nil, invalidRequest("缺少 model", nil)
	}

	req := &domain.Request{
		RequestID: wire.RequestID,
		Protocol:  domain.ProtocolOpenAIResponses,
		Model:     wire.Model,
		Stream:    wire.Stream,
		// RawBody 拷贝一份：调用方会在解码后复用或改写同一块缓冲区，
		// 直接持有会让原始请求体被篡改（协议回退需按原样重放）。
		RawBody: append([]byte(nil), body...),
	}
	if wire.MaxOutputTokens != nil {
		if *wire.MaxOutputTokens < 0 {
			return nil, invalidRequest("max_output_tokens 不得为负", nil)
		}
		req.MaxTokens = *wire.MaxOutputTokens
	}
	if wire.Temperature != nil {
		if *wire.Temperature < 0 || *wire.Temperature > 2 {
			return nil, invalidRequest("temperature 必须在 [0, 2] 区间", nil)
		}
		temperature := *wire.Temperature
		req.Temperature = &temperature
	}

	// instructions 归一化为 system 消息，排在 input 之前。
	if wire.Instructions != "" {
		req.Messages = append(req.Messages, domain.Message{
			Role:  domain.RoleSystem,
			Parts: []domain.Part{{Kind: domain.PartText, Text: wire.Instructions}},
		})
	}
	messages, err := decodeInput(wire.Input)
	if err != nil {
		return nil, err
	}
	req.Messages = append(req.Messages, messages...)
	if len(req.Messages) == 0 {
		return nil, invalidRequest("缺少 input", nil)
	}

	tools, err := decodeTools(wire.Tools)
	if err != nil {
		return nil, err
	}
	req.Tools = tools

	choice, err := decodeToolChoice(wire.ToolChoice)
	if err != nil {
		return nil, err
	}
	req.ToolChoice = choice

	return req, nil
}

// decodeInput 把 input 字段解码为消息列表。input 可以是字符串或条目数组。
func decodeInput(raw json.RawMessage) ([]domain.Message, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == jsonNullLiteral {
		return nil, invalidRequest("缺少 input", nil)
	}
	switch trimmed[0] {
	case '"':
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return nil, invalidRequest("input 字符串不合法", err)
		}
		if text == "" {
			return nil, invalidRequest("input 文本为空", nil)
		}
		return []domain.Message{{
			Role:  domain.RoleUser,
			Parts: []domain.Part{{Kind: domain.PartText, Text: text}},
		}}, nil
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return nil, invalidRequest("input 数组不合法", err)
		}
		if len(items) == 0 {
			return nil, invalidRequest("input 数组为空", nil)
		}
		messages := make([]domain.Message, 0, len(items))
		for i, item := range items {
			message, keep, err := decodeInputItem(item, i)
			if err != nil {
				return nil, err
			}
			if !keep {
				// 未识别的条目类型、角色或全由未知内容组成：跳过该条目并继续。
				continue
			}
			messages = append(messages, message)
		}
		if len(messages) == 0 {
			return nil, invalidRequest("input 数组中没有可解码的条目", nil)
		}
		return messages, nil
	default:
		return nil, invalidRequest("input 必须是字符串或条目数组", nil)
	}
}

// decodeInputItem 解码 input 数组里的一个条目。第二个返回值为 false 表示该条目
// 无法用内部格式表达（未识别的条目类型、角色，或内容全为未识别类型），跳过它。
func decodeInputItem(raw json.RawMessage, index int) (domain.Message, bool, error) {
	var item wireInputItem
	if err := json.Unmarshal(raw, &item); err != nil {
		return domain.Message{}, false, invalidRequest(fmt.Sprintf("input 第 %d 个条目不是合法对象", index), err)
	}
	switch item.Type {
	case itemTypeFunctionCall:
		if item.CallID == "" {
			return domain.Message{}, false, invalidRequest(fmt.Sprintf("input 第 %d 个 function_call 缺少 call_id", index), nil)
		}
		if item.Name == "" {
			return domain.Message{}, false, invalidRequest(fmt.Sprintf("input 第 %d 个 function_call 缺少 name", index), nil)
		}
		return domain.Message{
			Role: domain.RoleAssistant,
			Parts: []domain.Part{{Kind: domain.PartToolCall, ToolCall: &domain.ToolCall{
				ID:        item.CallID,
				Name:      item.Name,
				Arguments: item.Arguments,
			}}},
		}, true, nil
	case itemTypeFunctionCallOutput:
		if item.CallID == "" {
			return domain.Message{}, false, invalidRequest(fmt.Sprintf("input 第 %d 个 function_call_output 缺少 call_id", index), nil)
		}
		content, err := decodeToolOutput(item.Output, index)
		if err != nil {
			return domain.Message{}, false, err
		}
		return domain.Message{
			Role: domain.RoleTool,
			Parts: []domain.Part{{Kind: domain.PartToolResult, ToolResult: &domain.ToolResult{
				ToolCallID: item.CallID,
				Content:    content,
			}}},
		}, true, nil
	case "", itemTypeMessage:
		if item.Role == "" {
			return domain.Message{}, false, invalidRequest(fmt.Sprintf("input 第 %d 个条目缺少 role 或 type", index), nil)
		}
		role, ok := decodeRole(item.Role)
		if !ok {
			// 未识别的角色：跳过该条消息。
			return domain.Message{}, false, nil
		}
		parts, skipped, err := decodeContent(item.Content, index)
		if err != nil {
			return domain.Message{}, false, err
		}
		if len(parts) == 0 {
			if skipped {
				// 内容全为未识别类型：内部表达不了，跳过该条消息。
				return domain.Message{}, false, nil
			}
			// assistant 轮的语义由独立的 function_call 条目承载：
			// 回放工具循环时客户端会在function_call 之前发一条 content 为空的 assistant message 条目。
			// 这是对真实客户端请求体与真实上游实测得出的合法形态
			// （不是公开规范条款，仓库内无可复现的抓包佐证），上游接受它；内部无内容可承载时跳过该条消息，
			// 不得判为客户端错误，否则网关会比上游更严，工具循环在网关处就断、上游根本不会被调用。
			if role == domain.RoleAssistant {
				return domain.Message{}, false, nil
			}
			return domain.Message{}, false, invalidRequest(fmt.Sprintf("input 第 %d 个条目 content 为空", index), nil)
		}
		return domain.Message{Role: role, Parts: parts}, true, nil
	default:
		// 未识别的条目类型（如 reasoning）：跳过它并继续。
		return domain.Message{}, false, nil
	}
}

// decodeToolOutput 解码 function_call_output 的 output。
//
// 字符串原样返回；结构化结果（数组/对象）保留其 JSON 原文，避免丢失内容。
func decodeToolOutput(raw json.RawMessage, index int) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == jsonNullLiteral {
		return "", nil
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return "", invalidRequest(fmt.Sprintf("input 第 %d 个 function_call_output 的 output 不是字符串", index), err)
		}
		return text, nil
	}
	return string(trimmed), nil
}

// decodeRole 把 Responses 的角色字面量映射为内部角色。第二个返回值为 false 表示
// 角色未识别，调用方应跳过该条消息。
//
// developer 是 Responses 中替代 system 的角色，统一归一化为 system。
func decodeRole(role string) (domain.Role, bool) {
	switch role {
	case "user":
		return domain.RoleUser, true
	case "assistant":
		return domain.RoleAssistant, true
	case "system", "developer":
		return domain.RoleSystem, true
	case "tool":
		return domain.RoleTool, true
	default:
		return "", false
	}
}

// decodeContent 把消息 content 解码为片段列表。content 可以是字符串或片段数组。
// 第二个返回值报告是否跳过了未识别类型的内容片段，供上层区分「结构上的空内容」
// 与「只含未识别类型、内部表示不了的内容」。
//
// 结构上的空内容（空字符串、空数组）不算错误，而是返回空片段列表且无错误：
// 空 content 对 message 条目是否合法取决于角色
// （assistant 轮由独立的 function_call条目承载语义，空 content 合法；user/system 轮则非法），
// 角色判断统一由调用方decodeInputItem 完成，本函数不持有角色信息。
func decodeContent(raw json.RawMessage, index int) ([]domain.Part, bool, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == jsonNullLiteral {
		return nil, false, invalidRequest(fmt.Sprintf("input 第 %d 个条目缺少 content", index), nil)
	}
	switch trimmed[0] {
	case '"':
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return nil, false, invalidRequest(fmt.Sprintf("input 第 %d 个条目 content 不合法", index), err)
		}
		if text == "" {
			return nil, false, nil
		}
		return []domain.Part{{Kind: domain.PartText, Text: text}}, false, nil
	case '[':
		var elements []json.RawMessage
		if err := json.Unmarshal(trimmed, &elements); err != nil {
			return nil, false, invalidRequest(fmt.Sprintf("input 第 %d 个条目 content 数组不合法", index), err)
		}
		if len(elements) == 0 {
			return nil, false, nil
		}
		out := make([]domain.Part, 0, len(elements))
		skipped := false
		for j, element := range elements {
			element = bytes.TrimSpace(element)
			// 兼容 content 数组中直接写字符串的写法。
			if len(element) > 0 && element[0] == '"' {
				var text string
				if err := json.Unmarshal(element, &text); err != nil {
					return nil, false, invalidRequest(fmt.Sprintf("input 第 %d 个条目第 %d 个内容不合法", index, j), err)
				}
				if text == "" {
					return nil, false, invalidRequest(fmt.Sprintf("input 第 %d 个条目第 %d 个内容文本为空", index, j), nil)
				}
				out = append(out, domain.Part{Kind: domain.PartText, Text: text})
				continue
			}
			var part wireContentPart
			if err := json.Unmarshal(element, &part); err != nil {
				return nil, false, invalidRequest(fmt.Sprintf("input 第 %d 个条目第 %d 个内容不是合法对象", index, j), err)
			}
			switch part.Type {
			case "input_text", contentTypeOutputText, "text":
				if part.Text == "" {
					return nil, false, invalidRequest(fmt.Sprintf("input 第 %d 个条目第 %d 个内容文本为空", index, j), nil)
				}
				out = append(out, domain.Part{Kind: domain.PartText, Text: part.Text})
			case "input_image":
				url, err := decodeImageURL(part.ImageURL)
				if err != nil {
					return nil, false, invalidRequest(fmt.Sprintf("input 第 %d 个条目第 %d 个 input_image 不合法", index, j), err)
				}
				if url == "" {
					return nil, false, invalidRequest(fmt.Sprintf("input 第 %d 个条目第 %d 个 input_image 缺少 image_url", index, j), nil)
				}
				out = append(out, domain.Part{Kind: domain.PartImage, ImageURL: url})
			default:
				// 未识别的内容类型（如 reasoning、input_audio）：跳过该片段。
				skipped = true
			}
		}
		return out, skipped, nil
	default:
		return nil, false, invalidRequest(fmt.Sprintf("input 第 %d 个条目 content 类型非法", index), nil)
	}
}

// decodeImageURL 兼容 image_url 为字符串或 {"url": "..."} 对象的写法。
func decodeImageURL(raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == jsonNullLiteral {
		return "", nil
	}
	if trimmed[0] == '"' {
		var url string
		if err := json.Unmarshal(trimmed, &url); err != nil {
			return "", err
		}
		return url, nil
	}
	var obj wireImageURL
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return "", err
	}
	return obj.URL, nil
}

// decodeTools 把 Responses 工具定义归一化为内部工具列表。
func decodeTools(tools []wireTool) ([]domain.ToolSpec, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	out := make([]domain.ToolSpec, 0, len(tools))
	for i, tool := range tools {
		if tool.Type != "" && tool.Type != "function" {
			// 未识别的工具类型跳过，不因单个工具定义让整条请求失败。
			continue
		}
		name := tool.Name
		description := tool.Description
		parameters := tool.Parameters
		if tool.Function != nil {
			if name == "" {
				name = tool.Function.Name
			}
			if description == "" {
				description = tool.Function.Description
			}
			if isEmptyJSON(parameters) {
				parameters = tool.Function.Parameters
			}
		}
		if name == "" {
			return nil, invalidRequest(fmt.Sprintf("第 %d 个工具缺少 name", i), nil)
		}
		spec := domain.ToolSpec{Name: name, Description: description}
		if trimmed := bytes.TrimSpace(parameters); len(trimmed) > 0 && string(trimmed) != jsonNullLiteral {
			spec.ParametersJSON = string(trimmed)
		}
		out = append(out, spec)
	}
	return out, nil
}

// decodeToolChoice 把 tool_choice 归一化为内部工具选择。
//
// 字符串形态只允许 none/auto/required；对象形态只支持{"type":"function","name":"…"}
// （指定具体函数）。其余对象形态（如 {"type":"allowed_tools",…}、{"type":"mcp",…}）
// 返回 invalid_request，不得静默退化成空串让上游按默认策略放行工具调用。
func decodeToolChoice(raw json.RawMessage) (domain.ToolChoice, error) {
	trimmed := bytes.TrimSpace(raw)
	if isEmptyJSON(trimmed) {
		return domain.ToolChoice{}, nil
	}
	if trimmed[0] == '"' {
		var value string
		if err := json.Unmarshal(trimmed, &value); err != nil {
			return domain.ToolChoice{}, invalidRequest("tool_choice 不合法", err)
		}
		mode, err := domain.ParseToolChoiceMode(value)
		if err != nil {
			return domain.ToolChoice{}, invalidRequest("tool_choice 不合法", err)
		}
		return domain.ToolChoice{Mode: mode}, nil
	}
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return domain.ToolChoice{}, invalidRequest("tool_choice 不合法", err)
	}
	if obj.Type != functionToolType {
		return domain.ToolChoice{}, invalidRequest(fmt.Sprintf("tool_choice 对象类型 %q 不受支持", obj.Type), nil)
	}
	if strings.TrimSpace(obj.Name) == "" {
		return domain.ToolChoice{}, invalidRequest("tool_choice 对象缺少 name", nil)
	}
	return domain.ToolChoice{Mode: domain.ToolChoiceTool, Name: obj.Name}, nil
}

// wireResponse 是 Responses 响应的线上结构，编解码共用。
type wireResponse struct {
	ID     string           `json:"id"`
	Object string           `json:"object"`
	Model  string           `json:"model"`
	Status string           `json:"status"`
	Output []wireOutputItem `json:"output"`
	Usage  *wireUsage       `json:"usage"`
}

// wireOutputItem 是 output 数组里的一种条目。字段按条目类型取用。
type wireOutputItem struct {
	Type string `json:"type"`
	// ID 是条目对象自身的标识（流式事件里作为 item_id 出现），与函数调用的
	// call_id 不是同一个值：call_id 才是客户端回放工具结果时使用的调用 id。
	ID        string              `json:"id,omitempty"`
	Role      string              `json:"role,omitempty"`
	Content   []wireOutputContent `json:"content,omitempty"`
	CallID    string              `json:"call_id,omitempty"`
	Name      string              `json:"name,omitempty"`
	Arguments string              `json:"arguments,omitempty"`
	Output    string              `json:"output,omitempty"`
	// Summary 是 reasoning 条目的可读汇总文本；其具体形状未在仓库内留有官方快照，属未核实。
	// encrypted_content 是密文，不解密。
	Summary []wireReasoningSummary `json:"summary,omitempty"`
}

// wireReasoningSummary 是 reasoning 条目 summary 数组的一项。
type wireReasoningSummary struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// wireOutputContent 是消息条目的输出内容。
type wireOutputContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// wireUsage 是响应中的用量字段。
//
// 非流式响应与流式 response.completed 帧内的 response.usage 是同一个结构，
// 故本类型同时用于 DecodeResponse/EncodeResponse 与 decodeStreamEnd/EncodeStreamEnd。
type wireUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`

	InputTokensDetails  *inputTokensDetailsWire  `json:"input_tokens_details,omitempty"`
	OutputTokensDetails *outputTokensDetailsWire `json:"output_tokens_details,omitempty"`
}

// inputTokensDetailsWire 是 usage.input_tokens_details。
//
// cached_tokens 是命中缓存的输入 token，已包含在 input_tokens 内；
// cache_write_tokens 是写入缓存的输入 token。两者都不再加进 total_tokens。
type inputTokensDetailsWire struct {
	CachedTokens     int `json:"cached_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
}

// outputTokensDetailsWire 是 usage.output_tokens_details。
//
// reasoning_tokens 是用于推理的输出 token，已包含在 output_tokens 内。
type outputTokensDetailsWire struct {
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}

// decodeUsage 把上游 Responses 用量归一化为内部用量。
//
// 上游未给出 usage（nil）时返回零值，即来源为 domain.UsageSourceUnknown 的
// 「未取得用量」；调用方必须据此走兜底分支，不得按零结算。
//
// cached_tokens 与 reasoning_tokens 已分别包含在 input_tokens 与 output_tokens 内，
// 只映射为内部子项，不再累加进输入输出计数。
//
// 部分兼容上游偶发把子项报得比主计数还大，原样落库会违反「子项不得大于主计数」的
// 检查约束，故在此把子项钳回主计数内（见 Usage.BoundSubitemsToMain）。
func decodeUsage(wire *wireUsage) domain.Usage {
	if wire == nil {
		return domain.Usage{}
	}
	usage := domain.Usage{
		Source:       domain.UsageSourceUpstream,
		InputTokens:  wire.InputTokens,
		OutputTokens: wire.OutputTokens,
	}
	if details := wire.InputTokensDetails; details != nil {
		usage.CacheReadTokens = details.CachedTokens
		usage.CacheWriteTokens = details.CacheWriteTokens
	}
	if details := wire.OutputTokensDetails; details != nil {
		usage.ReasoningTokens = details.ReasoningTokens
	}
	return usage.BoundSubitemsToMain()
}

// encodeUsage 把内部用量映射为 Responses usage。
//
// total_tokens 按输入与输出之和重算；缓存与推理 token 不改变 total_tokens：
// cached_tokens 已包含在 input_tokens 内、reasoning_tokens 已包含在 output_tokens 内，
// 只写入对应的 details 子对象供客户端读取。子项全为零时不输出明细对象。
func encodeUsage(usage domain.Usage) *wireUsage {
	wire := &wireUsage{
		InputTokens:  usage.InputTokens,
		OutputTokens: usage.OutputTokens,
		TotalTokens:  usage.InputTokens + usage.OutputTokens,
	}
	if usage.CacheReadTokens != 0 || usage.CacheWriteTokens != 0 {
		wire.InputTokensDetails = &inputTokensDetailsWire{
			CachedTokens:     usage.CacheReadTokens,
			CacheWriteTokens: usage.CacheWriteTokens,
		}
	}
	if usage.ReasoningTokens != 0 {
		wire.OutputTokensDetails = &outputTokensDetailsWire{ReasoningTokens: usage.ReasoningTokens}
	}
	return wire
}

// EncodeResponse 把内部统一响应编码为 Responses 响应体。
//
// 响应 id 优先取上游标识（同协议透传时客户端看到的就是上游 id），上游未给出时
// 才用本适配器的生成器兜底。
func (a *Adapter) EncodeResponse(resp *domain.Response) ([]byte, error) {
	if resp == nil {
		return nil, domain.NewError(domain.CodeInternal, "响应为空")
	}
	output, err := encodeOutput(resp.Message)
	if err != nil {
		return nil, err
	}
	id := resp.UpstreamRequestID
	if id == "" {
		id = newResponseID()
	}
	wire := wireResponse{
		ID:     id,
		Object: objectResponse,
		Model:  resp.Model,
		Status: statusFromFinish(resp.FinishReason),
		Output: output,
		Usage:  encodeUsage(resp.Usage),
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, domain.NewError(domain.CodeInternal, "编码响应失败").WithCause(err)
	}
	return body, nil
}

// DecodeStreamFrame 把上游 Responses 的一帧流式数据归一化为内部统一分片。
//
// 返回空切片表示该帧不产生分片：纯控制帧（response.created、response.in_progress 等）与空文本增量都归此类。
// 工具调用参数增量即使 delta 为空串也产出分片：空参数是上游真实语义（无参调用），
// 分片携带的 id 与函数名是有内容的事实，丢弃它丢掉的是整个调用。
// response.output_item.added 虽然不直接产生分片，但会缓存函数调用条目里的元数据
// （item_id 与 call_id、函数名与 output_index），供后续参数增量帧回填（参数增量帧只带 item_id）。
// 文本增量、工具调用参数增量与结束帧（response.completed、response.incomplete）各自映射为对应分片；
// response.failed 与 error 事件返回按类型分级的统一错误
// （请求级、凭据、权限与资源类错误归不可重试的上游拒绝，其余可换渠道重试）。
// 未识别事件名按控制帧忽略，避免因上游新增事件而中断转发。
//
// 一帧多载荷：本适配器处理的事件每帧至多产生一个分片——Responses 的并行工具调用
// 由上游拆成多个 output_item.added 与 function_call_arguments.delta 事件，不存在把多个调用塞进同一帧的形态。
// 唯一带数组的载荷是 response.completed / response.incomplete 内嵌的 response.output，它不再产出内容分片：
// 在增量帧已逐条下发的前提下，再产出会与增量内容重复。该前提由本适配器自己保证：参数增量无论 delta 是否为空串
// 都逐条产出分片（见 response.function_call_arguments.delta 分支）。
// 单载荷帧仍按统一契约以长度为 1 的切片返回。
func (a *Adapter) DecodeStreamFrame(event string, data []byte) ([]domain.Chunk, error) {
	name := event
	if name == "" {
		var envelope struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(data, &envelope); err != nil {
			return nil, invalidRequest("流式帧不是合法的 JSON", err)
		}
		name = envelope.Type
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	switch name {
	case "response.output_text.delta":
		var payload wireTextDelta
		if err := json.Unmarshal(data, &payload); err != nil {
			return nil, invalidRequest("文本增量帧不是合法的 JSON", err)
		}
		if payload.Delta == "" {
			return nil, nil
		}
		return []domain.Chunk{{Kind: domain.ChunkTextDelta, TextDelta: payload.Delta}}, nil
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		// 推理文本增量只在解码方向建模；事件名与载荷字段名来自官方 OpenAPI，仓库内无快照，属未核实。
		var payload wireTextDelta
		if err := json.Unmarshal(data, &payload); err != nil {
			return nil, invalidRequest("推理文本增量帧不是合法的 JSON", err)
		}
		if payload.Delta == "" {
			return nil, nil
		}
		return []domain.Chunk{{Kind: domain.ChunkReasoningDelta, TextDelta: payload.Delta}}, nil
	case eventOutputItemAdded:
		var payload wireOutputItemAdded
		if err := json.Unmarshal(data, &payload); err != nil {
			return nil, invalidRequest("output_item.added 帧不是合法的 JSON", err)
		}
		// 只有函数调用条目需要缓存：参数增量帧只有 item_id，函数名与工具调用 id（call_id）
		// 只能从这里记住。条目对象上 id 与 call_id 都会出现，两者都按非空登记同一份元数据：
		// 参数增量按 item_id 关联，仍带 call_id 的历史形态按 call_id 关联。
		if payload.Item.Type == itemTypeFunctionCall {
			if a.decodeCalls == nil {
				a.decodeCalls = make(map[string]toolCallMeta)
			}
			meta := toolCallMeta{index: payload.OutputIndex, name: payload.Item.Name, callID: payload.Item.CallID}
			if payload.Item.ID != "" {
				a.decodeCalls[payload.Item.ID] = meta
			}
			if payload.Item.CallID != "" {
				a.decodeCalls[payload.Item.CallID] = meta
			}
		}
		return nil, nil
	case "response.function_call_arguments.delta":
		var payload wireToolArgumentsDelta
		if err := json.Unmarshal(data, &payload); err != nil {
			return nil, invalidRequest("工具调用参数增量帧不是合法的 JSON", err)
		}
		// delta 为空串时同样产出分片：空参数是上游真实语义（无参调用），
		// 分片携带的 id 与函数名是有内容的事实，不跳过。
		// 本分支是解码侧唯一产出 ChunkToolCallDelta 的位置：置位即代表客户端
		// 确实会收到工具调用分片，与结束原因推断严格对应。
		a.decodeSawToolCall = true
		call := &domain.ToolCall{
			Index:     payload.OutputIndex,
			ID:        payload.CallID,
			Arguments: payload.Delta,
		}
		// 真实上游的参数增量只带 item_id，故先按 item_id 查；
		// 仍带 call_id 的历史形态按 call_id 兜底查。查不到时保留上面的兜底值。
		meta, ok := a.decodeCalls[payload.ItemID]
		if !ok && payload.CallID != "" {
			meta, ok = a.decodeCalls[payload.CallID]
		}
		if ok {
			call.Index = meta.index
			call.Name = meta.name
			// output_item.added 的 item 缺 call_id 时元数据里的 callID 是空串，
			// 命中该元数据不得把本可用的 id 覆写成空串：保留 delta 自带的 call_id
			// （即上面 payload.CallID 的兜底值），否则下游按「缺少 id」拒绝该工具调用。
			if meta.callID != "" {
				call.ID = meta.callID
			}
		}
		return []domain.Chunk{{Kind: domain.ChunkToolCallDelta, ToolCall: call}}, nil
	case "response.completed", "response.incomplete":
		return a.decodeStreamEnd(data)
	case "response.failed":
		return nil, streamFailureError(data, "response.failed")
	case "error":
		return nil, streamFailureError(data, "error")
	default:
		return nil, nil
	}
}

// FinishStream 恒返回空。
//
// OpenAI Responses 的结束信号是真实帧 response.completed 与 response.incomplete，
// DecodeStreamFrame 已在这两类帧内产出携带用量与结束原因的 ChunkStreamEnd；EOF 处
// 不需要也不必再补收尾分片，缺结束帧的流因此仍按截断处理。
func (a *Adapter) FinishStream() []domain.Chunk { return nil }

// decodeStreamEnd 解析 response.completed 与 response.incomplete 的结束载荷，
// 产出一个携带用量与结束原因的流结束分片。
//
// 该帧内嵌的 response.output 数组不再产出内容分片：在增量帧已逐条下发的前提下，
// 重复产出会与增量内容冲突。
//
// 结束原因由状态与「本轮流是否向客户端下发了工具调用分片」共同推断：Responses 没有专门的工具调用结束状态，
// 工具调用由本回合的 function_call 条目表达。状态映射为正常结束（completed → FinishStop）且本轮流确实产出过
// ChunkToolCallDelta 时改判为 domain.FinishToolCalls；长度截断（incomplete → FinishLength）、失败与未知结束原因
// 一律保持原判，不被覆盖。置位只发生在产出分片的参数增量帧（见 DecodeStreamFrame），条目上屏帧不会单独置位，
// 故「判为工具调用结束」与「客户端收到工具调用分片」严格一致。
// 参数增量帧含 delta 为空串的空参数增量：它同样产出分片并置位，空参数调用会随流正常下发、结束原因随之改判工具调用结束。
// 推断依据的回合状态在产出结束分片后清空，下一轮流重新累计。
func (a *Adapter) decodeStreamEnd(data []byte) ([]domain.Chunk, error) {
	var payload wireStreamCreated
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, invalidRequest("结束帧不是合法的 JSON", err)
	}
	reason := finishReasonFromStatus(payload.Response.Status)
	if reason == domain.FinishStop && a.decodeSawToolCall {
		reason = domain.FinishToolCalls
	}
	chunk := domain.Chunk{
		Kind:         domain.ChunkStreamEnd,
		Model:        payload.Response.Model,
		FinishReason: reason,
	}
	// 上游未给出 usage 时仍然携带用量对象，其来源为未取得用量；结算方据此走
	// 兜底分支，不会把缺省填充的零当成真实用量。
	usage := decodeUsage(payload.Response.Usage)
	chunk.Usage = &usage
	// 一轮流结束，清空本轮解码状态（含工具调用推断依据），实例可顺序用于下一轮流。
	a.decodeSawToolCall = false
	a.decodeCalls = nil
	return []domain.Chunk{chunk}, nil
}

// streamFailureError 把上游失败帧转为统一错误。
//
// 分级与另外两个适配器一致：请求级错误（参数/凭据/权限/资源）归不可重试的
// CodeUpstreamRejected，其余归可换渠道重试的 CodeUpstreamUnavailable。
// 错误正文只放进 Detail，不直接作为用户文案，避免把上游内部细节原样透出。
func streamFailureError(data []byte, event string) error {
	var payload wireStreamFailure
	if jsonErr := json.Unmarshal(data, &payload); jsonErr != nil {
		return domain.NewError(domain.CodeUpstreamUnavailable, "上游流式响应失败").
			WithDetail(fmt.Sprintf("%s 帧不是合法的 JSON: %v", event, jsonErr))
	}
	// 上游错误对象的 type 承载分级、code 承载机器码，两者都要看：
	// 只看 code 会漏掉 {"error":{"type":"invalid_request_error"}} 这类不带 code 的形态。
	// 判据统一由 domain.NonRetryableUpstreamErrorType 提供，避免各适配器各写一份而漂移。
	code := domain.CodeUpstreamUnavailable
	if domain.NonRetryableUpstreamErrorType(payload.Code) {
		code = domain.CodeUpstreamRejected
	}
	if payload.Error != nil && (domain.NonRetryableUpstreamErrorType(payload.Error.Type) || domain.NonRetryableUpstreamErrorType(payload.Error.Code)) {
		code = domain.CodeUpstreamRejected
	}
	if payload.Response != nil && payload.Response.Error != nil &&
		(domain.NonRetryableUpstreamErrorType(payload.Response.Error.Type) || domain.NonRetryableUpstreamErrorType(payload.Response.Error.Code)) {
		code = domain.CodeUpstreamRejected
	}
	err := domain.NewError(code, "上游流式响应失败")
	detail := payload.Message
	if detail == "" && payload.Error != nil {
		detail = payload.Error.Message
	}
	if detail == "" && payload.Response != nil && payload.Response.Error != nil {
		detail = payload.Response.Error.Message
	}
	if detail == "" {
		detail = fmt.Sprintf("%s 帧未携带错误信息", event)
	}
	return err.WithDetail(detail)
}

// encodeOutput 把内部消息拆成 Responses 的 output 条目。
//
// 连续的文本片段合并为一个 message 条目；工具调用与工具结果各自成为独立条目，
// 出现时先结束当前 message 条目，以保持片段顺序。
func encodeOutput(message domain.Message) ([]wireOutputItem, error) {
	items := make([]wireOutputItem, 0, len(message.Parts))
	var content []wireOutputContent
	flush := func() {
		if len(content) == 0 {
			return
		}
		items = append(items, wireOutputItem{
			Type:    itemTypeMessage,
			Role:    string(domain.RoleAssistant),
			Content: content,
		})
		content = nil
	}
	for _, part := range message.Parts {
		switch part.Kind {
		case domain.PartText:
			if part.Text == "" {
				return nil, domain.NewError(domain.CodeInternal, "响应文本片段为空")
			}
			content = append(content, wireOutputContent{Type: contentTypeOutputText, Text: part.Text})
		case domain.PartToolCall:
			if part.ToolCall == nil {
				return nil, domain.NewError(domain.CodeInternal, "工具调用片段缺少内容")
			}
			flush()
			items = append(items, wireOutputItem{
				Type:      itemTypeFunctionCall,
				CallID:    part.ToolCall.ID,
				Name:      part.ToolCall.Name,
				Arguments: part.ToolCall.Arguments,
			})
		case domain.PartReasoning:
			// 推理内容不下发：Responses 的 reasoning 条目需要上下文标识，纯文本无法重建，显式跳过。
		case domain.PartToolResult:
			if part.ToolResult == nil {
				return nil, domain.NewError(domain.CodeInternal, "工具结果片段缺少内容")
			}
			flush()
			items = append(items, wireOutputItem{
				Type:   itemTypeFunctionCallOutput,
				CallID: part.ToolResult.ToolCallID,
				Output: part.ToolResult.Content,
			})
		default:
			return nil, domain.NewError(domain.CodeInternal,
				fmt.Sprintf("响应包含无法编码的片段类型 %q", string(part.Kind)))
		}
	}
	flush()
	return items, nil
}

// DecodeResponse 把上游 Responses 响应体归一化为内部统一响应。
//
// 状态字面量按 Responses 语义映射：completed → stop，incomplete → length，
// 其余（含 failed、in_progress 等）一律映射为 domain.FinishUnknown。
// 响应含 function_call 条目时，映射为正常结束（completed → stop）的结果改判为
// domain.FinishToolCalls：Responses 没有专门的工具调用结束状态，工具调用由 output
// 条目表达；长度截断等非正常结束保持原判，不得被覆盖。
func (a *Adapter) DecodeResponse(body []byte) (*domain.Response, error) {
	var wire wireResponse
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, invalidRequest("响应体不是合法的 JSON", err)
	}
	if wire.Model == "" {
		return nil, invalidRequest("响应缺少 model", nil)
	}
	message := domain.Message{Role: domain.RoleAssistant}
	for i, item := range wire.Output {
		switch item.Type {
		case itemTypeMessage:
			for j, content := range item.Content {
				switch content.Type {
				case contentTypeOutputText, "text":
					if content.Text == "" {
						return nil, invalidRequest(fmt.Sprintf("第 %d 个 output 条目第 %d 个内容文本为空", i, j), nil)
					}
					message.Parts = append(message.Parts, domain.Part{Kind: domain.PartText, Text: content.Text})
				default:
					// 未识别的内容类型（含 reasoning 等）：跳过该片段，不把上游的成功响应当成客户端错误。
				}
			}
		case itemTypeFunctionCall:
			if item.CallID == "" || item.Name == "" {
				return nil, invalidRequest(fmt.Sprintf("第 %d 个 output 条目 function_call 缺少 call_id 或 name", i), nil)
			}
			message.Parts = append(message.Parts, domain.Part{Kind: domain.PartToolCall, ToolCall: &domain.ToolCall{
				ID:        item.CallID,
				Name:      item.Name,
				Arguments: item.Arguments,
			}})
		case itemTypeFunctionCallOutput:
			if item.CallID == "" {
				return nil, invalidRequest(fmt.Sprintf("第 %d 个 output 条目 function_call_output 缺少 call_id", i), nil)
			}
			message.Parts = append(message.Parts, domain.Part{Kind: domain.PartToolResult, ToolResult: &domain.ToolResult{
				ToolCallID: item.CallID,
				Content:    item.Output,
			}})
		case itemTypeReasoning:
			// reasoning 条目只在解码方向建模；取不到可读文本（例如只有 encrypted_content）
			// 时跳过，不产出空片段。
			if text := reasoningText(item); text != "" {
				message.Parts = append(message.Parts, domain.Part{Kind: domain.PartReasoning, Text: text})
			}
		default:
			// 未识别的 output 条目类型：跳过该条目，不把上游的成功响应当成客户端错误。
		}
	}
	// 上游未给出 usage 时保持零值，即来源为 domain.UsageSourceUnknown 的
	// 「未取得用量」；调用方必须据此走兜底分支，不得按零结算。
	usage := decodeUsage(wire.Usage)
	finishReason := finishReasonFromStatus(wire.Status)
	if finishReason == domain.FinishStop {
		for _, part := range message.Parts {
			if part.Kind == domain.PartToolCall {
				finishReason = domain.FinishToolCalls
				break
			}
		}
	}
	return &domain.Response{
		Model:             wire.Model,
		Message:           message,
		FinishReason:      finishReason,
		Usage:             usage,
		UpstreamRequestID: wire.ID,
	}, nil
}

// reasoningText 提取 reasoning 条目的可读文本：summary 项与 content 文本按顺序拼接。
// 只有密文载荷（encrypted_content）或取不到任何文本时返回空串，调用方据此跳过。
func reasoningText(item wireOutputItem) string {
	var sb strings.Builder
	for _, summary := range item.Summary {
		sb.WriteString(summary.Text)
	}
	for _, content := range item.Content {
		sb.WriteString(content.Text)
	}
	return sb.String()
}

// statusFromFinish 把内部结束原因映射为 Responses 状态字面量。
//
// 未知结束原因映射为 failed，而不是 incomplete：incomplete 会被反向映射为
// FinishLength（长度截断），会让未知情形被误当成正常的长度结束。
func statusFromFinish(reason domain.FinishReason) string {
	switch reason {
	case domain.FinishStop, domain.FinishToolCalls:
		return statusCompleted
	case domain.FinishLength, domain.FinishContentFilter:
		return statusIncomplete
	default:
		return "failed"
	}
}

// finishReasonFromStatus 把 Responses 状态字面量映射为内部结束原因。
//
// 未识别状态一律返回 domain.FinishUnknown，绝不当成正常结束。
func finishReasonFromStatus(status string) domain.FinishReason {
	switch status {
	case statusCompleted:
		return domain.FinishStop
	case statusIncomplete:
		return domain.FinishLength
	default:
		return domain.FinishUnknown
	}
}

// wireStreamCreated 是 response.created 与 response.completed 事件的载荷。
type wireStreamCreated struct {
	Type     string             `json:"type"`
	Response wireStreamResponse `json:"response"`
}

// wireOutputItemAdded 是 response.output_item.added 事件的载荷。
type wireOutputItemAdded struct {
	Type        string         `json:"type"`
	OutputIndex int            `json:"output_index"`
	Item        wireOutputItem `json:"item"`
}

// wireStreamFailure 是 response.failed 与 error 事件的载荷。
//
// response.failed 把错误放在 response.error 下，error 事件直接放在顶层；
// 两种形态都解析，取到非空文案写入 Detail 供排障。
type wireStreamFailure struct {
	// Type 是事件名（error、response.failed），不承载错误分级，故不参与分级判断。
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Error   *struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Response *struct {
		Error *struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	} `json:"response"`
}

type wireStreamResponse struct {
	// ID 是响应标识。流式帧里的 response 对象就是完整响应对象，id 是必需字段，
	// 缺失会被严格校验的客户端（如 OpenAI SDK）拒绝；同一轮流的各帧共用同一个取值。
	ID     string     `json:"id"`
	Object string     `json:"object"`
	Status string     `json:"status"`
	Model  string     `json:"model,omitempty"`
	Usage  *wireUsage `json:"usage,omitempty"`
}

// wireTextDelta 是 response.output_text.delta 事件的载荷。
//
//  1. ItemID 是条目级标识，与 message 条目的 item.id 一致；
//  2. Model 是对外模型别名，取自分片的 domain.Chunk.Model；
//  3. 为空时不输出该字段。
type wireTextDelta struct {
	Type         string `json:"type"`
	ItemID       string `json:"item_id,omitempty"`
	Delta        string `json:"delta"`
	OutputIndex  int    `json:"output_index"`
	ContentIndex int    `json:"content_index"`
	Model        string `json:"model,omitempty"`
}

// wireToolArgumentsDelta 是 response.function_call_arguments.delta 事件的载荷。
//
// 真实上游的参数增量帧字段为 delta、item_id、output_index、sequence_number、type，
// 不含 call_id 与 name；ItemID 用于关联 output_item.added 缓存的元数据。
// CallID 只兼容仍携带 call_id 的历史形态保留（解码方向），编码方向写条目缓存的 call_id。
// Model 是对外模型别名，取自分片的 domain.Chunk.Model；为空时不输出该字段。
type wireToolArgumentsDelta struct {
	Type        string `json:"type"`
	ItemID      string `json:"item_id,omitempty"`
	Delta       string `json:"delta"`
	CallID      string `json:"call_id,omitempty"`
	OutputIndex int    `json:"output_index"`
	Model       string `json:"model,omitempty"`
}

// wireMessageItem 是 message 条目在 output_item.added 与 output_item.done 帧里的形状。
//
// Content 必须始终写出：added 时是空数组（`[]`）、done 时是单个 output_text 内容项。
// 不得复用 wireOutputItem——它的 `content` 带 `omitempty`，空数组会被省略，
// 而 Responses 客户端要求该字段存在。
type wireMessageItem struct {
	ID      string                  `json:"id"`
	Type    string                  `json:"type"`
	Status  string                  `json:"status"`
	Role    string                  `json:"role"`
	Content []wireOutputTextContent `json:"content"`
}

// wireOutputTextContent 是 message 条目 content 数组里的一个输出文本内容项。
// Annotations 必须写出空数组。
type wireOutputTextContent struct {
	Type        string            `json:"type"`
	Text        string            `json:"text"`
	Annotations []json.RawMessage `json:"annotations"`
}

// wireFunctionCallItem 是 function_call 条目在 output_item.added 与 output_item.done 帧里的形状。
//
// Arguments 必须始终写出：added 时是空串、done 时是该条目累计参数。
// CallID 与 Name 在未知时不写（上游未给出时不得凭空造值）。
type wireFunctionCallItem struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Status    string `json:"status"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

// wireOutputMessageAdded 是 message 条目的 response.output_item.added 帧。
type wireOutputMessageAdded struct {
	Type        string          `json:"type"`
	OutputIndex int             `json:"output_index"`
	Item        wireMessageItem `json:"item"`
}

// wireOutputFunctionCallAdded 是 function_call 条目的 response.output_item.added 帧。
type wireOutputFunctionCallAdded struct {
	Type        string               `json:"type"`
	OutputIndex int                  `json:"output_index"`
	Item        wireFunctionCallItem `json:"item"`
}

// wireOutputItemDone 是两类条目的 response.output_item.done 帧；Item 按条目类型取
// wireMessageItem 或 wireFunctionCallItem。
type wireOutputItemDone struct {
	Type        string `json:"type"`
	OutputIndex int    `json:"output_index"`
	Item        any    `json:"item"`
}

// wireOutputTextPart 是 message 条目里 output_text 内容分片的形状，
// 用于 response.content_part.added 与 response.content_part.done。Annotations 必须写出空数组。
type wireOutputTextPart struct {
	Type        string            `json:"type"`
	Text        string            `json:"text"`
	Annotations []json.RawMessage `json:"annotations"`
}

// wireContentPartEvent 是 response.content_part.added 与 response.content_part.done 事件的载荷。
type wireContentPartEvent struct {
	Type         string             `json:"type"`
	ItemID       string             `json:"item_id"`
	OutputIndex  int                `json:"output_index"`
	ContentIndex int                `json:"content_index"`
	Part         wireOutputTextPart `json:"part"`
}

// wireTextDone 是 response.output_text.done 事件的载荷，Text 为该条目累计文本。
type wireTextDone struct {
	Type         string `json:"type"`
	ItemID       string `json:"item_id"`
	OutputIndex  int    `json:"output_index"`
	ContentIndex int    `json:"content_index"`
	Text         string `json:"text"`
}

// wireToolArgumentsDone 是 response.function_call_arguments.done 事件的载荷，
// Arguments 为该条目累计参数。
type wireToolArgumentsDone struct {
	Type        string `json:"type"`
	ItemID      string `json:"item_id"`
	CallID      string `json:"call_id,omitempty"`
	OutputIndex int    `json:"output_index"`
	Arguments   string `json:"arguments"`
}

// EncodeStreamStart 返回流开始帧（response.created）。
//
// 调用方在转发首个分片前调用一次。首次调用生成一个流级响应 id 并记住，重复调用沿用同一个 id
// （返回帧因此相同）；该 id 会被 EncodeStreamEnd 的结束帧共用，并在流结束时清空。
// Responses 的开始帧不带模型名，模型名随增量帧自带，本方法忽略入参。
func (a *Adapter) EncodeStreamStart(string) ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.streamResponseID == "" {
		a.streamResponseID = newResponseID()
	}
	return encodeFrameE("response.created", wireStreamCreated{
		Type:     "response.created",
		Response: wireStreamResponse{ID: a.streamResponseID, Object: objectResponse, Status: statusInProgress},
	})
}

// EncodeChunk 把一个内部统一分片编码为 Responses 的 SSE 帧。
//
// 按内部契约只处理 ChunkTextDelta 与 ChunkToolCallDelta；用量、
// 结束原因与流结束必须经 EncodeStreamEnd 下发，其余分片返回错误而非空帧（空帧会破坏 SSE 帧结构）。
//
// 跨协议重建路径上，文本与工具增量都必须归属于一个已上屏的条目，因此本方法会在
// 需要时补齐条目生命周期帧（added／content_part.added／delta／.done）；返回值
// 允许拼接多帧。
func (a *Adapter) EncodeChunk(chunk domain.Chunk) ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	switch chunk.Kind {
	case domain.ChunkTextDelta:
		return a.encodeTextDelta(chunk)
	case domain.ChunkReasoningDelta:
		// 推理增量在编码方向不下发，显式跳过而不报错。
		return nil, nil
	case domain.ChunkToolCallDelta:
		return a.encodeToolCallDelta(chunk)
	default:
		return nil, domain.NewError(domain.CodeInternal,
			fmt.Sprintf("分片类型 %q 不能经 EncodeChunk 下发：用量、结束原因与流结束只经 EncodeStreamEnd", string(chunk.Kind)))
	}
}

// encodeTextDelta 编码文本增量，并在需要时补齐 message 条目的生命周期帧。
//
// 文本增量到达时若当前没有打开的 message 条目
// （含上一个 message 条目已关闭、或正打开着 function_call 条目），先关闭当前打开的条目、
// 再新开一个 message 条目：因此「工具调用之后又出现文本」形成第二个 message 条目
// （新的 output_index、新的条目 id），而不是复用已关闭的条目。
// 新开条目的 output_item.added 与content_part.added 两帧与触发它的首个文本增量由同一次返回一并给出。
// 空文本增量（TextDelta 为空串）不开启条目、不产帧，直接返回空。
func (a *Adapter) encodeTextDelta(chunk domain.Chunk) ([]byte, error) {
	// 空文本增量一律丢弃：不开 message 条目、不产出 delta 帧、不占用 output_index。
	if chunk.TextDelta == "" {
		return nil, nil
	}
	var frames []byte
	if a.openItem == nil || a.openItem.kind != itemTypeMessage {
		closed, err := a.closeOpenItem()
		if err != nil {
			return nil, err
		}
		opened, err := a.openMessageItem()
		if err != nil {
			return nil, err
		}
		frames = append(append(frames, closed...), opened...)
	}
	a.openItem.text += chunk.TextDelta
	delta, err := encodeFrameE("response.output_text.delta", wireTextDelta{
		Type:         "response.output_text.delta",
		ItemID:       a.openItem.id,
		Delta:        chunk.TextDelta,
		OutputIndex:  a.openItem.outputIndex,
		ContentIndex: 0,
		Model:        chunk.Model,
	})
	if err != nil {
		return nil, err
	}
	return append(frames, delta...), nil
}

// openMessageItem 新开一个 message 条目并给出它的上屏两帧。
//
// output_index 由网关按首次上屏顺序分配；content 与 part.annotations 都显式写出
// 空数组而不是缺省（Responses 客户端要求这两个字段存在）。
func (a *Adapter) openMessageItem() ([]byte, error) {
	item := &encodeItem{
		kind:        itemTypeMessage,
		id:          newItemID(itemIDPrefixMessage),
		outputIndex: a.nextOutputIndex,
	}
	a.nextOutputIndex++
	a.openItem = item

	added, err := encodeFrameE(eventOutputItemAdded, wireOutputMessageAdded{
		Type:        eventOutputItemAdded,
		OutputIndex: item.outputIndex,
		Item: wireMessageItem{
			ID:      item.id,
			Type:    itemTypeMessage,
			Status:  statusInProgress,
			Role:    string(domain.RoleAssistant),
			Content: []wireOutputTextContent{},
		},
	})
	if err != nil {
		return nil, err
	}
	part, err := encodeFrameE("response.content_part.added", wireContentPartEvent{
		Type:         "response.content_part.added",
		ItemID:       item.id,
		OutputIndex:  item.outputIndex,
		ContentIndex: 0,
		Part: wireOutputTextPart{
			Type:        contentTypeOutputText,
			Text:        "",
			Annotations: []json.RawMessage{},
		},
	})
	if err != nil {
		return nil, err
	}
	return append(added, part...), nil
}

// closeOpenItem 关闭当前打开的条目，给出它的 .done 帧；没有打开的条目时返回空。
//
// 关闭顺序与上屏顺序对称：
//   - message 条目：依次给出 output_text.done、content_part.done、output_item.done
//   - function_call 条目：依次给出 function_call_arguments.done、output_item.done
func (a *Adapter) closeOpenItem() ([]byte, error) {
	item := a.openItem
	if item == nil {
		return nil, nil
	}
	a.openItem = nil
	switch item.kind {
	case itemTypeMessage:
		return closeMessageItem(item)
	case itemTypeFunctionCall:
		item.done = true
		return closeFunctionCallItem(item)
	default:
		return nil, domain.NewError(domain.CodeInternal, fmt.Sprintf("条目类型 %q 无法关闭", item.kind))
	}
}

// closeMessageItem 给出 message 条目的三帧关闭帧，累计文本原样写入 .done 载荷。
func closeMessageItem(item *encodeItem) ([]byte, error) {
	textDone, err := encodeFrameE("response.output_text.done", wireTextDone{
		Type:         "response.output_text.done",
		ItemID:       item.id,
		OutputIndex:  item.outputIndex,
		ContentIndex: 0,
		Text:         item.text,
	})
	if err != nil {
		return nil, err
	}
	partDone, err := encodeFrameE("response.content_part.done", wireContentPartEvent{
		Type:         "response.content_part.done",
		ItemID:       item.id,
		OutputIndex:  item.outputIndex,
		ContentIndex: 0,
		Part: wireOutputTextPart{
			Type:        contentTypeOutputText,
			Text:        item.text,
			Annotations: []json.RawMessage{},
		},
	})
	if err != nil {
		return nil, err
	}
	itemDone, err := encodeFrameE("response.output_item.done", wireOutputItemDone{
		Type:        "response.output_item.done",
		OutputIndex: item.outputIndex,
		Item: wireMessageItem{
			ID:     item.id,
			Type:   itemTypeMessage,
			Status: statusCompleted,
			Role:   string(domain.RoleAssistant),
			Content: []wireOutputTextContent{{
				Type:        contentTypeOutputText,
				Text:        item.text,
				Annotations: []json.RawMessage{},
			}},
		},
	})
	if err != nil {
		return nil, err
	}
	frames := append(textDone, partDone...)
	return append(frames, itemDone...), nil
}

// closeFunctionCallItem 给出 function_call 条目的两帧关闭帧，累计参数原样写入 .done 载荷。
func closeFunctionCallItem(item *encodeItem) ([]byte, error) {
	argumentsDone, err := encodeFrameE("response.function_call_arguments.done", wireToolArgumentsDone{
		Type:        "response.function_call_arguments.done",
		ItemID:      item.id,
		CallID:      item.callID,
		OutputIndex: item.outputIndex,
		Arguments:   item.arguments,
	})
	if err != nil {
		return nil, err
	}
	itemDone, err := encodeFrameE("response.output_item.done", wireOutputItemDone{
		Type:        "response.output_item.done",
		OutputIndex: item.outputIndex,
		Item: wireFunctionCallItem{
			ID:        item.id,
			Type:      itemTypeFunctionCall,
			Status:    statusCompleted,
			CallID:    item.callID,
			Name:      item.name,
			Arguments: item.arguments,
		},
	})
	if err != nil {
		return nil, err
	}
	return append(argumentsDone, itemDone...), nil
}

// encodeToolCallDelta 编码工具调用参数增量。
//
// Responses 要求先有 response.output_item.added（条目类型 function_call），参数增量
// 才有归属，故必须在该内部下标的第一个分片前补发 added。内部参数增量分片又不保证
// 首个就带函数名，因此两种情形都要处理：
//   - 首个分片带 name：直接发出带 name 的 added；
//   - 首个分片不带 name：先发出不带 name 的 added 以保证分片归属正确，待后续分片
//     首次带上 name 时再补发一次带 name 的 added（沿用同一条目 id 与同一 output_index），
//     避免函数名晚到就永久丢失。
//
// 工具调用参数增量按内部 domain.ToolCall.Index 一对一归属条目：该下标首次出现时新开
// 条目（先关闭当前打开的条目），其后该下标的增量追加到同一条目。已关闭的条目再次
// 收到同一内部下标的增量时只给出 delta，不重发 added、不重发 done，已给出的 .done
// 载荷不因后到的增量改写。
func (a *Adapter) encodeToolCallDelta(chunk domain.Chunk) ([]byte, error) {
	var index int
	var callID, name, arguments string
	if chunk.ToolCall != nil {
		index = chunk.ToolCall.Index
		callID = chunk.ToolCall.ID
		name = chunk.ToolCall.Name
		arguments = chunk.ToolCall.Arguments
	}

	item := a.callItems[index]
	if item == nil {
		return a.openFunctionCallItem(index, callID, name, arguments, chunk.Model)
	}

	// 已有条目：补全调用 id（分片不保证首个就带 call_id）。
	if callID != "" && item.callID == "" {
		item.callID = callID
	}
	if item.done {
		return encodeFunctionArgumentsDelta(item, arguments, chunk.Model)
	}

	var frames []byte
	// 函数名晚到：沿用同一条目 id 与同一 output_index 补发一次带 name 的 added。
	if name != "" && (!item.named || item.name != name) {
		item.name = name
		item.named = true
		added, err := encodeFunctionCallAdded(item)
		if err != nil {
			return nil, err
		}
		frames = append(frames, added...)
	}
	delta, err := encodeFunctionArgumentsDelta(item, arguments, chunk.Model)
	if err != nil {
		return nil, err
	}
	item.arguments += arguments
	return append(frames, delta...), nil
}

// openFunctionCallItem 新开一个 function_call 条目（先关闭当前打开的条目），
// 给出它的上屏帧与触发它的首个参数增量。
func (a *Adapter) openFunctionCallItem(index int, callID, name, arguments, model string) ([]byte, error) {
	closed, err := a.closeOpenItem()
	if err != nil {
		return nil, err
	}
	item := &encodeItem{
		kind:        itemTypeFunctionCall,
		id:          newItemID(itemIDPrefixFunctionCall),
		outputIndex: a.nextOutputIndex,
	}
	a.nextOutputIndex++
	if a.callItems == nil {
		a.callItems = make(map[int]*encodeItem)
	}
	a.callItems[index] = item
	a.openItem = item
	if callID != "" {
		item.callID = callID
	}
	if name != "" {
		item.name = name
		item.named = true
	}
	added, err := encodeFunctionCallAdded(item)
	if err != nil {
		return nil, err
	}
	delta, err := encodeFunctionArgumentsDelta(item, arguments, model)
	if err != nil {
		return nil, err
	}
	item.arguments += arguments
	return append(append(closed, added...), delta...), nil
}

// encodeFunctionCallAdded 给出一个 function_call 条目的上屏帧。
//
// `arguments` 必须是空串而不是缺省：条目参数随后的 delta 追加，上屏时不携带内容。
func encodeFunctionCallAdded(item *encodeItem) ([]byte, error) {
	return encodeFrameE(eventOutputItemAdded, wireOutputFunctionCallAdded{
		Type:        eventOutputItemAdded,
		OutputIndex: item.outputIndex,
		Item: wireFunctionCallItem{
			ID:        item.id,
			Type:      itemTypeFunctionCall,
			Status:    statusInProgress,
			CallID:    item.callID,
			Name:      item.name,
			Arguments: "",
		},
	})
}

// encodeFunctionArgumentsDelta 给出一个工具条目的参数增量帧，沿用条目的
// item_id、上游工具调用 id 与网关分配的下标。
func encodeFunctionArgumentsDelta(item *encodeItem, arguments, model string) ([]byte, error) {
	return encodeFrameE("response.function_call_arguments.delta", wireToolArgumentsDelta{
		Type:        "response.function_call_arguments.delta",
		ItemID:      item.id,
		Delta:       arguments,
		CallID:      item.callID,
		OutputIndex: item.outputIndex,
		Model:       model,
	})
}

// EncodeStreamEnd 返回流结束帧。
//
// 分片携带的用量与结束原因写入 response 对象：结束原因决定 status，用量回填 usage，
// 模型名回填 model；未携带用量时省略 usage 字段。事件名与 status 保持一致：
// completed 配 response.completed、incomplete 配response.incomplete、其余（含未知结束原因）
// 配 response.failed，避免出现「事件名说完成、status 说失败」的自相矛盾帧。
//
// 给出结束帧之前先关闭仍打开的条目（实现上至多一个），把它们的三／两帧 .done 帧
// 拼接在结束帧之前；没有打开的条目时返回值仍以结束帧开头。
func (a *Adapter) EncodeStreamEnd(chunk domain.Chunk) ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	// 结束帧与开始帧共用同一个流级响应 id；未先调用 EncodeStreamStart 时兜底生成，
	// 保证 response.failed 等帧同样带上非空 id。
	id := a.streamResponseID
	if id == "" {
		id = newResponseID()
	}
	closed, err := a.closeOpenItem()
	if err != nil {
		return nil, err
	}
	// 一轮流结束，清空流式状态（含流级响应 id、条目状态与工具调用推断依据），实例可顺序用于下一轮流。
	a.streamResponseID = ""
	a.nextOutputIndex = 0
	a.callItems = nil
	a.openItem = nil
	a.decodeCalls = nil
	a.decodeSawToolCall = false

	status := statusFromFinish(chunk.FinishReason)
	event := streamEndEvent(status)
	wire := wireStreamResponse{
		ID:     id,
		Object: objectResponse,
		Status: status,
		Model:  chunk.Model,
	}
	if chunk.Usage != nil {
		wire.Usage = encodeUsage(*chunk.Usage)
	}
	end, err := encodeFrameE(event, wireStreamCreated{
		Type:     event,
		Response: wire,
	})
	if err != nil {
		return nil, err
	}
	return append(closed, end...), nil
}

// streamEndEvent 返回与结束状态匹配的 SSE 事件名。
func streamEndEvent(status string) string {
	switch status {
	case statusCompleted:
		return "response.completed"
	case statusIncomplete:
		return "response.incomplete"
	default:
		return "response.failed"
	}
}

// encodeFrameE 把载荷编码为 `event: <name>\ndata: <json>\n\n` 格式的 SSE 帧。
func encodeFrameE(event string, payload any) ([]byte, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, domain.NewError(domain.CodeInternal, "编码流式帧失败").WithCause(err)
	}
	frame := make([]byte, 0, len(event)+len(data)+sseFrameOverheadBytes)
	frame = append(frame, "event: "...)
	frame = append(frame, event...)
	frame = append(frame, "\ndata: "...)
	frame = append(frame, data...)
	frame = append(frame, "\n\n"...)
	return frame, nil
}

// wireError 是 Responses 错误响应体。
type wireError struct {
	Error wireErrorBody `json:"error"`
}

type wireErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

// EncodeError 把错误编码为 HTTP 状态码与 Responses 错误响应体。
func (a *Adapter) EncodeError(err error) (int, []byte) {
	status := domain.HTTPStatus(err)
	code := domain.CodeInternal
	message := "内部错误"
	if domainErr := domain.AsError(err); domainErr != nil {
		code = domainErr.Code
		if domainErr.Message != "" {
			message = domainErr.Message
		}
	}
	body, marshalErr := json.Marshal(wireError{Error: wireErrorBody{
		Message: message,
		Type:    errorType(code),
		Code:    string(code),
	}})
	if marshalErr != nil {
		return status, []byte(`{"error":{"message":"内部错误","type":"api_error","code":"internal"}}`)
	}
	return status, body
}

// errorType 返回错误码对应的 Responses 错误类型。
func errorType(code domain.Code) string {
	switch code {
	case domain.CodeInvalidRequest, domain.CodeModelNotFound:
		return "invalid_request_error"
	case domain.CodeUnauthorized:
		return "authentication_error"
	case domain.CodeForbidden:
		return "permission_error"
	case domain.CodeRateLimited:
		return "rate_limit_error"
	default:
		return "api_error"
	}
}

// invalidRequest 构造 domain.CodeInvalidRequest 错误。
func invalidRequest(message string, cause error) *domain.Error {
	err := domain.NewError(domain.CodeInvalidRequest, message)
	if cause != nil {
		return err.WithCause(cause)
	}
	return err
}

// isEmptyJSON 报告 JSON 原文是否缺失或为 null。
func isEmptyJSON(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || string(trimmed) == jsonNullLiteral
}
