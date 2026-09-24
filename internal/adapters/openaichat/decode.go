package openaichat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/sumwai/nova/internal/domain"
)

// DecodeRequest 把 OpenAI Chat Completions 请求体解码为内部统一请求。
//
// 只做协议形状校验：
//   - 非法 JSON、已知类型缺少必填字段（model、messages、片段必填项）返回 domain.CodeInvalidRequest
//   - 未识别的角色与内容片段类型跳过，跳过全部消息时仍返回 CodeInvalidRequest
//
// temperature 区间（[0, 2]）与 max_tokens / max_completion_tokens 非负在这里先拦一次
// （同一判据在 domain.Request.Validate 中还有一份，该函数由共享转发流水线调用），
// 与domain.Request.Validate 构成有意的双点校验：适配器负责「它解析的字段它自己拦」，Validate 负责
// 「归一化后的请求整体拦」，两处状态码同为 400。适配器这一份长期保留，不为「消除重复」删除：
// 早拦一次成本极低，漏拦则会白费一次上游尝试，并把客户端参数错误记成上游错误。
//
// max_completion_tokens 是 max_tokens 的替代字段，新模型只支持它。两个字段都映射进内部
// Request.MaxTokens：max_tokens 存在时以它为准，否则用 max_completion_tokens。两者同时
// 给出时不报错，交由上游判定，网关不额外发明拒绝。
// request_id 若请求体自带则沿用，否则留空由调用方注入。
func (a *Adapter) DecodeRequest(body []byte) (*domain.Request, error) {
	var wire requestWire
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, invalidRequest("请求体不是合法 JSON").WithCause(err)
	}
	if strings.TrimSpace(wire.Model) == "" {
		return nil, invalidRequest("缺少 model")
	}
	if len(wire.Messages) == 0 {
		return nil, invalidRequest("缺少 messages")
	}
	if wire.MaxTokens != nil && *wire.MaxTokens < 0 {
		return nil, invalidRequest("max_tokens 不得为负")
	}
	if wire.MaxCompletionTokens != nil && *wire.MaxCompletionTokens < 0 {
		return nil, invalidRequest("max_completion_tokens 不得为负")
	}
	if wire.Temperature != nil && (*wire.Temperature < 0 || *wire.Temperature > 2) {
		return nil, invalidRequest("temperature 必须在 [0, 2] 区间")
	}

	messages := make([]domain.Message, 0, len(wire.Messages))
	for i, raw := range wire.Messages {
		message, keep, err := decodeMessage(raw)
		if err != nil {
			return nil, invalidRequest(fmt.Sprintf("第 %d 条消息：%s", i, err)).WithCause(err)
		}
		if !keep {
			// 未识别的角色或全由未知类型片段构成的消息：跳过该条并继续，避免未建模的类型让整条请求被拒。
			continue
		}
		messages = append(messages, message)
	}
	if len(messages) == 0 {
		return nil, invalidRequest("messages 中没有可解码的消息")
	}

	tools, err := decodeTools(wire.Tools)
	if err != nil {
		return nil, invalidRequest(err.Error()).WithCause(err)
	}
	toolChoice, err := decodeToolChoice(wire.ToolChoice)
	if err != nil {
		return nil, invalidRequest("tool_choice 取值非法").WithCause(err)
	}

	req := &domain.Request{
		RequestID:  wire.RequestID,
		Protocol:   domain.ProtocolOpenAIChat,
		Model:      wire.Model,
		Messages:   messages,
		Tools:      tools,
		ToolChoice: toolChoice,
		Stream:     wire.Stream,
		// RawBody 必须是解码时的一份拷贝，不能别名调用方传入的缓冲：
		// 调用方可能在解码后复用或改写 body（例如按原协议重放竞态）。
		RawBody: append([]byte(nil), body...),
	}
	// max_tokens 存在时以它为准，否则用 max_completion_tokens（见 DecodeRequest 的说明）。
	if wire.MaxTokens != nil {
		req.MaxTokens = *wire.MaxTokens
	} else if wire.MaxCompletionTokens != nil {
		req.MaxTokens = *wire.MaxCompletionTokens
	}
	req.Temperature = wire.Temperature
	return req, nil
}

