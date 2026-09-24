package domain

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// Adapter 负责一种线协议与内部统一格式的双向转换。
//
// 三种协议各自实现本接口：
//
//   - OpenAI Chat Completions（POST /v1/chat/completions）；
//   - OpenAI Responses（POST /v1/responses）；
//   - Anthropic Messages（POST /v1/messages）。
//
// 流水线只依赖本接口，不感知协议细节。
//
// 同一适配器同时用于两个方向。
//
// 面向客户端：
//
//   - 请求解码：DecodeRequest；
//   - 响应编码：EncodeResponse；
//   - 流式开始编码：EncodeStreamStart；
//   - 流式分片编码：EncodeChunk；
//   - 流式结束编码：EncodeStreamEnd；
//   - 错误编码：EncodeError；
//   - 内容类型：ContentType / StreamContentType。
//
// 面向上游：
//
//   - 响应解码：DecodeResponse / DecodeStreamFrame；
//   - 流式收尾：FinishStream。
//
// 方法集合在契约测试中被锁定：新增或删除方法必须同步更新测试，
// 避免适配器实现各自补私有方法而破坏统一转发流水线。
//
// 解码失败必须返回 domain.Error，错误码用 CodeInvalidRequest。
type Adapter interface {
	// Protocol 返回本适配器对应的线协议。
	Protocol() Protocol
	// NewStream 派生一个用于单次流式响应的适配器实例。
	//
	// 有状态的协议（例如 Anthropic 必须用 message_start 带上内容块下标）需要
	// 一次流一个实例；无状态实现可以直接返回自身。调用方必须为每条并发流
	// 单独调用 NewStream，不得把同一个实例用于多条并发流。
	//
	// 派生出的实例可以继承基础实例上的请求级信息；因此调用方必须先对**同一个基础实例**解码本次请求，
	// 再为本次流派生实例；基础实例不得跨请求复用，否则并发流会串用彼此的请求级信息。
	// 响应侧的模型名不由本机制传递，而是作为EncodeStreamStart 的入参由重建侧给出。
	NewStream() Adapter

	// DecodeRequest 把客户端请求体解码为内部统一请求。RequestID 由调用方注入，若请求体中已带则沿用。
	DecodeRequest(body []byte) (*Request, error)
	// EncodeResponse 把内部统一响应编码为对客户端响应体。
	EncodeResponse(resp *Response) ([]byte, error)
	// EncodeStreamStart 返回流开始帧；无需开始帧的协议返回 nil。
	//
	// model 是本次重建要写入开始帧的上游模型名，由重建侧传入。OpenAI Chat 与
	// OpenAI Responses 忽略入参；Anthropic Messages 用它写入
	// message_start.message.model。
	EncodeStreamStart(model string) ([]byte, error)
	// EncodeChunk 把一个内部统一分片编码为对客户端的流式帧（含协议自身的帧格式）。
	//
	// 约定：
	//   - 本方法只处理 ChunkTextDelta 与 ChunkToolCallDelta；
	//   - 推理分片 ChunkReasoningDelta 在编码方向不下发，实现应显式跳过而不报错；
	//   - 用量、结束原因与流结束一律经 EncodeStreamEnd 的结束分片下发，避免同一语义出现两种下发路径。
	EncodeChunk(chunk Chunk) ([]byte, error)
	// EncodeStreamEnd 把结束分片（Kind 为 ChunkStreamEnd，可携带用量与结束原因）编码为流结束帧；
	// 无需结束帧的协议返回 nil。不带用量的结束分片同样接受，是否输出用量字段由各协议自定
	// （例如 Anthropic 会写一个全零的 usage 对象，因为该协议要求 message_delta 必带 usage）。
	// 协议本身需要多个结束帧时（例如 Anthropic的 message_delta 加 message_stop），可拼接后一并返回。
	EncodeStreamEnd(chunk Chunk) ([]byte, error)
	// EncodeError 把错误编码为对客户端的 HTTP 状态码与响应体。
	EncodeError(err error) (status int, body []byte)
	// ContentType 与 StreamContentType 分别返回非流式与流式响应的 Content-Type。
	ContentType() string
	StreamContentType() string

	// DecodeResponse 把上游返回的完整响应体解码为内部统一响应。
	DecodeResponse(body []byte) (*Response, error)
	// DecodeStreamFrame 把上游返回的一帧流式数据解码为若干内部统一分片。
	// event 为协议事件名（无事件名的协议传空串），data 为帧载荷。
	//
	// 之所以返回切片而不是单个分片：上游可以在同一帧里给出多个载荷
	// （例如 OpenAI 的并行工具调用把多个 tool_calls 放进同一帧），
	// 单个分片的签名会迫使实现静默丢弃其余载荷。
	//
	// 约定：
	//   - 不产生分片时（例如心跳或纯控制帧）实现必须返回 nil 而非空切片，使三个适配器的形状一致、
	//     便于日志与观测统一处理；调用方仍一律以 len(chunks) == 0 判断，不得直接与 nil 比较。
	//   - 同类型载荷按协议数组顺序返回；文本与工具调用出现在同一帧时，先返回文本分片，
	//     再按数组顺序返回各工具调用分片。JSON 对象解码后键序已丢失，故不要求也不可能复现原始键序。
	DecodeStreamFrame(event string, data []byte) ([]Chunk, error)
	// FinishStream 在上游流到达 EOF 时给出本轮收尾分片。
	//
	// 调用方（共享转发层）在读取上游流的循环退出后调用一次，用于回答「这条流是完整结束还是被截断」
	// ——该事实只有读到 EOF 才知道，故不能提前到某一帧内判断。
	//
	// 语义：正常结束时返回恰好一个 ChunkStreamEnd，携带本轮已缓存的模型名、用量与结束
	// 原因，用量对象非 nil（未取得用量时为零值未知）；判定为截断时返回空（nil）。
	// 实现方在本方法内只允许产出 ChunkStreamEnd，不得产出内容、工具调用或用量分片。
	//
	// 幂等由适配器内部的「本轮已产出结束分片」标志承担：协议自身既有真实终止帧、又有
	// 可选哨兵帧时（例如 OpenAI Chat 的 [DONE]），两条路径合计只能产出一个 ChunkStreamEnd。
	// 调用方不得承担按协议去重的职责（不得解析帧、不得自行判断哪条路径该产出）；它只按
	// 一个与协议无关的事实决定是否交付 EOF 收尾分片：本轮是否已产出过 ChunkStreamEnd
	// （见 internal/upstream 的 sawEnd 判据）。该标志与累计状态分开保存：它在同一轮的
	// [DONE] 分支清零累计状态后仍须保持为真，但必须在下一轮起始处复位，否则跨轮复用实例时，
	// 新一轮的 FinishStream 会被上一轮的标志挡住而返回空，被误判为截断。
	//
	// 不采用带 error 的签名：本方法只读实例内已累计的内存状态，没有失败模式；截断由
	// 「本轮是否产出过 ChunkStreamEnd」这一唯一信号表达，不应再引入第二套信号。
	// 结束信号与哨兵帧的区分、以及各协议的实现口径由适配器各自实现。
	FinishStream() []Chunk
}

