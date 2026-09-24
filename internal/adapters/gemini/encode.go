package gemini

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/sumwai/nova/internal/domain"
)

// ---------------------------------------------------------------------------
// 流式编码（面向客户端）
// ---------------------------------------------------------------------------

// EncodeStreamStart 返回流开始帧。
//
// Gemini 的 SSE 流没有独立的开始帧：第一帧就是候选内容，因此返回 (nil, nil)。
// 但模型名要写进后续每一帧的 modelVersion，这里把它记在实例上。
func (a *Adapter) EncodeStreamStart(model string) ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.streamStarted {
		return nil, domain.NewError(domain.CodeInternal, "流已经开始，不能重复发送开始帧")
	}
	a.resetStreamLocked()
	a.streamStarted = true
	a.streamModel = model
	return nil, nil
}

// EncodeChunk 把一个内容分片编码为 Gemini 的 SSE 帧。
//
// 必须在 EncodeStreamStart 之后调用。用量与结束原因不由本方法编码：它们随
// ChunkStreamEnd 交给 EncodeStreamEnd，统一发成一个带 finishReason 的终止帧。
// 推理分片在编码方向不下发（与另外两种协议一致），显式跳过而不报错。
func (a *Adapter) EncodeChunk(chunk domain.Chunk) ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.streamStarted {
		return nil, domain.NewError(domain.CodeInternal, "流尚未开始，编码分片前必须调用 EncodeStreamStart")
	}

	switch chunk.Kind {
	case domain.ChunkTextDelta:
		return a.encodeStreamFrame(wireResponse{
			Candidates: []wireCandidate{{
				Content: wireContent{Role: roleModel, Parts: []wirePart{{Text: chunk.TextDelta}}},
			}},
			ModelVersion: a.streamModel,
		})
	case domain.ChunkToolCallDelta:
		if chunk.ToolCall == nil {
			return nil, domain.NewError(domain.CodeInternal, "工具调用分片缺少 ToolCall")
		}
		return a.encodeStreamFrame(wireResponse{
			Candidates: []wireCandidate{{
				Content: wireContent{Role: roleModel, Parts: []wirePart{{FunctionCall: encodeFunctionCall(chunk.ToolCall)}}},
			}},
			ModelVersion: a.streamModel,
		})
	case domain.ChunkReasoningDelta:
		// 推理增量在编码方向不下发，显式跳过而不报错。
		return nil, nil
	default:
		return nil, domain.NewError(domain.CodeInternal, fmt.Sprintf(
			"流式分片类型 %q 不是内容分片；用量与结束原因请通过 EncodeStreamEnd 编码", string(chunk.Kind)))
	}
}

// EncodeStreamEnd 把结束分片编码为 Gemini 的终止帧。
//
// Gemini 的流没有 [DONE] 一类的哨兵帧：终止信号就是候选里的 finishReason，
// 用量随同一帧的 usageMetadata 下发。因此本方法只产出一帧。流未开始或已结束时调用返回错误。
func (a *Adapter) EncodeStreamEnd(chunk domain.Chunk) ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.streamStarted {
		return nil, domain.NewError(domain.CodeInternal, "流尚未开始或已结束，不能重复发送终止帧")
	}

	finishReason := encodeFinishReason(chunk.FinishReason)
	candidate := wireCandidate{
		Content: wireContent{Role: roleModel, Parts: []wirePart{}},
	}
	if finishReason != "" {
		candidate.FinishReason = &finishReason
	}
	wire := wireResponse{
		Candidates:   []wireCandidate{candidate},
		ModelVersion: a.streamModel,
	}
	if chunk.Usage != nil {
		wire.UsageMetadata = wireUsageFromDomain(*chunk.Usage)
	}
	frame, err := a.encodeStreamFrame(wire)
	a.resetStreamLocked()
	return frame, err
}

// EncodeStreamError 报告本协议没有流式错误帧。
//
// Gemini 的错误一律以 HTTP 错误响应表达，SSE 流里没有承载错误的事件类型。
// 上游在流中途断开时，调用方按「结束后关闭连接」降级处理。
func (a *Adapter) EncodeStreamError(error) ([]byte, bool) { return nil, false }

// encodeStreamFrame 把一份响应对象编码成一帧 `data: <json>\n\n`。
//
// Gemini 的 SSE 帧不带 event 字段，因此在协议侧没有事件名可用。
func (a *Adapter) encodeStreamFrame(wire wireResponse) ([]byte, error) {
	payload, err := json.Marshal(wire)
	if err != nil {
		return nil, domain.NewError(domain.CodeInternal, "编码流式帧失败").WithCause(err)
	}
	var buf bytes.Buffer
	buf.WriteString("data: ")
	buf.Write(payload)
	buf.WriteString("\n\n")
	return buf.Bytes(), nil
}