func invalidRequest(message string) *domain.Error {
	return domain.NewError(domain.CodeInvalidRequest, message)
}

// decodeMessage 解码一条消息。第二个返回值为 false 表示该消息无法用内部格式表达
// （未识别的角色，或内容片段与工具调用全为未识别类型），调用方应跳过它并继续解码，
// 而不是把整条请求判为失败。
func decodeMessage(w requestMessageWire) (domain.Message, bool, error) {
	role := domain.Role(w.Role)
	if w.Role == roleDeveloper {
		// OpenAI Chat 用 developer 承载系统提示，映射为内部 system（与 Responses 适配器一致）。
		role = domain.RoleSystem
	}
	switch role {
	case domain.RoleSystem, domain.RoleUser:
		parts, skipped, err := decodeContentParts(w.Content)
		if err != nil {
			return domain.Message{}, false, err
		}
		return messageWithParts(role, parts, skipped)
	case domain.RoleAssistant:
		parts, skipped, err := decodeContentParts(w.Content)
		if err != nil {
			return domain.Message{}, false, err
		}
		for i, call := range w.ToolCalls {
			part, keep, callErr := decodeToolCall(i, call)
			if callErr != nil {
				return domain.Message{}, false, callErr
			}
			if keep {
				parts = append(parts, part)
				continue
			}
			// 未识别的工具调用被跳过，与 content 里跳过未识别片段同一口径：
			// 若这条消息因此一条片段也不剩，应整条跳过，而不是判为结构上的空消息。
			skipped = true
		}
		return messageWithParts(role, parts, skipped)
	case domain.RoleTool:
		if w.ToolCallID == "" {
			return domain.Message{}, false, errors.New("tool 消息缺少 tool_call_id")
		}
		content, err := decodeTextContent(w.Content)
		if err != nil {
			return domain.Message{}, false, err
		}
		result := domain.ToolResult{ToolCallID: w.ToolCallID, Content: content}
		return domain.Message{Role: role, Parts: []domain.Part{{Kind: domain.PartToolResult, ToolResult: &result}}}, true, nil
	default:
		// 未识别的角色（含已废弃的 function）跳过该条消息。
		return domain.Message{}, false, nil
	}
}

// messageWithParts 组装消息。skipped 表示该消息存在被跳过的未识别类型
// （content 数组中的片段，或 tool_calls 中的调用类型）：此时片段为空说明该消息只含
// 内部表示不了的内容，应整条跳过而不是报错；没有未识别类型却为空才是结构上的空消息，
// 仍要拒绝。
func messageWithParts(role domain.Role, parts []domain.Part, skipped bool) (domain.Message, bool, error) {
	if len(parts) == 0 {
		if skipped {
			return domain.Message{}, false, nil
		}
		return domain.Message{}, false, errors.New("消息内容为空")
	}
	return domain.Message{Role: role, Parts: parts}, true, nil
}

// decodeToolCall 解码一个工具调用。第二个返回值为 false 表示工具类型未识别，跳过它。
func decodeToolCall(index int, call requestToolCallWire) (domain.Part, bool, error) {
	if call.Type != "" && call.Type != toolTypeFunction {
		return domain.Part{}, false, nil
	}
	if call.ID == "" {
		return domain.Part{}, false, fmt.Errorf("第 %d 个工具调用缺少 id", index)
	}
	if call.Function.Name == "" {
		return domain.Part{}, false, fmt.Errorf("第 %d 个工具调用缺少 function.name", index)
	}
	result := domain.ToolCall{ID: call.ID, Name: call.Function.Name, Arguments: call.Function.Arguments}
	return domain.Part{Kind: domain.PartToolCall, ToolCall: &result}, true, nil
}

