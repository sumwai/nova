// Package gemini 实现 Google Gemini 的 generateContent 线协议与
// internal/domain 统一内部格式之间的双向转换。
//
// 它与另外三种协议的差别集中在三处，本包的结构也照这三处分：
//
//   - 模型名与动作（是否流式）写在请求路径上，而不是请求体里：
//     POST /v1beta/models/{model}:generateContent 与 :streamGenerateContent?alt=sse。
//     上游地址因此由本适配器按本次请求构造（domain.UpstreamURLBuilder），
//     客户端进来的模型名由入口层在解码前绑定（domain.RequestBinder）；
//   - 消息角色只有 user 与 model 两种，工具调用是 content.parts 里的一种片段
//     （functionCall），工具结果以 functionResponse 片段回填；
//   - 流式响应没有独立的开始帧，也没有 [DONE] 一类的终止哨兵帧：一帧就是一份不完整的
//     响应，结束信号是最后一个帧里候选的 finishReason。
//
// 协议差异只能在本包内表达，流水线与计费代码只依赖 domain.Adapter。
// 本包只依赖标准库与 internal/domain。
//
// 关于请求体里的模型名：它不存在。因此同协议透传路径（RewriteRawBody）改写的字段里
// 没有模型名一项，模型名只在构造上游地址时写入路径（见 path.go）。
package gemini

import (
	"encoding/json"
)

const (
	// contentTypeJSON 是非流式响应的 Content-Type。
	contentTypeJSON = "application/json"
	// contentTypeSSE 是流式响应的 Content-Type。
	contentTypeSSE = "text/event-stream"

	// 消息角色。Gemini 用 model 表示助手，没有 system（系统提示走顶层 systemInstruction）。
	roleUser  = "user"
	roleModel = "model"

	// 动作段：它写在路径末尾，决定这次调用是流式还是非流式。
	// 上游地址里两种动作都合法，发哪一种由本次请求是否流式决定。
	actionGenerate       = ":generateContent"
	actionStreamGenerate = ":streamGenerateContent"

	// dataURIPrefix 与 dataURIBase64Suffix 用于识别并拆解 base64 图片的 data URI。
	dataURIPrefix       = "data:"
	dataURIBase64Suffix = ";base64"

	// 请求字段名。只有需要按字段名读写的那些才在这里成常量，其余由结构体标签表达。
	fieldGenerationConfig = "generationConfig"
	fieldMaxOutputTokens  = "maxOutputTokens"
	fieldParts            = "parts"

	// jsonObjectEmpty 是空 JSON 对象的紧凑写法，用于工具参数缺失时的兜底。
	jsonObjectEmpty = "{}"

	// jsonNullLiteral 是 JSON null 的文本表示。
	jsonNullLiteral = "null"

	// finishReasonUnspecified 是「上游还没给出结束原因」的取值，解码时按未给出处理。
	finishReasonUnspecified = "FINISH_REASON_UNSPECIFIED"
)

// ---------------------------------------------------------------------------
// 请求 wire 结构
// ---------------------------------------------------------------------------

// wireRequest 是 generateContent 请求体。
//
// 它同时用于解码客户端请求与重建上游请求：两侧字段完全一致，差异只在取值来源。
type wireRequest struct {
	Contents          []wireContent         `json:"contents"`
	SystemInstruction *wireContent          `json:"systemInstruction,omitempty"`
	GenerationConfig  *wireGenerationConfig `json:"generationConfig,omitempty"`
	Tools             []wireTool            `json:"tools,omitempty"`
	ToolConfig        *wireToolConfig       `json:"toolConfig,omitempty"`
	SafetySettings    []wireSafetySetting   `json:"safetySettings,omitempty"`
	CachedContent     string                `json:"cachedContent,omitempty"`
}

// wireContent 是一条消息：角色加若干片段。
type wireContent struct {
	Role  string     `json:"role,omitempty"`
	Parts []wirePart `json:"parts"`
}