// resetStreamLocked 清空一轮流的编码进度。
func (a *Adapter) resetStreamLocked() {
	a.streamStarted = false
	a.streamModel = ""
}

// ---------------------------------------------------------------------------
// 请求重建（面向上游）
// ---------------------------------------------------------------------------

// EncodeRequest 按内部统一格式重建 Gemini 的 generateContent 请求体。
//
// 三处协议差异在这里被吸收：
//   - system 角色上提为顶层 systemInstruction（Gemini 的 contents 里没有 system 角色）；
//   - tool 角色折叠为 user 内容里的 functionResponse 片段，assistant 的工具调用写成
//     functionCall 片段；
//   - 模型名与是否流式不写进请求体：模型名由 UpstreamURL 写进路径，流式由路径动作决定。
//
// 输出上限按统一口径计算后写进 generationConfig.maxOutputTokens。第二个返回值报告本次
// 重建相对客户端请求实际改过的部分：模型名（地址里用的上游模型与客户端请求的不同）与
// 输出上限。流式形态不产生标注——它由地址而非报文表达。
func (a *Adapter) EncodeRequest(req *domain.Request, options domain.RewriteOptions) ([]byte, domain.RewriteParts, error) {
	if req == nil {
		return nil, nil, domain.NewError(domain.CodeInternal, "请求为空")
	}
	system, err := encodeSystemInstruction(req.Messages)
	if err != nil {
		return nil, nil, err
	}
	contents, err := encodeContents(req.Messages)
	if err != nil {
		return nil, nil, err
	}
	if len(contents) == 0 {
		return nil, nil, domain.NewError(domain.CodeInternal, "Gemini 要求至少一条 contents")
	}
	toolConfig, err := encodeToolConfig(req.ToolChoice)
	if err != nil {
		return nil, nil, err
	}
	tools, err := encodeTools(req.Tools)
	if err != nil {
		return nil, nil, err
	}

	wire := wireRequest{
		Contents:          contents,
		SystemInstruction: system,
		Tools:             tools,
		ToolConfig:        toolConfig,
	}
	var parts domain.RewriteParts
	if options.UpstreamModel != "" && options.UpstreamModel != req.Model {
		// 模型名不在请求体里，但上游地址里的模型确实被换成了渠道映射的上游名，
		// 因此这一项照实登记。
		parts = append(parts, domain.RewritePartRequestModel)
	}
	if maxTokens := domain.OutputLimit(req.MaxTokens, options.MaxOutputTokens); maxTokens > 0 {
		wire.GenerationConfig = &wireGenerationConfig{MaxOutputTokens: &maxTokens}
		if maxTokens != req.MaxTokens {
			parts = append(parts, domain.RewritePartRequestOutputLimit)
		}
	}
	if req.Temperature != nil {
		if wire.GenerationConfig == nil {
			wire.GenerationConfig = &wireGenerationConfig{}
		}
		wire.GenerationConfig.Temperature = req.Temperature
	}

	body, err := json.Marshal(wire)
	if err != nil {
		return nil, nil, domain.NewError(domain.CodeInternal, "编码上游请求失败").WithCause(err)
	}
	return body, parts, nil
}

// RewriteRawBody 对同协议透传的原始报文做字段级改写。
//
// 唯一可改写的字段是 generationConfig.maxOutputTokens：模型名与流式形态都不在请求体里
// （前者由上游地址承载、后者由地址动作决定），因此本方法不改也不标模型名。
// 第二个返回值按实际发生的变化填报；没有任何改写发生时逐字节返回入参。
func (a *Adapter) RewriteRawBody(body []byte, options domain.RewriteOptions) ([]byte, domain.RewriteParts, error) {
	if options.MaxOutputTokens == nil || *options.MaxOutputTokens <= 0 {
		return body, nil, nil
	}
	fields, err := domain.DecodeRawFields(body)
	if err != nil {
		return nil, nil, err
	}
	generation, err := rawGenerationConfig(fields)
	if err != nil {
		return nil, nil, err
	}
	if !domain.SetRawOutputLimit(generation, []string{fieldMaxOutputTokens}, fieldMaxOutputTokens, options.MaxOutputTokens) {
		return body, nil, nil
	}
	encoded, err := json.Marshal(generation)
	if err != nil {
		return nil, nil, domain.NewError(domain.CodeInternal, "编码 generationConfig 失败").WithCause(err)
	}
	fields[fieldGenerationConfig] = encoded
	body, err = json.Marshal(fields)
	if err != nil {
		return nil, nil, domain.NewError(domain.CodeInternal, "编码改写后的请求失败").WithCause(err)
	}
	return body, domain.RewriteParts{domain.RewritePartRequestOutputLimit}, nil
}

