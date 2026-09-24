package openaichat

import "encoding/json"

// 本文件定义 OpenAI Chat Completions 线协议的 JSON 结构，只列出转换所需字段。
// 解码对未知字段宽容：encoding/json 默认忽略未声明的字段。

const (
	// objectChatCompletion 与 objectChatCompletionChunk 是响应的 object 取值。
	objectChatCompletion      = "chat.completion"
	objectChatCompletionChunk = "chat.completion.chunk"

	// toolTypeFunction 是 OpenAI 目前唯一支持的工具类型。
	toolTypeFunction = "function"

	// roleDeveloper 是 OpenAI Chat 用于承载系统提示的角色，归一化为内部 system。
	roleDeveloper = "developer"

	// contentPartText 与 contentPartImageURL 是 content 数组片段的 type 取值。
	contentPartText     = "text"
	contentPartImageURL = "image_url"
)

// requestWire 是 POST /v1/chat/completions 的请求体。
type requestWire struct {
	RequestID  string               `json:"request_id"`
	Model      string               `json:"model"`
	Messages   []requestMessageWire `json:"messages"`
	Tools      []requestToolWire    `json:"tools"`
	ToolChoice json.RawMessage      `json:"tool_choice"`
	MaxTokens  *int                 `json:"max_tokens"`
	// MaxCompletionTokens 是 max_tokens 的替代字段，新模型只支持它。
	MaxCompletionTokens *int     `json:"max_completion_tokens"`
	Temperature         *float64 `json:"temperature"`
	Stream              bool     `json:"stream"`
	// User 是 OpenAI 客户端自带的终端用户标识；非空时作为会话标识参与前缀指纹拼装。
	User string `json:"user"`
}

type requestMessageWire struct {
	Role       string                `json:"role"`
	Content    json.RawMessage       `json:"content"`
	ToolCalls  []requestToolCallWire `json:"tool_calls"`
	ToolCallID string                `json:"tool_call_id"`
}

type requestToolCallWire struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type requestToolWire struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// contentPartWire 是 messages[].content 数组中的一个片段。
type contentPartWire struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	ImageURL json.RawMessage `json:"image_url"`
}

// responseWire 是非流式响应体。
type responseWire struct {
	ID      string           `json:"id"`
	Object  string           `json:"object"`
	Created int64            `json:"created"`
	Model   string           `json:"model"`
	Choices []responseChoice `json:"choices"`
	Usage   *usageWire       `json:"usage,omitempty"`
}

type responseChoice struct {
	Index        int             `json:"index"`
	Message      responseMessage `json:"message"`
	FinishReason string          `json:"finish_reason"`
}

type responseMessage struct {
	Role    string  `json:"role"`
	Content *string `json:"content"`
	// ReasoningContent 是部分兼容渠道放在 message 上的推理内容，不属于官方规范；
	// 取到非空文本时归一化为 domain.PartReasoning。
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCalls        []toolCallWire `json:"tool_calls,omitempty"`
}

type toolCallWire struct {
	ID       string               `json:"id"`
	Type     string               `json:"type"`
	Function toolCallFunctionWire `json:"function"`
}

type toolCallFunctionWire struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// chunkWire 是流式响应中的一帧 JSON 负载（不含 "data: " 前缀与结尾空行）。
type chunkWire struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model,omitempty"`
	Choices []chunkChoice `json:"choices"`
	Usage   *usageWire    `json:"usage,omitempty"`
	// Error 是上游在流式帧里返回的错误体；非空表示该帧是错误帧而非内容帧。
	Error *errorBody `json:"error,omitempty"`
}

type chunkChoice struct {
	Index        int        `json:"index"`
	Delta        chunkDelta `json:"delta"`
	FinishReason *string    `json:"finish_reason"`
}

type chunkDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
	// ReasoningContent 是部分兼容渠道放在 delta 上的推理增量，不属于官方规范；
	// 取到非空文本时产出 ChunkReasoningDelta。
	ReasoningContent string              `json:"reasoning_content,omitempty"`
	ToolCalls        []toolCallDeltaWire `json:"tool_calls,omitempty"`
}

type toolCallDeltaWire struct {
	Index    int                        `json:"index"`
	ID       string                     `json:"id,omitempty"`
	Type     string                     `json:"type,omitempty"`
	Function *toolCallFunctionDeltaWire `json:"function,omitempty"`
}

type toolCallFunctionDeltaWire struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// usageWire 是 token 用量。微元与金额不在此协议层表达。
//
// 非流式响应与流式用量帧使用同一个用量结构，因此本类型同时用于 responseWire 与 chunkWire。
type usageWire struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`

	PromptTokensDetails     *promptTokensDetailsWire     `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *completionTokensDetailsWire `json:"completion_tokens_details,omitempty"`
}

// promptTokensDetailsWire 是 usage.prompt_tokens_details。
//
// cached_tokens 是命中缓存的 prompt token，已包含在 prompt_tokens 内；
// cache_write_tokens 是写入缓存的 prompt token。两者都不再加进 total_tokens。
type promptTokensDetailsWire struct {
	CachedTokens     int `json:"cached_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
}

// completionTokensDetailsWire 是 usage.completion_tokens_details。
//
// reasoning_tokens 是用于推理的 completion token，已包含在 completion_tokens 内。
type completionTokensDetailsWire struct {
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}

// errorEnvelope 与 errorBody 是错误响应体结构。
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}
