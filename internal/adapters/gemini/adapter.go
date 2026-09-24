package gemini

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

// 编译期断言：本适配器实现统一适配器，以及上游地址构造、路径绑定与
// 上游请求构建、上游请求头三项可选能力。
var (
	_ domain.Adapter                = (*Adapter)(nil)
	_ domain.UpstreamURLBuilder     = (*Adapter)(nil)
	_ domain.RequestBinder          = (*Adapter)(nil)
	_ domain.UpstreamRequestBuilder = (*Adapter)(nil)
	_ domain.UpstreamHeaderProvider = (*Adapter)(nil)
	_ domain.StreamErrorEncoder     = (*Adapter)(nil)
)

// Adapter 实现 domain.Adapter。
//
// 它是无服务的单例，但同一份配置下需要承载三类状态，因此每类状态各有一份边界：
//
//   - 路径绑定状态（boundModel / boundStream / bindErr）：由 BindRequest 产出的副本持有，
//     单例本身不带，用于把请求路径上的模型名与流式标记带进 DecodeRequest；
//   - 编码方向流式状态（streamStarted / streamModel）：一次客户端流一个实例；
//   - 解码方向流式状态（decoder）：一次上游流一个实例。
//
// 编码与解码状态在 NewStream 派生的实例上各自独立，两条并发流不得共享实例。
type Adapter struct {
	// 路径绑定事实。三者只由 BindRequest 写入，其余方法只读。
	boundModel  string
	boundStream bool
	bindErr     error

	mu            sync.Mutex
	streamStarted bool
	// streamModel 是本次流要写进每个帧的模型名，取自 EncodeStreamStart 的入参。
	streamModel string

	decodeMu sync.Mutex
	decoder  streamDecoder
}

// New 返回一个未被绑定路径的适配器实例。
//
// 它可以直接用于上游方向（请求构建、响应解码），也用于测试；
// 面向客户端的请求必须在 DecodeRequest 之前经 BindRequest 绑定路径事实。
func New() *Adapter {
	return &Adapter{}
}

// NewStream 派生一个只服务单次流式响应的新适配器实例。
//
// 编码方向要跨帧保存「流是否已开始」与模型名，解码方向要保存累计用量与结束原因，
// 都不能被两条并发流共享。路径绑定事实不继承：流式编解码都不需要它。
func (a *Adapter) NewStream() domain.Adapter {
	return &Adapter{}
}

// Protocol 返回本适配器对应的协议。
func (a *Adapter) Protocol() domain.Protocol {
	return domain.ProtocolGemini
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
// 请求解码（面向客户端）
// ---------------------------------------------------------------------------

// DecodeRequest 把 Gemini 的 generateContent 请求体解码为统一内部请求。
//
// 模型名与是否流式不在请求体里：前者取自路径绑定（BindRequest），后者由路径动作与
// alt 查询参数判定。因此本方法只处理请求体的字段，两者都从实例上读。
func (a *Adapter) DecodeRequest(body []byte) (*domain.Request, error) {
	if a.bindErr != nil {
		return nil, a.bindErr
	}
	if strings.TrimSpace(a.boundModel) == "" {
		return nil, invalidRequest("Gemini 请求的模型名来自请求路径，解码前必须先绑定路径")
	}
	var wire wireRequest
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, invalidRequest("请求体不是合法 JSON")
	}
	if len(wire.Contents) == 0 {
		return nil, invalidRequest("缺少必填字段 contents")
	}

	messages, err := decodeMessages(&wire)
	if err != nil {
		return nil, err
	}
	toolChoice, err := decodeToolChoice(wire.ToolConfig)
	if err != nil {
		return nil, err
	}
	tools, err := decodeTools(wire.Tools)
	if err != nil {
		return nil, err
	}

	req := &domain.Request{
		Protocol:   domain.ProtocolGemini,
		Model:      a.boundModel,
		Messages:   messages,
		Tools:      tools,
		ToolChoice: toolChoice,
		Stream:     a.boundStream,
		// RawBody 必须拷贝：所有权归 Request，同协议回退重放要用它。
		RawBody: bytes.Clone(body),
	}
	if gc := wire.GenerationConfig; gc != nil {
		if gc.MaxOutputTokens != nil {
			if *gc.MaxOutputTokens <= 0 {
				return nil, invalidRequest("generationConfig.maxOutputTokens 必须为正整数")
			}
			req.MaxTokens = *gc.MaxOutputTokens
		}
		if gc.Temperature != nil {
			if *gc.Temperature < 0 || *gc.Temperature > 2 {
				return nil, invalidRequest("generationConfig.temperature 必须在 [0, 2] 区间")
			}
			req.Temperature = gc.Temperature
		}
	}

	a.mu.Lock()
	a.resetStreamLocked()
	a.mu.Unlock()
	return req, nil
}