// wirePart 是消息里的一个片段。字段按片段类型可选，解码时忽略与本类型无关的字段。
//
// 未建模的片段类型（executableCode、codeExecutionResult、mediaResolution 等）解码时跳过，
// 重建时不产出：统一内部格式没有对应表达，硬塞进文本会伪造一份上游没给过的内容。
type wirePart struct {
	// text 片段；thought 为真时它是模型的推理内容而不是回答正文。
	Text    string `json:"text,omitempty"`
	Thought bool   `json:"thought,omitempty"`

	// 图片与文件：inlineData 携带 base64，fileData 引用 Files API 的上传结果。
	InlineData *wireInlineData `json:"inlineData,omitempty"`
	FileData   *wireFileData   `json:"fileData,omitempty"`

	// 工具调用与工具结果。
	FunctionCall     *wireFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *wireFunctionResponse `json:"functionResponse,omitempty"`

	// 推理签名：只在解码时保留原文，重建方向不下发（纯文本无法重建签名）。
	ThoughtSignature json.RawMessage `json:"thoughtSignature,omitempty"`
}

// wireInlineData 是内联二进制载荷。
type wireInlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

// wireFileData 是 Files API 引用。
type wireFileData struct {
	MimeType string `json:"mimeType,omitempty"`
	FileURI  string `json:"fileUri,omitempty"`
}

// wireFunctionCall 是一次工具调用。Arguments 在 Gemini 里是对象，不是字符串。
//
// PartialArgs 与 WillContinue 是流式工具调用的增量形态：参数按 JSONPath 分片下发，
// 最后一个分片把 willContinue 置为假。非流式响应里两者都不出现。
type wireFunctionCall struct {
	ID           string           `json:"id,omitempty"`
	FunctionName string           `json:"name"`
	Arguments    any              `json:"args,omitempty"`
	PartialArgs  []wirePartialArg `json:"partialArgs,omitempty"`
	WillContinue *bool            `json:"willContinue,omitempty"`
}

// wireFunctionResponse 是一次工具调用的结果回填。
//
// Response 在 Gemini 里是对象；上游要求它可解析成对象，因此回填时不是对象的文本会被
// 包成 {"content": <原文>}。ID 用 RawMessage：旧版上游把它当字符串、新版按任意 JSON 回显，
// 原样保留避免因类型不符把整条请求判为非法。
type wireFunctionResponse struct {
	Name     string          `json:"name,omitempty"`
	Response map[string]any  `json:"response"`
	ID       json.RawMessage `json:"id,omitempty"`
}

// wirePartialArg 是流式工具调用的一个参数分片。
type wirePartialArg struct {
	JSONPath     string          `json:"jsonPath"`
	NumberValue  *float64        `json:"numberValue,omitempty"`
	StringValue  *string         `json:"stringValue,omitempty"`
	BoolValue    *bool           `json:"boolValue,omitempty"`
	NullValue    json.RawMessage `json:"nullValue,omitempty"`
	WillContinue *bool           `json:"willContinue,omitempty"`
}

