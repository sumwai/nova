// Package openaichat 实现 OpenAI Chat Completions 线协议（POST /v1/chat/completions）
// 与内部统一格式（internal/domain）之间的转换。
//
// 它实现 domain.Adapter：请求侧把线协议解码为 domain.Request，响应侧把
// domain.Response 与 domain.Chunk 编码为线协议。协议专有的结束原因字面量映射
// 由 DecodeFinishReason 承担，未识别字面量一律归为 domain.FinishUnknown，
// 不得当作正常结束。
package openaichat

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/sumwai/nova/internal/domain"
)

// 编译期断言：Adapter 必须完整实现 domain.Adapter。
var _ domain.Adapter = (*Adapter)(nil)

const (
	contentTypeJSON = "application/json"
	// contentTypeSSE 是 SSE（Server-Sent Events，服务器发送事件）流式响应的 Content-Type。
	contentTypeSSE = "text/event-stream"

	// streamEndFrame 是 OpenAI 流式响应的结束帧。
	streamEndFrame = "data: [DONE]\n\n"

	// streamDoneMarker 是结束帧的载荷，解码上游流时需要单独识别。
	streamDoneMarker = "[DONE]"

	// internalErrorMessage 是非统一错误的对外兜底文案。
	internalErrorMessage = "内部错误"
)

// idSeq 为默认响应 id 提供进程内唯一序号。
var idSeq atomic.Uint64

// Adapter 实现 domain.Adapter，负责 OpenAI Chat Completions 协议的双向转换。
//
// now 与 newID 供测试注入确定性时钟与 id；生产代码用 New 取默认实现。
type Adapter struct {
	now   func() time.Time
	newID func() string

	// stream 是面向上游的流式解码跨帧累计状态。
	//
	// OpenAI 把结束原因与用量分成两帧下发，二者都要缓存到本轮收尾才合并成一个
	// ChunkStreamEnd（见 DecodeStreamFrame 与 FinishStream），因此本适配器不是无状态实现：
	// 一次流必须使用一个独立实例，见 NewStream。
	stream streamState
	// streamEnded 记录本轮是否已产出结束分片，用于保证 [DONE] 路径与 FinishStream 的
	// EOF 收尾路径合计只产出一个 ChunkStreamEnd。
	//
	// 它必须与 stream 分开保存：在同一轮的 [DONE] 分支中，它要在 stream 被清零之后仍保持为真。
	// 它必须在新一轮起始处复位（DecodeStreamFrame 函数起始处，以及首帧带 role 时的补充复位），
	// 否则跨轮复用实例时，新一轮的 FinishStream 会被上一轮的标志挡住而返回空，被误判为截断。
	streamEnded bool
}

// streamState 保存流式解码跨帧累计的结束原因、用量与模型。
type streamState struct {
	model        string
	usage        *domain.Usage
	finishReason domain.FinishReason
	// finishReasonSeen 记录本轮是否观察到 choices[0].finish_reason 非 null。
	// 它是 Chat 的必有结束信号：从未观察到就 EOF 属真截断，FinishStream 不得产出结束分片。
	finishReasonSeen bool
}

// NewStream 为一条流式响应派生独立实例。
//
// 本适配器的解码路径需要跨帧缓存结束原因与用量，不能把同一实例用于多条并发流，
// 故这里返回一个复制了时钟与 id 生成器、但清零流式累计状态的新实例
// （见 internal/domain/ports.go 对 NewStream 的约定）。
func (a *Adapter) NewStream() domain.Adapter {
	clone := *a
	clone.stream = streamState{}
	clone.streamEnded = false
	return &clone
}

// New 返回使用系统时钟与进程内自增 id 的适配器。
func New() *Adapter {
	return &Adapter{now: time.Now, newID: defaultID}
}

func defaultID() string {
	return fmt.Sprintf("chatcmpl-%d", idSeq.Add(1))
}

func (a *Adapter) id() string {
	if a.newID == nil {
		return defaultID()
	}
	return a.newID()
}

func (a *Adapter) timestamp() int64 {
	if a.now == nil {
		return time.Now().Unix()
	}
	return a.now().Unix()
}

// Protocol 返回本适配器对应的对外协议。
func (a *Adapter) Protocol() domain.Protocol {
	return domain.ProtocolOpenAIChat
}