// decodeMessages 把 contents 与 systemInstruction 归一化为内部消息序列。
//
// systemInstruction 上提为一条 system 消息放在最前；contents 里的角色映射为
// user / assistant，role 为 model 时是助手。未识别的角色与只含未识别片段的
// 消息跳过，避免上游扩展角色枚举时整条请求失败。推理片段（thought）在请求方向不建模：
// 它必须连同签名一并回传上游，重建出来的纯文本反而会让上游拒绝。
func decodeMessages(wire *wireRequest) ([]domain.Message, error) {
	messages := make([]domain.Message, 0, len(wire.Contents)+1)
	if wire.SystemInstruction != nil {
		parts, err := decodeSystemParts(wire.SystemInstruction.Parts)
		if err != nil {
			return nil, err
		}
		if len(parts) > 0 {
			messages = append(messages, domain.Message{Role: domain.RoleSystem, Parts: parts})
		}
	}

	for i := range wire.Contents {
		content := &wire.Contents[i]
		role, ok := decodeRole(content.Role)
		if !ok {
			continue
		}
		parts, skipped, err := decodeParts(content.Parts, false)
		if err != nil {
			return nil, err
		}
		if len(parts) == 0 {
			if skipped {
				continue
			}
			return nil, invalidRequest(fmt.Sprintf("第 %d 条消息内容为空", i))
		}
		messages = append(messages, domain.Message{Role: role, Parts: parts})
	}
	if len(messages) == 0 {
		return nil, invalidRequest("contents 中没有可解码的消息")
	}
	return messages, nil
}

// decodeSystemParts 把 systemInstruction 归一化为文本片段。
// 该字段只承载系统提示，出现非文本片段时报错：内部格式的 system 消息是纯文本的。
func decodeSystemParts(parts []wirePart) ([]domain.Part, error) {
	out := make([]domain.Part, 0, len(parts))
	for i := range parts {
		part := &parts[i]
		if part.Text == "" {
			return nil, invalidRequest(fmt.Sprintf("systemInstruction 第 %d 个片段不是文本", i))
		}
		out = append(out, domain.Part{Kind: domain.PartText, Text: part.Text})
	}
	return out, nil
}

// decodeRole 把 Gemini 的角色映射为内部角色。
//
// Gemini 只有 user 与 model 两种角色（旧的 function 角色已废弃，工具结果现在写在
// user 消息的 functionResponse 片段里）。第二个返回值为 false 表示角色未识别。
func decodeRole(role string) (domain.Role, bool) {
	switch role {
	case roleUser, "function":
		// function 是被废弃的旧角色，语义等同「工具结果所在的那条消息」，按 user 处理。
		return domain.RoleUser, true
	case roleModel:
		return domain.RoleAssistant, true
	default:
		return "", false
	}
}