// rawGenerationConfig 取出请求体顶层的 generationConfig 字段表。
//
// 字段缺失时返回空表，让改写按「补齐」语义写入；字段存在但不是 JSON 对象时原样返回空表，
// 由上游判定这份报文非法，本方法不替它下结论。
func rawGenerationConfig(fields map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	raw, ok := fields[fieldGenerationConfig]
	if !ok {
		return map[string]json.RawMessage{}, nil
	}
	var generation map[string]json.RawMessage
	if err := json.Unmarshal(raw, &generation); err != nil {
		return map[string]json.RawMessage{}, nil //nolint:nilerr // 形状非预期时退回空表，交由上游判定。
	}
	if generation == nil {
		generation = map[string]json.RawMessage{}
	}
	return generation, nil
}

// UpstreamHeaders 返回调用上游时应携带的请求头。
//
// Gemini 的必需信息只有凭据（由装配层的凭据提供者按协议注入 x-goog-api-key），
// 协议自身没有额外的必需头，因此这里原样回传渠道级配置。
func (a *Adapter) UpstreamHeaders(channelHeaders http.Header) http.Header {
	return domain.CloneHeader(channelHeaders)
}

// encodeSystemInstruction 把所有 system 消息的文本片段上提为 systemInstruction。
func encodeSystemInstruction(messages []domain.Message) (*wireContent, error) {
	var parts []wirePart
	for i := range messages {
		message := &messages[i]
		if message.Role != domain.RoleSystem {
			continue
		}
		for j := range message.Parts {
			part := &message.Parts[j]
			if part.Kind != domain.PartText {
				return nil, domain.NewError(domain.CodeInternal, fmt.Sprintf(
					"第 %d 条 system 消息第 %d 个片段不是文本，Gemini 的 systemInstruction 无法表达", i, j))
			}
			parts = append(parts, wirePart{Text: part.Text})
		}
	}
	if len(parts) == 0 {
		return nil, nil
	}
	return &wireContent{Parts: parts}, nil
}

// encodeContents 把非 system 消息编码为 contents。
//
// Gemini 的会话是 user 与 model 交替的两方对话：内部 tool 角色的消息折叠为 user 内容里的
// functionResponse 片段，相邻同角色的内容合并成一条，避免上游因「连续两条 user」拒绝。
// functionResponse 的 name 由 assistant 侧 functionCall 的 id→name 映射补出：
// 工具结果本身只带调用 id，而上游要求这里写函数名。
func encodeContents(messages []domain.Message) ([]wireContent, error) {
	names := toolCallNames(messages)
	var contents []wireContent
	for i := range messages {
		message := &messages[i]
		if message.Role == domain.RoleSystem {
			continue
		}
		role, err := encodeRole(message.Role, i)
		if err != nil {
			return nil, err
		}
		parts, err := encodeContentParts(message.Parts, names, i)
		if err != nil {
			return nil, err
		}
		if len(parts) == 0 {
			return nil, domain.NewError(domain.CodeInternal, fmt.Sprintf("第 %d 条消息内容为空", i))
		}
		if n := len(contents); n > 0 && contents[n-1].Role == role {
			contents[n-1].Parts = append(contents[n-1].Parts, parts...)
			continue
		}
		contents = append(contents, wireContent{Role: role, Parts: parts})
	}
	return contents, nil
}

// encodeRole 把内部角色映射为 Gemini 的角色。
//
// tool 角色的消息承载工具结果，在 Gemini 里属于 user 一侧。
func encodeRole(role domain.Role, index int) (string, error) {
	switch role {
	case domain.RoleUser, domain.RoleTool:
		return roleUser, nil
	case domain.RoleAssistant:
		return roleModel, nil
	default:
		return "", domain.NewError(domain.CodeInternal, fmt.Sprintf(
			"第 %d 条消息角色 %q 无法编码为 Gemini 请求", index, string(role)))
	}
}

// toolCallNames 收集 assistant 消息里工具调用的 id→name 映射。
func toolCallNames(messages []domain.Message) map[string]string {
	names := make(map[string]string)
	for i := range messages {
		for j := range messages[i].Parts {
			call := messages[i].Parts[j].ToolCall
			if call == nil {
				continue
			}
			if _, exists := names[call.ID]; !exists {
				names[call.ID] = call.Name
			}
		}
	}
	return names
}