// CredentialHeaderStyle 描述调用上游时凭据请求头的注入形态。
//
// 取值由上游渠道配置给出，只决定「用哪个请求头、以什么形态拼装凭据」，
// 不含凭据本身；凭据仍按协议从环境变量读入，不因本类型改变来源。
//
// 零值 CredentialHeaderAuto 表示「渠道未配置」，此时按协议现状注入：
// OpenAI Chat 与 OpenAI Responses 用 Authorization: Bearer、Anthropic Messages 用
// x-api-key。取值域只有两个非零取值，「未配置」在数据里只能以「键缺失」表达，
// 因此零值不是合法配置值（Valid 对它返回 false）。
type CredentialHeaderStyle string

const (
	// CredentialHeaderAuto 是零值：渠道未配置该项，按协议现状注入。
	CredentialHeaderAuto CredentialHeaderStyle = ""
	// CredentialHeaderAuthorization 注入 Authorization: Bearer <凭据>。
	CredentialHeaderAuthorization CredentialHeaderStyle = "authorization"
	// CredentialHeaderXAPIKey 注入 x-api-key: <凭据>。
	CredentialHeaderXAPIKey CredentialHeaderStyle = "x-api-key"
)

// Valid 报告取值是否为两个受支持的非零取值之一。
//
// 零值（未配置）返回 false：配置写入路径必须把「键缺失」与「键存在但取值非法」
// 分开处理，不得把零值当成一个可写入的取值。
func (s CredentialHeaderStyle) Valid() bool {
	switch s {
	case CredentialHeaderAuthorization, CredentialHeaderXAPIKey:
		return true
	default:
		return false
	}
}