// decodeParts 把若干片段归一化为内部片段。
//
// decodeReasoning 是显式方向参数：响应方向把 thought 文本建模为 PartReasoning，
// 请求方向跳过它（见 decodeMessages 的说明）。未识别片段与取不到取值的片段按
// 向前兼容跳过；已知片段缺必填字段仍返回错误。
func decodeParts(parts []wirePart, decodeReasoning bool) ([]domain.Part, bool, error) {
	out := make([]domain.Part, 0, len(parts))
	skipped := false
	for i := range parts {
		part := &parts[i]
		switch {
		case part.InlineData != nil:
			if part.InlineData.MimeType == "" || part.InlineData.Data == "" {
				return nil, false, invalidRequest("inlineData 缺少 mimeType 或 data")
			}
			out = append(out, domain.Part{
				Kind:     domain.PartImage,
				ImageURL: dataURIPrefix + part.InlineData.MimeType + dataURIBase64Suffix + "," + part.InlineData.Data,
			})
		case part.FileData != nil:
			if part.FileData.FileURI == "" {
				return nil, false, invalidRequest("fileData 缺少 fileUri")
			}
			out = append(out, domain.Part{Kind: domain.PartImage, ImageURL: part.FileData.FileURI})
		case part.FunctionCall != nil:
			if part.FunctionCall.FunctionName == "" {
				return nil, false, invalidRequest("functionCall 缺少 name")
			}
			arguments := compactJSON(marshalRaw(part.FunctionCall.Arguments))
			if arguments == "" {
				arguments = jsonObjectEmpty
			}
			out = append(out, domain.Part{
				Kind: domain.PartToolCall,
				ToolCall: &domain.ToolCall{
					ID:        orGeneratedCallID(part.FunctionCall.ID),
					Name:      part.FunctionCall.FunctionName,
					Arguments: arguments,
				},
			})
		case part.FunctionResponse != nil:
			id := decodeFunctionResponseID(part.FunctionResponse)
			content := compactJSON(marshalRaw(part.FunctionResponse.Response))
			if content == "" {
				content = jsonObjectEmpty
			}
			out = append(out, domain.Part{
				Kind:       domain.PartToolResult,
				ToolResult: &domain.ToolResult{ToolCallID: id, Content: content},
			})
		case part.Text != "":
			if part.Thought {
				if !decodeReasoning {
					skipped = true
					continue
				}
				out = append(out, domain.Part{Kind: domain.PartReasoning, Text: part.Text})
				continue
			}
			out = append(out, domain.Part{Kind: domain.PartText, Text: part.Text})
		default:
			// 未建模的片段类型（executableCode、codeExecutionResult 等）：跳过。
			skipped = true
		}
	}
	return out, skipped, nil
}

// decodeFunctionResponseID 取工具结果对应的调用标识。
//
// 新版上游用 ID 字段把结果对上一次调用；旧版没有 ID，只有函数名，因此退回用函数名
// 作为关联键（重建方向同样按它去找调用）。
func decodeFunctionResponseID(response *wireFunctionResponse) string {
	if id := strings.TrimSpace(compactJSON(response.ID)); id != "" {
		return strings.Trim(id, `"`)
	}
	return response.Name
}

// decodeTools 把 wire.tools 里的函数声明归一化为内部工具表。
//
// 一个 tools 条目可以只带 googleSearch、codeExecution 一类内置工具；那些没有
// functionDeclarations，统一内部格式表达不了，跳过。多条声明的同名冲突交由上游判定。
func decodeTools(tools []wireTool) ([]domain.ToolSpec, error) {
	var out []domain.ToolSpec
	for i := range tools {
		for j := range tools[i].FunctionDeclarations {
			declaration := &tools[i].FunctionDeclarations[j]
			if strings.TrimSpace(declaration.Name) == "" {
				return nil, invalidRequest(fmt.Sprintf("第 %d 个工具集合第 %d 个声明缺少 name", i, j))
			}
			out = append(out, domain.ToolSpec{
				Name:           declaration.Name,
				Description:    declaration.Description,
				ParametersJSON: compactJSON(marshalRaw(declaration.Parameters)),
			})
		}
	}
	return out, nil
}

// decodeToolChoice 把 toolConfig.functionCallingConfig 归一化为内部工具选择。
//
// 模式映射：AUTO → auto，NONE → none，ANY 带一个允许名 → 指定工具，ANY 不带名字或多个名字
// → required。多个允许名在内部格式里没有对应表达，收敛成「必须调用至少一个工具」：
// 它比丢弃整个约束更接近客户端的要求，也不至于把一条合法请求判为非法。
func decodeToolChoice(config *wireToolConfig) (domain.ToolChoice, error) {
	if config == nil || config.FunctionCallingConfig == nil {
		return domain.ToolChoice{}, nil
	}
	calling := config.FunctionCallingConfig
	switch strings.ToUpper(strings.TrimSpace(calling.Mode)) {
	case "":
		// 只给了 allowedFunctionNames、没给模式：上游按 ANY 的语义处理。
		if len(calling.AllowedFunctionNames) > 0 {
			return toolChoiceFromNames(calling.AllowedFunctionNames), nil
		}
		return domain.ToolChoice{}, nil
	case "AUTO":
		return domain.ToolChoice{Mode: domain.ToolChoiceAuto}, nil
	case "NONE":
		return domain.ToolChoice{Mode: domain.ToolChoiceNone}, nil
	case "ANY":
		return toolChoiceFromNames(calling.AllowedFunctionNames), nil
	default:
		return domain.ToolChoice{}, invalidRequest(fmt.Sprintf("toolConfig.functionCallingConfig.mode %q 不受支持", calling.Mode))
	}
}