// decodeContentParts 解析 messages[].content：字符串，或 text / image_url 片段数组。
// 缺失或 null 返回空片段（由上层判断消息是否为空）。第二个返回值报告是否跳过了未识别类型的片段，
// 供上层区分「结构上的空消息」与「只含未识别类型片段、内部表示不了的消息」。
func decodeContentParts(raw json.RawMessage) ([]domain.Part, bool, error) {
	if isAbsent(raw) {
		return nil, false, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if text == "" {
			return nil, false, nil
		}
		return []domain.Part{{Kind: domain.PartText, Text: text}}, false, nil
	}
	var items []contentPartWire
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, false, errors.New("content 必须是字符串或片段数组")
	}
	parts := make([]domain.Part, 0, len(items))
	skipped := false
	for i, item := range items {
		part, keep, err := item.toDomain()
		if err != nil {
			return nil, false, fmt.Errorf("第 %d 个内容片段：%w", i, err)
		}
		if !keep {
			skipped = true
			continue
		}
		parts = append(parts, part)
	}
	return parts, skipped, nil
}

// toDomain 把内容片段归一化为内部片段。第二个返回值为 false 表示片段类型未识别，
// 跳过该片段；已知类型缺少必填字段仍返回错误。
func (c contentPartWire) toDomain() (domain.Part, bool, error) {
	switch c.Type {
	case contentPartText:
		if c.Text == "" {
			return domain.Part{}, false, errors.New("文本片段内容为空")
		}
		return domain.Part{Kind: domain.PartText, Text: c.Text}, true, nil
	case contentPartImageURL:
		url, err := decodeImageURL(c.ImageURL)
		if err != nil {
			return domain.Part{}, false, err
		}
		return domain.Part{Kind: domain.PartImage, ImageURL: url}, true, nil
	default:
		return domain.Part{}, false, nil
	}
}

// decodeImageURL 解析 image_url 字段：既支持 {"url": "..."} 对象，也支持直接给字符串。
func decodeImageURL(raw json.RawMessage) (string, error) {
	if isAbsent(raw) {
		return "", errors.New("图片片段缺少 image_url")
	}
	var url string
	if err := json.Unmarshal(raw, &url); err == nil {
		if url == "" {
			return "", errors.New("图片片段缺少 image_url")
		}
		return url, nil
	}
	var object struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(raw, &object); err != nil {
		return "", errors.New("image_url 取值非法")
	}
	if object.URL == "" {
		return "", errors.New("图片片段缺少 image_url.url")
	}
	return object.URL, nil
}