// Route 是一次选路的结果，描述一个可用的上游渠道。
//
// UpstreamID / Protocol / UpstreamModel / BaseURL / Timeout / OutputLimit 是路由必需事实；
// CredentialRef 是不透明引用，明文凭据由装配层的请求头提供者按它解析后注入，Route 不得携带明文。
type Route struct {
	UpstreamID string
	// Protocol 是上游渠道使用的线协议。调用方据此判定客户端协议与上游协议是否一致，
	// 从而决定走同协议透传还是按内部统一格式重建。
	Protocol      Protocol
	UpstreamModel string
	BaseURL       string
	// CredentialRef 是不透明引用，装配层的请求头提供者按它取出明文凭据并注入请求头。
	// 它本身不携带明文，以免随日志泄露。
	CredentialRef string
	// AccountRef 是渠道内账号的引用，形如 "#2"；空串表示该渠道的默认账号
	// （即声明顺序里的第一个）。它只在渠道声明了多个账号时非空：单账号时
	// 留空串，日志、熔断键与折叠键因此与多账号之前的输出逐字一致。
	AccountRef string
	// Headers 是渠道级静态请求头，与凭据头合并后交适配器补协议内置必需头。
	// nil 表示该渠道没有额外请求头。
	Headers http.Header
	// Timeout 是调用本渠道上游的超时预算。同一个字段按转发形态解释为两种语义：
	//
	//   - 非流式（UpstreamCaller.Complete）：整段上游调用的截止时间，覆盖连接、请求与响应体读取；
	//   - 流式（UpstreamCaller.Stream）：最长无进展时间（空闲超时），底层每有字节到达即重新计时。
	//     注释行与纯空块心跳不产生帧，但同属有进展，按字节到达重置计时。
	//
	// 流式不能用整段截止时间，否则长生成会因整段耗时超过预算而被误杀；
	// 空闲超时既能在上游挂死时及时取消，又不限制正常的长流。
	// 「整段流时长」属传输侧概念，本项目不设该预算。
	Timeout time.Duration
	// OutputLimit 是该渠道 × 该上游模型的有效输出上限；nil 表示未配置，
	// 形状与 RewriteOptions.MaxOutputTokens 一致。
	OutputLimit *int
	// CredentialHeaderStyle 是调用本渠道上游时凭据请求头的注入形态，由渠道配置给出；
	// 零值 CredentialHeaderAuto 表示未配置，按协议现状注入。
	CredentialHeaderStyle CredentialHeaderStyle
}

// BreakerKey 返回一次尝试在熔断与折叠口径下的渠道键。
//
// 声明了账号池的渠道按「渠道 + 账号」分开计：一个账号被限流不应该把整条渠道熔断，
// 而同一账号的重复失败仍然归到同一个窗口里折叠。单账号时不加后缀，
// 键与账号池引入之前逐字一致。
func (r Route) BreakerKey() string {
	if r.AccountRef == "" {
		return r.UpstreamID
	}
	return r.UpstreamID + " " + r.AccountRef
}