// toolChoiceFromNames 按允许的函数名产出一个工具选择。
func toolChoiceFromNames(names []string) domain.ToolChoice {
	if len(names) == 1 && strings.TrimSpace(names[0]) != "" {
		return domain.ToolChoice{Mode: domain.ToolChoiceTool, Name: names[0]}
	}
	return domain.ToolChoice{Mode: domain.ToolChoiceRequired}
}

// ---------------------------------------------------------------------------
// 响应编码（面向客户端）
// ---------------------------------------------------------------------------

// EncodeResponse 把内部统一响应编码为 Gemini 非流式响应体。
func (a *Adapter) EncodeResponse(resp *domain.Response) ([]byte, error) {
	if resp == nil {
		return nil, domain.NewError(domain.CodeInternal, "响应为空")
	}
	parts, err := encodeParts(resp.Message.Parts)
	if err != nil {
		return nil, err
	}
	finishReason := encodeFinishReason(resp.FinishReason)
	wire := wireResponse{
		Candidates: []wireCandidate{{
			Content: wireContent{Role: roleModel, Parts: parts},
			Index:   0,
		}},
		UsageMetadata: wireUsageFromDomain(resp.Usage),
		ModelVersion:  resp.Model,
		ResponseID:    resp.UpstreamRequestID,
	}
	if finishReason != "" {
		wire.Candidates[0].FinishReason = &finishReason
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, domain.NewError(domain.CodeInternal, "编码响应失败").WithCause(err)
	}
	return body, nil
}

// encodeParts 把内部片段编码为 Gemini 片段。
//
// 工具结果（PartToolResult）不允许出现在响应里：它是客户端回填给上游的东西，
// 出现在模型响应中说明上游解码或重建出了问题，报内部错误而不是产出一个语义错误的响应。
func encodeParts(parts []domain.Part) ([]wirePart, error) {
	out := make([]wirePart, 0, len(parts))
	for i := range parts {
		part := &parts[i]
		switch part.Kind {
		case domain.PartText:
			out = append(out, wirePart{Text: part.Text})
		case domain.PartReasoning:
			// 推理内容在编码方向不下发（与另外两种协议一致）：Gemini 的 thought 片段
			// 只是显示用的，重建出来的纯文本没有签名，回传上游会被拒。
		case domain.PartImage:
			encoded, err := encodeImagePart(part.ImageURL)
			if err != nil {
				return nil, err
			}
			out = append(out, encoded)
		case domain.PartToolCall:
			if part.ToolCall == nil {
				return nil, domain.NewError(domain.CodeInternal, fmt.Sprintf("第 %d 个片段缺少工具调用", i))
			}
			out = append(out, wirePart{FunctionCall: encodeFunctionCall(part.ToolCall)})
		default:
			return nil, domain.NewError(domain.CodeInternal, fmt.Sprintf("响应片段类型 %q 无法编码", string(part.Kind)))
		}
	}
	return out, nil
}

// encodeImagePart 把内部图片 URL 编码为 Gemini 片段：
//   - data URI 拆成 inlineData（Gemini 用它承载内联 base64）；
//   - 其余按 fileData 原样引用（Files API 的 URI）。
func encodeImagePart(imageURL string) (wirePart, error) {
	mediaType, data, ok := splitDataURI(imageURL)
	if ok {
		return wirePart{InlineData: &wireInlineData{MimeType: mediaType, Data: data}}, nil
	}
	if imageURL == "" {
		return wirePart{}, domain.NewError(domain.CodeInternal, "图片片段缺少 image_url")
	}
	// fileData 的 mimeType 在内部格式里没有保存：只带 URI，让上游按 URI 推断。
	return wirePart{FileData: &wireFileData{FileURI: imageURL}}, nil
}

