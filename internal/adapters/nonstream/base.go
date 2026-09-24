// Package nonstream 为不支持流式交互的协议适配器提供 domain.Adapter 的流式方法缺省实现。
//
// 嵌入与重排是单次请求—响应交互，协议本身没有 SSE（Server-Sent Events，服务器发送事件）分帧。
// 共享转发层按 req.Stream 选择流式或非流式入口，本包的缺省实现让这类协议在适配器的
// DecodeRequest 拒绝 stream:true 之后，仍能完整满足 domain.Adapter 接口，
// 而不必让每个非流式协议各自复制一份「流式方法不产出任何分片」的样板代码。
//
// 本包的实现只被适配器以嵌入字段方式复用，不单独实现任何协议。
package nonstream

import (
	"encoding/json"
	"net/http"

	"github.com/sumwai/nova/internal/domain"
)

// modelField 是各协议请求体里的顶层模型名字段。非流式协议的同协议透传路径只改写这一处。
const modelField = "model"

const (
	// contentTypeJSON 是非流式响应的 Content-Type。
	contentTypeJSON = "application/json"
	// contentTypeSSE 是流式响应的 Content-Type。非流式协议不产出该响应，
	// 该取值只为满足 domain.Adapter 的接口约定。
	contentTypeSSE = "text/event-stream"
)

// Base 提供非流式协议在 domain.Adapter 上的流式方法缺省实现：一律不产出任何分片。
//
// 它不实现 Protocol、NewStream、DecodeRequest、DecodeResponse 与 EncodeError：
// 这些方法承载协议差异，必须由各协议适配器自己实现。NewStream 不在本包实现，
// 因为接口要求它返回适配器自身，而嵌入字段无法知道外层类型。
type Base struct{}

// 编译期断言：流式方法集合与 domain.Adapter 的约定一致。完整接口由各适配器的自身断言锁定。
var (
	_ domain.UpstreamRequestBuilder = (*Base)(nil)
	_ domain.UpstreamHeaderProvider = (*Base)(nil)
	_ domain.StreamErrorEncoder     = (*Base)(nil)
)

// ContentType 返回非流式响应的 Content-Type。
func (Base) ContentType() string { return contentTypeJSON }

// StreamContentType 返回流式响应的 Content-Type。
func (Base) StreamContentType() string { return contentTypeSSE }

// EncodeResponse 报告本协议不支持跨协议重建响应。
//
// 非流式协议只能与同协议渠道直连（见 domain.Protocol.CrossProtocolRebuildable），
// 跨协议重建路径在选路阶段就被排除；走到这里说明装配或选路违反了该前提，
// 按平台内部错误失败，而不是产出上游无法识别的响应体。
func (Base) EncodeResponse(*domain.Response) ([]byte, error) {
	return nil, domain.NewError(domain.CodeInternal, "本协议不支持跨协议重建响应")
}

// EncodeRequest 报告本协议不支持跨协议重建请求。
//
// 同协议转发走 RewriteRawBody，不经过本方法；本方法被调用即说明上游渠道协议与请求协议不一致，
// 按平台内部错误失败，而不是产出一份语义错误的上游请求。
func (Base) EncodeRequest(*domain.Request, domain.RewriteOptions) ([]byte, domain.RewriteParts, error) {
	return nil, nil, domain.NewError(domain.CodeInternal, "本协议不支持跨协议重建请求")
}

// UpstreamHeaders 返回调用上游时应携带的请求头：非流式协议没有内置必需头，
// 因此原样回传渠道级配置；nil 入参返回非 nil 的空 map，保证调用方可以直接写入。
func (Base) UpstreamHeaders(channelHeaders http.Header) http.Header {
	return domain.CloneHeader(channelHeaders)
}

// RewriteRawBody 对同协议透传的报文只改写顶层模型名。
//
// 非流式协议没有输出上限与索取用量开关一类可改写字段，模型名是唯一改写项：
// 渠道把对外模型别名映射到上游模型名时，必须把它替换进上游请求体。
func (Base) RewriteRawBody(body []byte, options domain.RewriteOptions) ([]byte, domain.RewriteParts, error) {
	if options.UpstreamModel == "" {
		return body, nil, nil
	}
	fields, err := domain.DecodeRawFields(body)
	if err != nil {
		return nil, nil, err
	}
	if !domain.SetRawStringField(fields, modelField, options.UpstreamModel) {
		return body, nil, nil
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return nil, nil, domain.NewError(domain.CodeInternal, "编码改写后的请求失败").WithCause(err)
	}
	return encoded, domain.RewriteParts{domain.RewritePartRequestModel}, nil
}

// EncodeStreamStart 返回流开始帧；非流式协议没有开始帧，返回 nil。
func (Base) EncodeStreamStart(string) ([]byte, error) { return nil, nil }

// EncodeChunk 返回流式帧；非流式协议没有增量帧，返回 nil。
func (Base) EncodeChunk(domain.Chunk) ([]byte, error) { return nil, nil }

// EncodeStreamEnd 返回流结束帧；非流式协议没有结束帧，返回 nil。
func (Base) EncodeStreamEnd(domain.Chunk) ([]byte, error) { return nil, nil }

// DecodeStreamFrame 解码上游流式帧；非流式协议的上游不会返回 SSE 帧，返回空分片。
func (Base) DecodeStreamFrame(string, []byte) ([]domain.Chunk, error) { return nil, nil }

// FinishStream 在 EOF 处给出收尾分片；非流式协议没有流式收尾，恒返回空。
func (Base) FinishStream() []domain.Chunk { return nil }

// PromptUsage 把只有输入侧计数的协议用量归一化为内部用量。
//
// 嵌入与重排都只有输入侧 token：promptTokens 为正时取它，否则回退 totalTokens；
// 负计数按「未给出」钳为 0，避免负 token 进入结算；两项都取不到时返回零值用量，
// 其来源为「未取得用量」，调用方据此走估算兜底，不得按零结算。
func PromptUsage(promptTokens, totalTokens int) domain.Usage {
	input := promptTokens
	if input < 0 {
		input = 0
	}
	if input == 0 && totalTokens > 0 {
		input = totalTokens
	}
	return domain.Usage{
		Source:      domain.UsageSourceUpstream,
		InputTokens: input,
	}
}

// EncodeStreamError 报告本协议没有流式错误帧。
//
// 非流式协议的失败一律经非流式错误响应表达，调用方据此按「结束后关闭连接」降级处理。
func (Base) EncodeStreamError(error) ([]byte, bool) { return nil, false }