// encodeContentParts 把一条消息的片段编码为 Gemini 片段。
func encodeContentParts(parts []domain.Part, names map[string]string, index int) ([]wirePart, error) {
	out := make([]wirePart, 0, len(parts))
	for j := range parts {
		part := &parts[j]
		switch part.Kind {
		case domain.PartText:
			out = append(out, wirePart{Text: part.Text})
		case domain.PartImage:
			encoded, err := encodeImagePart(part.ImageURL)
			if err != nil {
				return nil, err
			}
			out = append(out, encoded)
		case domain.PartToolCall:
			if part.ToolCall == nil {
				return nil, domain.NewError(domain.CodeInternal, fmt.Sprintf("第 %d 条消息的工具调用片段缺少内容", index))
			}
			out = append(out, wirePart{FunctionCall: encodeFunctionCall(part.ToolCall)})
		case domain.PartToolResult:
			if part.ToolResult == nil {
				return nil, domain.NewError(domain.CodeInternal, fmt.Sprintf("第 %d 条消息的工具结果片段缺少内容", index))
			}
			out = append(out, wirePart{FunctionResponse: encodeFunctionResponse(part.ToolResult, names)})
		case domain.PartReasoning:
			// 推理内容不回传上游：Gemini 要求 thought 片段带签名，纯文本重建不出来。
		default:
			return nil, domain.NewError(domain.CodeInternal, fmt.Sprintf(
				"第 %d 条消息包含无法编码的片段类型 %q", index, string(part.Kind)))
		}
	}
	return out, nil
}

// encodeFunctionResponse 把工具结果编码为 functionResponse。
//
// Gemini 要求 response 是一个对象，而内部格式保留的是字符串；能解析成对象就原样用，
// 能解析成数组就包一层 result，否则包成 {"content": <原文>}。
// name 取调用 id 对应的函数名；映射里查不到时退回调用 id，至少保证字段非空。
func encodeFunctionResponse(result *domain.ToolResult, names map[string]string) *wireFunctionResponse {
	name := names[result.ToolCallID]
	if strings.TrimSpace(name) == "" {
		name = result.ToolCallID
	}
	response := map[string]any{}
	content := strings.TrimSpace(result.Content)
	if content != "" {
		var decoded any
		if err := json.Unmarshal([]byte(content), &decoded); err == nil {
			switch typed := decoded.(type) {
			case map[string]any:
				response = typed
			case []any:
				response = map[string]any{"result": typed}
			default:
				response = map[string]any{"content": typed}
			}
		} else {
			response = map[string]any{"content": result.Content}
		}
	}
	encoded := &wireFunctionResponse{Name: name, Response: response}
	if id := strings.TrimSpace(result.ToolCallID); id != "" {
		encoded.ID = marshalRaw(id)
	}
	return encoded
}

// encodeTools 把内部工具表编码为一个 functionDeclarations 集合。
func encodeTools(tools []domain.ToolSpec) ([]wireTool, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	declarations := make([]wireFunctionDeclaration, 0, len(tools))
	for i := range tools {
		tool := &tools[i]
		if strings.TrimSpace(tool.Name) == "" {
			return nil, domain.NewError(domain.CodeInternal, fmt.Sprintf("第 %d 个工具缺少 name", i))
		}
		parameters, err := cleanFunctionSchema(tool.ParametersJSON)
		if err != nil {
			return nil, err
		}
		declarations = append(declarations, wireFunctionDeclaration{
			Name:        tool.Name,
			Description: tool.Description,
			Parameters:  parameters,
		})
	}
	return []wireTool{{FunctionDeclarations: declarations}}, nil
}

// encodeToolConfig 把内部工具选择映射为 Gemini 的 toolConfig。
//
// auto/none 原样；必须调用映射为 ANY；指定工具映射为 ANY 加一个允许名。
// 未指定时不携带该字段。无法映射的模式返回内部错误，不得静默丢弃客户端的约束。
func encodeToolConfig(choice domain.ToolChoice) (*wireToolConfig, error) {
	mode := ""
	var allowed []string
	switch choice.Mode {
	case domain.ToolChoiceUnset:
		return nil, nil
	case domain.ToolChoiceAuto:
		mode = "AUTO"
	case domain.ToolChoiceNone:
		mode = "NONE"
	case domain.ToolChoiceRequired:
		mode = "ANY"
	case domain.ToolChoiceTool:
		name := strings.TrimSpace(choice.Name)
		if name == "" {
			return nil, domain.NewError(domain.CodeInternal, "指定工具时缺少工具名")
		}
		mode = "ANY"
		allowed = []string{name}
	default:
		return nil, domain.NewError(domain.CodeInternal, fmt.Sprintf("无法编码的工具选择模式 %q", string(choice.Mode)))
	}
	return &wireToolConfig{FunctionCallingConfig: &wireFunctionCallingConfig{
		Mode:                 mode,
		AllowedFunctionNames: allowed,
	}}, nil
}