// splitDataURI 拆解 base64 图片的 data URI，返回 media type 与 base64 数据。
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

// encodeFunctionCall 把内部工具调用编码为 Gemini 的 functionCall。
//
// 参数在 Gemini 里是对象而不是字符串：内部格式保留的是 JSON 原文，这里解析回对象。
// 解析失败或不是对象时用空对象兜底，让上游按「没有参数」处理，
// 而不是把一份非法 JSON 塞进 args 让整条请求被拒。
func encodeFunctionCall(call *domain.ToolCall) *wireFunctionCall {
	encoded := &wireFunctionCall{
		ID:           call.ID,
		FunctionName: call.Name,
	}
	arguments := strings.TrimSpace(call.Arguments)
	if arguments != "" {
		var decoded any
		if err := json.Unmarshal([]byte(arguments), &decoded); err == nil {
			encoded.Arguments = decoded
		}
	}
	if encoded.Arguments == nil {
		encoded.Arguments = map[string]any{}
	}
	return encoded
}

// ---------------------------------------------------------------------------
// 错误编码
// ---------------------------------------------------------------------------

// EncodeError 把错误编码为 Gemini 的错误响应体与 HTTP 状态码。
//
// 响应体形如 {"error":{"code":<状态码>,"message":…,"status":"INVALID_ARGUMENT"}}，
// 与 Google API 的错误信封一致；status 由统一错误码映射到规范的 Google 状态名。
func (a *Adapter) EncodeError(err error) (int, []byte) {
	status := domain.HTTPStatus(err)
	message := "服务内部错误"
	code := domain.CodeInternal
	if e := domain.AsError(err); e != nil {
		code = e.Code
		if e.Message != "" {
			message = e.Message
		}
	}
	body, marshalErr := json.Marshal(wireError{Error: wireErrorDetail{
		Code:    status,
		Message: message,
		Status:  googleStatus(code),
	}})
	if marshalErr != nil {
		return status, []byte(`{"error":{"code":500,"message":"服务内部错误","status":"INTERNAL"}}`)
	}
	return status, body
}

// googleStatus 把统一错误码映射为 Google 的规范状态名。
//
// Google 客户端 SDK 按 status 分派异常类型，因此它比 HTTP 状态码更贴近使用者的判断依据。
// 未登记的错误码按 INTERNAL 处理，避免把一个平台故障说成客户端问题。
func googleStatus(code domain.Code) string {
	switch code {
	case domain.CodeInvalidRequest:
		return "INVALID_ARGUMENT"
	case domain.CodeUnauthorized:
		return "UNAUTHENTICATED"
	case domain.CodeForbidden:
		return "PERMISSION_DENIED"
	case domain.CodeRateLimited, domain.CodeGatewayOverloaded, domain.CodeUpstreamRateLimited:
		return "RESOURCE_EXHAUSTED"
	case domain.CodeModelNotFound, domain.CodeNotFound:
		return "NOT_FOUND"
	case domain.CodeUpstreamTimeout:
		return "DEADLINE_EXCEEDED"
	case domain.CodeUpstreamUnavailable:
		return "UNAVAILABLE"
	case domain.CodeUpstreamRejected:
		return "FAILED_PRECONDITION"
	default:
		return "INTERNAL"
	}
}

// ---------------------------------------------------------------------------
// 结束原因映射
// ---------------------------------------------------------------------------

// decodeFinishReason 把 Gemini 的结束原因字面量映射为内部枚举。
//
// 未识别的字面量一律归为 FinishUnknown，不当成正常结束：
// MALFORMED_FUNCTION_CALL、UNEXPECTED_TOOL_CALL 一类是上游在说「这次生成没走完」，把它们
// 当成 stop 会让客户端以为拿到的是完整回答。
func decodeFinishReason(literal string) domain.FinishReason {
	switch literal {
	case "STOP":
		return domain.FinishStop
	case "MAX_TOKENS":
		return domain.FinishLength
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII",
		"IMAGE_SAFETY", "IMAGE_PROHIBITED_CONTENT", "IMAGE_RECITATION", "LANGUAGE":
		return domain.FinishContentFilter
	default:
		return domain.FinishUnknown
	}
}