// RouteResolver 返回按有效优先级升序排列的候选渠道列表。
//
// 契约：候选按有效优先级升序排列；同一优先级内顺序确定（同一实现重复调用必须给出完全相同的顺序），
// 但不要求两个实现给出同一顺序——internal/router 的内存实现同优先级按声明顺序。
//
// 返回多个候选是为了让流水线在可重试失败时按顺序回退。
// 返回空列表表示无可用渠道，调用方必须按不可重试错误处理。
type RouteResolver interface {
	Candidates(ctx context.Context, req *Request) ([]Route, error)
}

// UpstreamResult 是一次成功的上游非流式调用的结果。
//
// Raw 是上游返回的原始响应体字节（成功时非空），是同协议非流式路径面向客户端的
// 唯一字节来源；Response 是同一份字节经上游协议适配器 DecodeResponse 归一化的结果，
// 供用量记账与跨协议重建使用。两者表达同一份响应，不允许分别来自两次读取。
type UpstreamResult struct {
	Raw      []byte
	Response *Response
}

// UpstreamCaller 调用一个具体上游渠道，并返回归一化结果。
//
// 两个方法都必须把上游错误转换为 domain.Error，并据此标注可重试性。
type UpstreamCaller interface {
	// Complete 发送定稿后的上游请求体并返回归一化结果。
	//
	// body 是流水线定稿的上游请求体：同协议透传时是 RewriteRawBody 改写后的原始报文，
	// 跨协议时是 EncodeRequest 的重建结果。req 只用于观测归属与用量关联，
	// 调用方不得用 req 重新构造请求体。
	//
	// 返回的 UpstreamResult 同时携带上游原始响应体字节与归一化响应。面向客户端的响应体字节由流水线生产
	// （同协议写出 Raw、跨协议写出 EncodeResponse(Response)），本方法不写客户端字节。
	Complete(ctx context.Context, route Route, req *Request, body []byte) (*UpstreamResult, error)
	// Stream 与 Complete 的入参含义相同，改为把上游流式分片写入 sink。
	Stream(ctx context.Context, route Route, req *Request, body []byte, sink ChunkSink) error
}

// UpstreamRequestBuilder 把内部统一请求转换为上游协议请求体。
//
// 之所以新增独立接口而不继续给 Adapter 加方法：Adapter 的方法集合由
// internal/domain/contract_test.go 锁定，且已有三个协议实现；请求侧的构建能力
// 只有共享转发层需要，扩大 Adapter 会让三个协议实现与契约测试一起被迫改动。
type UpstreamRequestBuilder interface {
	// EncodeRequest 按内部统一格式重建上游请求体，用于跨协议转发路径。
	//
	// 模型名取 options.UpstreamModel：非空时用它，空串时用 req.Model。
	// 调用方不再靠修改 req.Model 传递上游模型名。
	//
	// 第二个返回值报告本次重建相对客户端请求实际改写过的部分，填报口径与RewriteRawBody 相同。
	EncodeRequest(req *Request, options RewriteOptions) ([]byte, RewriteParts, error)
	// RewriteRawBody 对同协议透传的原始报文做字段级改写。
	//
	// 没有任何改写项启用时必须逐字节返回入参。启用改写时只改动 RewriteOptions
	// 明确列出的字段，其余字段的取值保持原样。
	//
	// 改写项当前包含：
	//
	//   - 输出上限（max_tokens 等字段的钳制与补齐）；
	//   - 索取用量开关（stream_options 等字段的注入）；
	//   - 模型名（上游模型名的替换与补齐）。
	//
	// 模型名一项的口径：options.UpstreamModel 非空时替换或补齐原始报文的顶层模型字段。
	//
	// 第二个返回值由产生产物的适配器按实际发生的变化填报，调用方只负责把它与
	// 响应侧标注合并后写入 AttemptRecord.RewrittenParts。
	// 调用方不能按「产物与入参逐字节比对」自行判定：
	//
	//   - 要区分改写项必须知道各协议的字段名；
	//   - 「客户端是否已声明索取用量」只有解析过请求体的适配器知道。
	//
	// 填报判据：
	//
	//   - 只在产物相对客户端原始请求确实发生变化时才填对应取值；
	//   - 没有任何改写项启用、或启用了但实际未触发时，逐字节返回入参且不留任何标注；
	//   - 无法判定是否变化时不得猜测填写（宁缺勿假）。
	RewriteRawBody(body []byte, options RewriteOptions) ([]byte, RewriteParts, error)
}