// ContentType 返回非流式响应的 Content-Type。
func (a *Adapter) ContentType() string {
	return contentTypeJSON
}

// StreamContentType 返回流式响应的 Content-Type。
func (a *Adapter) StreamContentType() string {
	return contentTypeSSE
}

// EncodeStreamStart 返回流开始帧。
//
// OpenAI Chat Completions 的流以首个 chat.completion.chunk 开始，没有独立的开始帧，
// 因此返回 (nil, nil)；调用方不得写出空帧。模型名随每个增量分片自带，本方法忽略入参。
func (a *Adapter) EncodeStreamStart(string) ([]byte, error) {
	return nil, nil
}

// EncodeStreamEnd 把结束分片编码为流结束帧。
//
// 结束分片可携带结束原因与用量：有结束原因时先出携带 finish_reason 的分片，
// 有用量时再出 choices 为空、usage 非空的分片，最后固定输出 data: [DONE]。
// 这两类尾部帧在 OpenAI 的真实流里分别独立成帧，故一次调用可能返回多帧拼接。
// 结束分片为空时只返回 data: [DONE]。
//
// 用量与结束原因的产帧逻辑不对外暴露：EncodeChunk 只处理文本与工具调用增量，
// 不得从其它路径下发用量或结束原因（见 internal/domain/ports.go 对 EncodeChunk 的约定）。
func (a *Adapter) EncodeStreamEnd(chunk domain.Chunk) ([]byte, error) {
	frames := make([]byte, 0, len(streamEndFrame))
	if chunk.FinishReason != "" {
		encoded, err := a.encodeFinishFrame(chunk.Model, chunk.FinishReason)
		if err != nil {
			return nil, err
		}
		frames = append(frames, encoded...)
	}
	if chunk.Usage != nil {
		encoded, err := a.encodeUsageFrame(chunk.Model, chunk.Usage)
		if err != nil {
			return nil, err
		}
		frames = append(frames, encoded...)
	}
	frames = append(frames, streamEndFrame...)
	return frames, nil
}

// EncodeError 把错误编码为 HTTP 状态码与 OpenAI 错误体。
//
// 状态码取 domain.HTTPStatus。错误分级映射到错误体 type：
//   - 用户侧为 invalid_request_error
//   - 上游侧为 upstream_error
//   - 其余为 api_error
//
// code 取统一错误码；非 domain.Error 一律按内部错误兜底。
func (a *Adapter) EncodeError(err error) (int, []byte) {
	status := domain.HTTPStatus(err)
	body := errorBody{
		Message: internalErrorMessage,
		Type:    "api_error",
		Code:    string(domain.CodeInternal),
	}
	var domainErr *domain.Error
	if errors.As(err, &domainErr) {
		body.Message = domainErr.Message
		body.Code = string(domainErr.Code)
		body.Type = errorType(domainErr.Class)
	}
	encoded, marshalErr := json.Marshal(errorEnvelope{Error: body})
	if marshalErr != nil {
		return http.StatusInternalServerError, []byte(`{"error":{"message":"内部错误","type":"api_error","code":"internal"}}`)
	}
	return status, encoded
}

func errorType(class domain.ErrorClass) string {
	switch class {
	case domain.ClassUser:
		return "invalid_request_error"
	case domain.ClassUpstream:
		return "upstream_error"
	default:
		return "api_error"
	}
}

// DecodeFinishReason 把 OpenAI 的 finish_reason 字面量映射为内部枚举。
//
// 支持的结束原因字面量包括：
//   - stop：正常停止
//   - length：达到最大输出长度
//   - tool_calls：触发工具调用
//   - content_filter：内容风控拦截
//
// 其余字面量（含空串）一律返回 domain.FinishUnknown，绝不退化为正常结束。
func DecodeFinishReason(literal string) domain.FinishReason {
	switch literal {
	case string(domain.FinishStop):
		return domain.FinishStop
	case string(domain.FinishLength):
		return domain.FinishLength
	case string(domain.FinishToolCalls):
		return domain.FinishToolCalls
	case string(domain.FinishContentFilter):
		return domain.FinishContentFilter
	default:
		return domain.FinishUnknown
	}
}