// encodeFinishReason 把内部结束原因映射回 Gemini 的字面量。
//
// 工具调用在 Gemini 里没有专用字面量：模型请求调用工具时结束原因就是 STOP，
// 调用意图由 content.parts 里的 functionCall 表达，因此 FinishToolCalls 映射为 STOP。
// FinishUnknown 没有对应字面量，返回空串（调用方省略该字段）。
func encodeFinishReason(reason domain.FinishReason) string {
	switch reason {
	case domain.FinishStop, domain.FinishToolCalls:
		return "STOP"
	case domain.FinishLength:
		return "MAX_TOKENS"
	case domain.FinishContentFilter:
		return "SAFETY"
	default:
		return ""
	}
}

// ---------------------------------------------------------------------------
// 用量映射
// ---------------------------------------------------------------------------

// usageFromWire 把上游用量归一化为内部用量。
//
// Gemini 的输入侧把「服务端工具占用的输入」单独计在 toolUsePromptTokenCount，
// 一次调用的输入总量是它与 promptTokenCount 之和；输出侧的推理 token 单独计在
// thoughtsTokenCount，输出总量是它与 candidatesTokenCount 之和，推理同时是它的子项。
// cachedContentTokenCount 是输入侧的命中缓存子项，直接从属于输入总数。
//
// 上游未给出 usageMetadata 时返回零值，即来源为「未取得用量」的未取得形态；
// 调用方必须据此走兜底分支，不得按零结算。
func usageFromWire(metadata *wireUsageMetadata) domain.Usage {
	if metadata == nil {
		return domain.Usage{}
	}
	input := metadata.PromptTokenCount + metadata.ToolUsePromptTokenCount
	output := metadata.CandidatesTokenCount + metadata.ThoughtsTokenCount
	if output == 0 && metadata.TotalTokenCount > input {
		// 上游只给了总数、没给候选计数：用总数回填输出侧，避免整份用量被判为已知的零。
		output = metadata.TotalTokenCount - input
	}
	return domain.Usage{
		Source:          domain.UsageSourceUpstream,
		InputTokens:     input,
		OutputTokens:    output,
		CacheReadTokens: metadata.CachedContentTokenCount,
		ReasoningTokens: metadata.ThoughtsTokenCount,
	}.BoundSubitemsToMain()
}

// wireUsageFromDomain 把内部用量还原为 Gemini 的 usageMetadata。
//
// 线格式的 candidatesTokenCount 不含推理 token，因此由输出总数减去推理子项得到；
// promptTokenCount 含缓存子项，与内部口径一致，直接取输入总数。
func wireUsageFromDomain(usage domain.Usage) *wireUsageMetadata {
	candidates := usage.OutputTokens - usage.ReasoningTokens
	if candidates < 0 {
		candidates = 0
	}
	return &wireUsageMetadata{
		PromptTokenCount:        usage.InputTokens,
		CandidatesTokenCount:    candidates,
		TotalTokenCount:         usage.Total(),
		ThoughtsTokenCount:      usage.ReasoningTokens,
		CachedContentTokenCount: usage.CacheReadTokens,
	}
}

// ---------------------------------------------------------------------------
// 工具与标识
// ---------------------------------------------------------------------------

// callIDPrefix 是给缺少 id 的工具调用生成的标识前缀，与另外两个协议保持一致。
const callIDPrefix = "call_"

// orGeneratedCallID 在工具调用自带标识时原样返回，缺失时生成一个。
//
// Gemini 官方把 functionCall.id 标为可选；内部格式要求工具调用必须有 id（流式拼接与
// 工具结果回填都靠它），因此这里补一个本地唯一值，而不是让一条合法响应解码失败。
func orGeneratedCallID(id string) string {
	if strings.TrimSpace(id) != "" {
		return id
	}
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return callIDPrefix + "local"
	}
	return callIDPrefix + hex.EncodeToString(raw[:])
}

// invalidRequest 构造 invalid_request 统一错误。
func invalidRequest(message string) *domain.Error {
	return domain.NewError(domain.CodeInvalidRequest, message)
}