// decodeTextContent 解析 tool 消息的 content：字符串，或 text 片段数组（拼接为文本）。
func decodeTextContent(raw json.RawMessage) (string, error) {
	if isAbsent(raw) {
		return "", nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	parts, _, err := decodeContentParts(raw)
	if err != nil {
		return "", err
	}
	var builder strings.Builder
	for _, part := range parts {
		if part.Kind != domain.PartText {
			return "", errors.New("tool 消息内容只能是文本")
		}
		builder.WriteString(part.Text)
	}
	return builder.String(), nil
}

func decodeTools(rawTools []requestToolWire) ([]domain.ToolSpec, error) {
	if len(rawTools) == 0 {
		return nil, nil
	}
	tools := make([]domain.ToolSpec, 0, len(rawTools))
	for i, tool := range rawTools {
		if tool.Type != "" && tool.Type != toolTypeFunction {
			// 未识别的工具类型跳过，不因单个工具定义让整条请求失败。
			continue
		}
		if tool.Function.Name == "" {
			return nil, fmt.Errorf("第 %d 个工具缺少 function.name", i)
		}
		spec := domain.ToolSpec{
			Name:        tool.Function.Name,
			Description: tool.Function.Description,
		}
		if !isAbsent(tool.Function.Parameters) {
			spec.ParametersJSON = string(tool.Function.Parameters)
		}
		tools = append(tools, spec)
	}
	return tools, nil
}

// decodeToolChoice 解析 tool_choice：
//   - 字符串只允许 auto/none/required
//   - 对象形态只支持 {"type":"function","function":{"name":"…"}}，指定具体工具必须用对象形态
//
// 未识别的字符串或对象形态报错，不得静默丢弃客户端的工具选择要求。
func decodeToolChoice(raw json.RawMessage) (domain.ToolChoice, error) {
	if isAbsent(raw) {
		return domain.ToolChoice{}, nil
	}
	trimmed := bytes.TrimSpace(raw)
	switch trimmed[0] {
	case '"':
		var value string
		if err := json.Unmarshal(trimmed, &value); err != nil {
			return domain.ToolChoice{}, errors.New("tool_choice 取值非法")
		}
		mode, err := domain.ParseToolChoiceMode(value)
		if err != nil {
			return domain.ToolChoice{}, err
		}
		return domain.ToolChoice{Mode: mode}, nil
	case '{':
		var obj struct {
			Type     string `json:"type"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		if err := json.Unmarshal(trimmed, &obj); err != nil {
			return domain.ToolChoice{}, errors.New("tool_choice 取值非法")
		}
		if obj.Type != toolTypeFunction {
			return domain.ToolChoice{}, fmt.Errorf("tool_choice 对象类型 %q 不受支持", obj.Type)
		}
		if strings.TrimSpace(obj.Function.Name) == "" {
			return domain.ToolChoice{}, errors.New("tool_choice 对象缺少 function.name")
		}
		return domain.ToolChoice{Mode: domain.ToolChoiceTool, Name: obj.Function.Name}, nil
	default:
		return domain.ToolChoice{}, errors.New("tool_choice 取值非法")
	}
}

func isAbsent(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

// DecodeResponse 把上游 Chat Completions 完整响应体归一化为内部统一响应。
//
// 只取 choices[0]：内部 Response 只表达单个候选，与 EncodeResponse 只写 index 0 对称。
// finish_reason 经 DecodeFinishReason 映射，未识别字面量（含空串）归为 FinishUnknown，
// 不得当作正常结束。usage 只映射 prompt_tokens 与 completion_tokens；其它类别本协议子集不表达。
func (a *Adapter) DecodeResponse(body []byte) (*domain.Response, error) {
	var wire responseWire
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, invalidRequest("上游响应不是合法 JSON").WithCause(err)
	}
	if len(wire.Choices) == 0 {
		return nil, invalidRequest("上游响应缺少 choices")
	}
	message, err := decodeResponseMessage(wire.Choices[0].Message)
	if err != nil {
		return nil, err
	}
	return &domain.Response{
		Model:             wire.Model,
		Message:           message,
		FinishReason:      DecodeFinishReason(wire.Choices[0].FinishReason),
		Usage:             decodeUsage(wire.Usage),
		UpstreamRequestID: wire.ID,
	}, nil
}

// decodeResponseMessage 把上游响应 message 归一化为内部消息：推理内容转为推理片段、
// 文本拼接为文本片段，tool_calls 转为工具调用片段。图片、工具结果等片段不会出现在响应方向。
//
// reasoning_content 不属于官方规范，是部分兼容渠道的既有形态；取不到非空文本时不产出推理片段。
func decodeResponseMessage(w responseMessage) (domain.Message, error) {
	role := domain.Role(w.Role)
	if role == "" {
		role = domain.RoleAssistant
	}
	message := domain.Message{Role: role}
	if w.ReasoningContent != "" {
		message.Parts = append(message.Parts, domain.Part{Kind: domain.PartReasoning, Text: w.ReasoningContent})
	}
	if w.Content != nil && *w.Content != "" {
		message.Parts = append(message.Parts, domain.Part{Kind: domain.PartText, Text: *w.Content})
	}
	for i, call := range w.ToolCalls {
		if call.Type != "" && call.Type != toolTypeFunction {
			// 未识别的工具调用类型跳过，不让整条上游响应解码失败。
			continue
		}
		if call.ID == "" || call.Function.Name == "" {
			return domain.Message{}, invalidRequest(fmt.Sprintf("上游响应第 %d 个工具调用缺少 id 或 function.name", i))
		}
		message.Parts = append(message.Parts, domain.Part{Kind: domain.PartToolCall, ToolCall: &domain.ToolCall{
			ID:        call.ID,
			Name:      call.Function.Name,
			Arguments: call.Function.Arguments,
		}})
	}
	return message, nil
}

// decodeUsage 把上游 usage 归一化为内部用量。
//
// 上游未给出 usage 时返回零值，即来源为 domain.UsageSourceUnknown 的「未取得用量」；
// 调用方必须据此走兜底分支，不得按零结算。
//
// cached_tokens 与 reasoning_tokens 已分别包含在 prompt_tokens 与 completion_tokens 内，
// 只映射为内部子项，不再累加进输入输出计数。
//
// 部分兼容上游偶发把 reasoning_tokens 报得比 completion_tokens 还大，原样落库会违反
// 「子项不得大于主计数」的检查约束，故在此把子项钳回主计数内（见 Usage.BoundSubitemsToMain）。
func decodeUsage(wire *usageWire) domain.Usage {
	if wire == nil {
		return domain.Usage{}
	}
	usage := domain.Usage{
		Source:       domain.UsageSourceUpstream,
		InputTokens:  wire.PromptTokens,
		OutputTokens: wire.CompletionTokens,
	}
	if details := wire.PromptTokensDetails; details != nil {
		usage.CacheReadTokens = details.CachedTokens
		usage.CacheWriteTokens = details.CacheWriteTokens
	}
	if details := wire.CompletionTokensDetails; details != nil {
		usage.ReasoningTokens = details.ReasoningTokens
	}
	return usage.BoundSubitemsToMain()
}

// DecodeStreamFrame 把上游一帧 SSE 数据解码为内部统一分片列表。
//
// event 在 Chat Completions 中不使用（本协议不发命名事件），忽略。
// data 为空白时按心跳忽略（返回空切片）；data 为 [DONE] 时产出一个 ChunkStreamEnd，
// 携带此前缓存的结束原因、用量与模型，并清空累计状态以备下一轮流复用。
//
// OpenAI 兼容上游可能把结束原因、用量与内容放在**同一帧**下发（自建网关与部分渠道的常见形态），
// 故本方法先无条件缓存同帧携带的用量与结束原因，再按内容类型产出分片，避免与内容同帧时静默丢数据。
//
// 同一帧内的载荷按出现顺序全部产出：先 content，再按数组顺序逐个 tool_calls。
// 并行工具调用会把多个 tool_calls 放进同一帧，逐帧只取首个会静默丢数据，
// 故返回切片而非单个分片（见 internal/domain/ports.go 对 DecodeStreamFrame 的约定）。
//
// 结束语义：本协议既有必有的结束信号 choices[0].finish_reason，也有可选的哨兵帧 [DONE]。
// [DONE] 到达时此处直接产出 ChunkStreamEnd；上游整条流都不发 [DONE] 时
// （例如部分兼容上游只在最后一个内容帧上给出 finish_reason，随后是 choices 为空的用量帧，再直接 EOF），
// 由 FinishStream 在 EOF 处补齐。本方法只缓存 finish_reason，
// 绝不在见到它时立刻产出结束分片——用量帧可能晚于 finish_reason 到达，提前产出会丢掉随后到达的用量。
// 两条路径合计只产出一个 ChunkStreamEnd，由实例上的 streamEnded 标志保证。
//
// 上游错误帧（data: {"error":{...}}）绝不静默忽略，并按类型分级：请求级错误
// （参数、凭据、权限与资源类错误）归不可重试的 CodeUpstreamRejected，
// 其余归可重试的CodeUpstreamUnavailable；分级细节见 upstreamStreamError。
// 空 choices 且既无 usage 又无 error 的帧不表达任何内容（例如仅含 id 的探活帧），忽略它不会丢错误
// （错误只出现在 error 字段），故按控制帧忽略。空 choices 且 usage 非空的帧是「只承载用量」的帧
// （客户端通过 stream_options.include_usage索取用量时上游追加）：除缓存用量供结束分片使用外，
// 还产出一个 ChunkUsage 分片，供透传下沉目标识别该帧只承载用量，
// 在客户端未索取用量时丢弃其原始字节。该分片只用于记账，编码方向必须跳过它。
func (a *Adapter) DecodeStreamFrame(event string, data []byte) ([]domain.Chunk, error) {
	_ = event
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, nil
	}
	// 新一轮起始处的复位：本轮已产出过结束分片时，后到的任何数据帧必然属于新一轮（实例顺序复用）
	// 或属重复帧序，先清空累计状态与「本轮已产出结束分片」标志，
	// 使下一轮的 FinishStream 不被上一轮的标志挡住而被误判为截断。
	// 该复位不依赖首帧是否带 role这一形态假设；下面 role 分支的复位只作为
	// 「上一轮未正常结束（streamEnded 仍为假）却直接复用实例」的补充信号。
	if a.streamEnded {
		a.stream = streamState{}
		a.streamEnded = false
	}
	if bytes.Equal(trimmed, []byte(streamDoneMarker)) {
		usage := a.stream.usage
		if usage == nil {
			// 流中没有出现用量帧时仍然携带用量对象，其来源为未取得用量；
			// 结算方据此走兜底分支，不会把缺省填充的零当成真实用量。
			usage = &domain.Usage{}
		}
		chunk := domain.Chunk{
			Kind:         domain.ChunkStreamEnd,
			Model:        a.stream.model,
			Usage:        usage,
			FinishReason: a.stream.finishReason,
		}
		a.stream = streamState{}
		a.streamEnded = true
		return []domain.Chunk{chunk}, nil
	}
	var wire chunkWire
	if err := json.Unmarshal(trimmed, &wire); err != nil {
		return nil, invalidRequest("上游流式帧不是合法 JSON").WithCause(err)
	}
	if wire.Error != nil {
		return nil, upstreamStreamError(wire.Error)
	}
	if len(wire.Choices) == 0 {
		if wire.Usage == nil {
			return nil, nil
		}
		usage := decodeUsage(wire.Usage)
		a.stream.usage = &usage
		if wire.Model != "" {
			a.stream.model = wire.Model
		}
		return []domain.Chunk{{Kind: domain.ChunkUsage, Model: wire.Model, Usage: &usage}}, nil
	}
	choice := wire.Choices[0]
	if choice.Delta.Role != "" {
		// 每条 OpenAI 流的第一帧带 role，以它作为新流开始的补充信号清空累计状态，
		// 使「上一轮未正常结束（streamEnded 仍为假）却直接复用实例」时不会串用上一轮的
		// 结束原因与用量。streamEnded 的常规复位在函数起始处（见上方说明）。
		a.stream = streamState{}
		a.streamEnded = false
	}
	if wire.Model != "" {
		a.stream.model = wire.Model
	}
	// 先无条件缓存同帧携带的用量与结束原因：上游可以在同一帧里同时给出
	// 内容与 usage / finish_reason，若放进下面的内容分支就会整段丢失。
	if wire.Usage != nil {
		usage := decodeUsage(wire.Usage)
		a.stream.usage = &usage
	}
	if choice.FinishReason != nil {
		a.stream.finishReason = DecodeFinishReason(*choice.FinishReason)
		// finish_reason 非 null 即 Chat 的必有结束信号，记下它供 FinishStream 在 EOF 处判定
		// 「完整结束」；这里只记标志，不产出分片（用量帧可能随后到达）。
		a.stream.finishReasonSeen = true
	}

	// 按帧内载荷出现顺序产出：推理、正文，再按数组顺序逐个 tool_calls。
	// 推理与正文同帧时顺序不影响记账（旁路按类别分别拼接），也不影响透传字节（透传只写原始帧）。
	// 并行工具调用会把多个 tool_calls 放进同一帧，全部产出并各带真实 Index，
	// 后续流水线据此区分不同调用（见 internal/domain 对 ToolCall.Index 的守护）。
	chunks := make([]domain.Chunk, 0, len(choice.Delta.ToolCalls)+1)
	if choice.Delta.ReasoningContent != "" {
		chunks = append(chunks, domain.Chunk{Kind: domain.ChunkReasoningDelta, Model: wire.Model, TextDelta: choice.Delta.ReasoningContent})
	}
	if choice.Delta.Content != "" {
		chunks = append(chunks, domain.Chunk{Kind: domain.ChunkTextDelta, Model: wire.Model, TextDelta: choice.Delta.Content})
	}
	for _, call := range choice.Delta.ToolCalls {
		if call.Type != "" && call.Type != toolTypeFunction {
			// 未识别的工具调用类型跳过，与请求侧同一口径。
			continue
		}
		toolCall := &domain.ToolCall{Index: call.Index, ID: call.ID}
		if call.Function != nil {
			toolCall.Name = call.Function.Name
			toolCall.Arguments = call.Function.Arguments
		}
		chunks = append(chunks, domain.Chunk{Kind: domain.ChunkToolCallDelta, Model: wire.Model, ToolCall: toolCall})
	}
	// 无载荷时返回 nil，与「不产生分片」的约定保持同一种形状（见 domain.Adapter 的说明）。
	if len(chunks) == 0 {
		return nil, nil
	}
	return chunks, nil
}

// FinishStream 在上游流到达 EOF 时给出 Chat 的收尾分片。
//
// 上游整条流都不发 [DONE] 时
// （例如部分兼容上游只在最后一个内容帧上给出choices[0].finish_reason，用量帧留在其后，再直接 EOF），
// 本方法在 EOF 处把已缓存的模型名、用量与结束原因合成唯一的 ChunkStreamEnd。
// 用量帧可能晚于 finish_reason 到达，因此不能在见到 finish_reason 时提前产出：
// 那样会丢掉随后到达的用量。
//
// 判据与幂等：
//   - 本轮已经产出过结束分片（[DONE] 路径已置位 streamEnded，或本方法先前已调用过）时
//     返回空，保证两条路径合计只产出一个 ChunkStreamEnd；
//   - 本轮从未观察到 choices[0].finish_reason 就 EOF 时返回空，调用方据此仍判为截断。
func (a *Adapter) FinishStream() []domain.Chunk {
	if a.streamEnded || !a.stream.finishReasonSeen {
		return nil
	}
	usage := a.stream.usage
	if usage == nil {
		// 流中出现了 finish_reason 但没有任何用量帧时仍携带用量对象，其来源为未取得
		// 用量；结算方据此走兜底分支，不会把缺省填充的零当成真实用量。
		usage = &domain.Usage{}
	}
	chunk := domain.Chunk{
		Kind:         domain.ChunkStreamEnd,
		Model:        a.stream.model,
		Usage:        usage,
		FinishReason: a.stream.finishReason,
	}
	a.stream = streamState{}
	a.streamEnded = true
	return []domain.Chunk{chunk}
}

// upstreamStreamError 把上游流式错误体转换为统一错误。
//
// 上游原文只进 Detail，不直接作为面向用户的 Message，与 OpenAI Responses 适配器保持同一口径。
// 上游明确表示请求本身有问题时归为不可重试的 CodeUpstreamRejected：
// 换渠道重试通常仍会失败，不应白白放大上游压力。
func upstreamStreamError(body *errorBody) *domain.Error {
	code := domain.CodeUpstreamUnavailable
	if domain.NonRetryableUpstreamErrorType(body.Type) || domain.NonRetryableUpstreamErrorType(body.Code) {
		code = domain.CodeUpstreamRejected
	}
	detail := fmt.Sprintf("上游错误 type=%q code=%q", body.Type, body.Code)
	if message := strings.TrimSpace(body.Message); message != "" {
		detail += " message=" + message
	}
	return domain.NewError(code, "上游流式响应返回错误").WithDetail(detail)
}