// UpstreamHeaderProvider 提供调用上游时必须携带的请求头。
//
// 上游必需的请求头属于协议自身的表达能力（例如 Anthropic Messages 必须携带版本头），
// 不能由共享转发层硬编码；渠道配置可以在其之上覆盖或补充。
type UpstreamHeaderProvider interface {
	// UpstreamHeaders 返回调用上游时应携带的请求头：适配器内置必需头与channelHeaders（渠道配置）合并，
	// 同名时 channelHeaders 覆盖内置值，其余 channelHeaders 原样补充。返回新 map，不修改入参。
	//
	// channelHeaders 为 nil 时按空请求头处理，返回值必须是非 nil 的新 map 且调用方可直接写入。
	//
	// 使用标准库 http.Header 承载，避免 domain 依赖具体 HTTP 客户端库。
	UpstreamHeaders(channelHeaders http.Header) http.Header
}

// CloneHeader 返回标准库 http.Header 的一份独立拷贝。
//
// nil 入参返回非 nil 的空 map：http.Header 的 Clone 方法在接收者为 nil 时返回 nil，
// 调用方写入会 panic，与 UpstreamHeaderProvider「返回新 map」的契约不符。
func CloneHeader(header http.Header) http.Header {
	cloned := make(http.Header, len(header))
	for key, values := range header {
		cloned[key] = append([]string(nil), values...)
	}
	return cloned
}

// StreamErrorEncoder 把错误编码为协议自身的流式错误帧。
//
// 上游在结束标记出现前断开连接时，网关需向客户端下发该协议的流式错误帧而非直接关闭连接。
type StreamErrorEncoder interface {
	// EncodeStreamError 把 err 编码为本协议在流式阶段的错误帧。
	//
	// supported 为 false 表示该协议没有流式错误帧；此时 frame 为 nil，调用方必须按
	// 「无法下发结构化错误」降级处理（结束后关闭连接，或改用 EncodeError 的非流式错误响应），
	// 不得把 nil 当成已下发成功。
	EncodeStreamError(err error) (frame []byte, supported bool)
}

// ChunkSink 接收归一化流式分片。
//
// Send 返回错误表示下游已不可写（例如客户端断开）；调用方必须立即取消上游且不得继续读取。
type ChunkSink interface {
	Send(ctx context.Context, chunk Chunk) error
}

// FrameSink 接收上游一帧的原始字节与该帧解出的全部分片，用于响应侧按帧粒度原样透传。
//
// 下沉模式由 sink 是否实现本接口决定：实现时 UpstreamCaller.Stream 对每一帧只调用
// SendFrame，一次交付一帧的原始字节与该帧解出的全部分片（无分片时为空）；未实现时
// 只逐分片调用 Send。EOF 收尾分片（Adapter.FinishStream 的返回值）没有对应的上游帧、
// 没有原始字节可交付，两种模式下一律经 Send 交付，因此实现本接口时 Send 也并非
// 完全不被调用。消费者（共享转发流水线）按本接口命名该能力，不得在包内自造同义接口。
//
// 契约：
//   - raw 取自 SSE（Server-Sent Events，服务器发送事件）帧的原始字节，含字段行与结尾空行；
//     心跳与纯注释块不产生帧、不会被交付。
//   - raw 是面向客户端的唯一字节来源；chunks 只用于记账与观测，不得再被编码给客户端。
//   - 实现方不得就地修改该切片。
type FrameSink interface {
	ChunkSink
	// SendFrame 接收上游一帧的原始字节与该帧解出的全部分片。
	SendFrame(ctx context.Context, raw []byte, chunks []Chunk) error
}