// wireGenerationConfig 是生成参数。
//
// 只建模统一内部格式能表达的部分：温度与输出上限。responseMimeType、thinkingConfig
// 一类字段客户端可以在同协议透传路径上原样带到上游（本适配器不改写它们），
// 但跨协议重建时不产出——别的协议表达不了这些约束。
type wireGenerationConfig struct {
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"topP,omitempty"`
	TopK            *float64 `json:"topK,omitempty"`
	MaxOutputTokens *int     `json:"maxOutputTokens,omitempty"`
	StopSequences   []string `json:"stopSequences,omitempty"`
	CandidateCount  *int     `json:"candidateCount,omitempty"`
}

// wireTool 是一个工具集合。Gemini 把一组函数声明放在同一个 functionDeclarations 数组里。
type wireTool struct {
	FunctionDeclarations []wireFunctionDeclaration `json:"functionDeclarations"`
}

// wireFunctionDeclaration 是一个可调用函数的声明。
//
// Parameters 用 any：清洗后的 Schema 已经是任意 JSON，保留原文不再二次解析。
type wireFunctionDeclaration struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}

// wireToolConfig 是工具选择配置。
type wireToolConfig struct {
	FunctionCallingConfig *wireFunctionCallingConfig `json:"functionCallingConfig,omitempty"`
}

// wireFunctionCallingConfig 是函数调用模式。
//
// Mode 取 AUTO / ANY / NONE 三个字面量；指定具体工具时用 ANY 加 AllowedFunctionNames。
type wireFunctionCallingConfig struct {
	Mode                 string   `json:"mode,omitempty"`
	AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
}

// wireSafetySetting 是一条安全阈值设置。
type wireSafetySetting struct {
	Category  string `json:"category"`
	Threshold string `json:"threshold"`
}

// ---------------------------------------------------------------------------
// 响应 wire 结构
// ---------------------------------------------------------------------------

// wireResponse 是 generateContent 与 streamGenerateContent 的响应体。
//
// 流式的每一帧也是这个形状的不完整版本：帧里可能只有候选的一个片段、只有结束原因、
// 或者只有用量。因此解码路径一律按「缺哪个字段就当哪个字段没给」处理。
type wireResponse struct {
	Candidates     []wireCandidate     `json:"candidates"`
	PromptFeedback *wirePromptFeedback `json:"promptFeedback,omitempty"`
	UsageMetadata  *wireUsageMetadata  `json:"usageMetadata,omitempty"`
	// ModelVersion 是上游实际履约的模型，例如 gemini-2.5-flash-001。
	ModelVersion string `json:"modelVersion,omitempty"`
	// ResponseID 是新版上游给出的响应标识；旧版没有该字段。
	ResponseID string `json:"responseId,omitempty"`
}

// wireCandidate 是一个候选回复。统一内部格式只保留第一个候选。
type wireCandidate struct {
	Content       wireContent        `json:"content"`
	FinishReason  *string            `json:"finishReason,omitempty"`
	Index         int                `json:"index,omitempty"`
	SafetyRatings []wireSafetyRating `json:"safetyRatings,omitempty"`
}

// wireSafetyRating 是一条安全评级，解码时只用于判定内容过滤。
type wireSafetyRating struct {
	Category    string `json:"category"`
	Probability string `json:"probability"`
}

// wirePromptFeedback 是输入侧被拦截时的反馈。
type wirePromptFeedback struct {
	BlockReason   *string            `json:"blockReason,omitempty"`
	SafetyRatings []wireSafetyRating `json:"safetyRatings,omitempty"`
}

// wireUsageMetadata 是上游给出的用量。
//
// 口径与统一内部格式的对应关系：
//   - 输入侧：promptTokenCount 与 toolUsePromptTokenCount 相加（后者是服务端工具调用占用的输入），
//     cachedContentTokenCount 是其中的命中缓存子项；
//   - 输出侧：candidatesTokenCount 与 thoughtsTokenCount 相加（推理 token 在 Gemini 里不计入候选），
//     thoughtsTokenCount 同时是其中的推理子项。
type wireUsageMetadata struct {
	PromptTokenCount        int `json:"promptTokenCount"`
	ToolUsePromptTokenCount int `json:"toolUsePromptTokenCount,omitempty"`
	CandidatesTokenCount    int `json:"candidatesTokenCount"`
	TotalTokenCount         int `json:"totalTokenCount,omitempty"`
	ThoughtsTokenCount      int `json:"thoughtsTokenCount,omitempty"`
	CachedContentTokenCount int `json:"cachedContentTokenCount,omitempty"`
}

// wireError 是 Gemini 的错误响应体。
type wireError struct {
	Error wireErrorDetail `json:"error"`
}

// wireErrorDetail 是错误详情。Status 是 Google 的规范状态名（INVALID_ARGUMENT 等）。
type wireErrorDetail struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status,omitempty"`
}

// marshalRaw 把任意值编码为压缩后的 JSON；编码失败时返回 nil。
//
// 用于函数参数与 Schema 一类「原样带走」的字段：编码失败只可能来自调用方传入的
// 不可序列化类型，由调用方按各自的口径兜底。
func marshalRaw(value any) json.RawMessage {
	if value == nil {
		return nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return encoded
}