// RoutedModelSink 接收本次实际履约的上游模型名。
//
// 跨渠道或跨协议转发后，响应体里的模型名可能不等于客户端请求的模型名；面向客户端的
// 写出目标实现本接口时，流水线把最终履约的模型名注入它（例如写成响应标头），
// 使客户端能读到网关实际履约的模型。
type RoutedModelSink interface {
	// SetRoutedModel 注入最终履约模型名；实现可忽略空串。
	SetRoutedModel(model string)
}

// AttemptOutcome 是一次上游尝试的结果分类。
type AttemptOutcome string

const (
	AttemptOK        AttemptOutcome = "ok"
	AttemptFailed    AttemptOutcome = "failed"
	AttemptCancelled AttemptOutcome = "cancelled"
)

// AttemptRecord 是一次上游尝试的可观测记录。
//
// 一次请求可能产生多条 AttemptRecord（重试与回退），与请求级记录共同构成排障所需的时间线。
//
// 本类型是「这次尝试到底发了什么、上游回了什么」的完整事实来源：没有数据库之后，
// 排查只能依靠这些字段，因此协议两侧、模型名两侧与上游用量都要照实记录，
// 而不是只留一个笼统的结果。
type AttemptRecord struct {
	RequestID string
	Attempt   int
	// ClientProtocol 是客户端使用的线协议，UpstreamProtocol 是本次尝试发往上游的线协议。
	// 两者不同即表示网关做了跨协议重建，使用方据此判定「这次转发转换过协议」。
	ClientProtocol   Protocol
	UpstreamProtocol Protocol
	// RequestedModel 是客户端请求里的模型名，UpstreamModel 是实际写入上游请求体的模型名。
	// 两者不同即表示网关改写过模型名。
	RequestedModel string
	UpstreamID     string
	UpstreamModel  string
	// AccountRef 是本次尝试使用的渠道内账号引用，空串表示单账号渠道。
	// 它与 UpstreamID 一起回答「这一条打到哪个账号上」：多账号排障时
	// 光看渠道名看不出是哪个账号在限流。
	AccountRef string
	Outcome    AttemptOutcome
	// Usage 是上游本次尝试陈述的用量；未取得时为来源未知的零值（见 Usage.Known）。
	Usage Usage
	// ErrorCode 是本次失败尝试的错误码；成功时为空串。
	ErrorCode string
	// ErrorDetail 是本次失败尝试的排障细节：上游返回的 HTTP 状态码与响应体片段，
	// 流式路径上是上游错误帧给出的原文。成功尝试或上游未返回可读报文时为空串。
	// 内容来自上游响应，不含本网关注入的凭据与请求头；长度由各生产端自行约束。
	ErrorDetail string
	StartedAt   time.Time
	EndedAt     time.Time
	// RewrittenParts 列出本次尝试中被网关改写过的报文部分；为空表示未改动任何部分。
	// 用于让「网关动过哪些部分」在尝试记录与日志中可审计。
	RewrittenParts RewriteParts
}

// Observer 记录一次上游尝试。实现失败不得影响转发结果，调用方可忽略其错误。
type Observer interface {
	RecordAttempt(ctx context.Context, rec AttemptRecord) error
}

// NonRetryableUpstreamErrorType 判断上游错误的类型名或机器码是否属于
// 「换渠道重试也不会成功」的一类：
//
//   - 参数不合规；
//   - 凭据或权限不足；
//   - 资源不存在。
//
// 三种线协议都用同一套字面量表达请求级错误，判据与协议无关，故放在统一协议包，
// 避免各适配器各写一份而漂移。
//
// 当前登记的字面量包括：
//
//   - invalid_request_error；
//   - authentication_error；
//   - permission_error；
//   - not_found_error。
//
// 其余字面量（含空串）一律返回 false，即按可重试处理，由共享转发层的重试上限兜底。
//
// 这与「未登记错误码按不可重试」的方向相反，属于有意的分工：
//   - 错误码兜底偏保守，避免打爆上游；
//   - 上游类型兜底偏可用，因为上游限流等瞬时故障换渠道可恢复。
func NonRetryableUpstreamErrorType(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "invalid_request_error", "authentication_error", "permission_error", "not_found_error":
		return true
	default:
		return false
	}
}
