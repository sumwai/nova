// Package transport 提供所有协议共用的 HTTP（HyperText Transfer Protocol，超文本传输协议）
// 入口：按装配层注入的路径到适配器映射分发请求，读取并解码请求体，向流水线转发，
// 再把面向客户端的响应（含流式逐帧推送）写回。
//
// 本包对协议无感知：不出现任何协议字面量，不按协议名分支；
// 路径到适配器的映射与适配器实例都由装配层注入，协议差异只在适配器内表达。
//
// 流式推送的三个关键行为：
//
//  1. 每次写出后立即 Flush，避免帧被 HTTP 缓冲吞掉；
//  2. 每次写出前设置写超时，客户端超过该时长不读取时写操作失败、连接被切断，并把错误向上传播，由流水线取消上游；
//  3. 响应头固定声明不允许缓存与代理缓冲，避免中间层把逐帧推送攒成整段后一次性下发。
package transport

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sumwai/nova/internal/domain"
)

const (
	// defaultMaxBodyBytes 是客户端请求体的默认字节上限，超限按客户端错误拒绝。
	defaultMaxBodyBytes = 32 << 20
	// defaultWriteTimeout 是单次向客户端写出的默认最长等待时间：客户端超过该时长不读取时切断连接。
	defaultWriteTimeout = 10 * time.Second
	// contentTypeHeader 是响应与请求的 Content-Type 头名。
	contentTypeHeader = "Content-Type"
	// cacheControlHeader 是响应缓存指令头名。
	cacheControlHeader = "Cache-Control"
	// xAccelBufferingHeader 是 nginx 一类反向代理用来开关响应缓冲的头名。
	xAccelBufferingHeader = "X-Accel-Buffering"
	// sseCacheControl 是流式响应的 Cache-Control 取值：禁止缓存，并禁止中间层改写或压缩响应体
	// （no-transform），避免反向代理把逐帧推送攒成整段。
	sseCacheControl = "no-cache, no-transform"
	// xAccelBufferingDisabled 是关闭反向代理响应缓冲的取值。
	xAccelBufferingDisabled = "no"
	// routedModelHeader 是向客户端告知本次实际履约模型名的响应标头。
	// 渠道配置改写模型名后，响应体中的模型名与网关实际履约的模型可能不同，
	// 该标头使客户端能读到网关最终履约的模型名。
	routedModelHeader = "x-tokenmp-routed-model"
)

// Forwarder 是 transport 所需的转发表面，由 internal/pipeline 的实现满足。
//
// 面向客户端的响应体字节（非流式响应体与流式帧）均由流水线写入 out；
// 入口只负责设置 Content-Type、在尚未写出字节时把错误编码为响应，以及逐帧 Flush。
// 用接口而不是具体类型，是为了让入口层只依赖「转发」这一能力；生产装配注入真实流水线。
type Forwarder interface {
	Forward(ctx context.Context, client domain.Adapter, req *domain.Request, out io.Writer) error
}

// AccessRecord 是一条访问日志的内容：请求处理结束时由入口层填齐，
// 字段名由日志实现映射为结构化日志键。入口层只产出记录、不拼日志键，
// 使协议字段名守卫（协议路径、请求体字段名不得出现在共享转发层）与访问日志互不干扰。
type AccessRecord struct {
	// RequestID 是本次请求的关联键；被拒绝的请求可能为空。
	RequestID string
	// Protocol 是客户端协议；未注册路径为空。
	Protocol domain.Protocol
	// Model 是客户端请求里的模型名（对外别名），不是实际发往上游的模型名：
	// 后者由选路决定，记录在流水线的上游尝试记录里。
	Model string
	// Stream 报告本次是否为流式请求。
	Stream bool
	// HTTPStatus 是返回给客户端的 HTTP 状态码。
	HTTPStatus int
	// DurationMS 是从入口接管请求到响应写完的耗时（毫秒）。
	DurationMS int64
	// WrittenBytes 是已写给客户端的响应体字节数。
	WrittenBytes int
	// ErrorCode 是错误码；成功时为空串。
	ErrorCode string
	// RemoteAddr 是客户端地址。
	RemoteAddr string
	// UserAgent 是客户端自报的 User-Agent，已按上限截断；未自报时为空串。
	// 它只作排障线索：客户端可伪造，不得用于任何安全判定。
	UserAgent string
}

// AccessLogger 写一条访问日志；实现由装配层注入，nil 表示不记录。
type AccessLogger interface {
	LogAccess(record AccessRecord)
}

// AdapterResolver 按请求路径返回客户端适配器。装配层把路径到适配器的映射注入这里，
// 使本包不出现任何协议字面量。
type AdapterResolver func(path string) (domain.Adapter, bool)

// Options 是构造 Handler 的依赖与默认值。
type Options struct {
	// Forwarder 是转发流水线。必填。
	Forwarder Forwarder
	// Adapters 是路径到适配器的映射。必填。
	Adapters AdapterResolver
	// NewRequestID 生成注入请求的 request_id；为 nil 时用进程内随机标识符。
	NewRequestID func() string
	// MaxBodyBytes 是客户端请求体字节上限；<= 0 时取 defaultMaxBodyBytes。
	MaxBodyBytes int64
	// WriteTimeout 是单次写出的最长等待时间；<= 0 时取 defaultWriteTimeout。
	WriteTimeout time.Duration
	// Logger 写请求级访问日志；为 nil 时不记录。
	Logger AccessLogger
}

// Handler 是所有协议共用的 HTTP 入口。
type Handler struct {
	forwarder    Forwarder
	adapters     AdapterResolver
	newRequestID func() string
	maxBodyBytes int64
	writeTimeout time.Duration
	logger       AccessLogger
}

// New 构造入口；依赖缺失在构造时报出，不推迟到请求时。
func New(opts Options) (*Handler, error) {
	if opts.Forwarder == nil {
		return nil, domain.NewError(domain.CodeInternal, "缺少转发流水线")
	}
	if opts.Adapters == nil {
		return nil, domain.NewError(domain.CodeInternal, "缺少路径到适配器的映射")
	}
	handler := &Handler{
		forwarder:    opts.Forwarder,
		adapters:     opts.Adapters,
		newRequestID: opts.NewRequestID,
		maxBodyBytes: opts.MaxBodyBytes,
		writeTimeout: opts.WriteTimeout,
		logger:       opts.Logger,
	}
	if handler.newRequestID == nil {
		handler.newRequestID = defaultRequestID
	}
	if handler.maxBodyBytes <= 0 {
		handler.maxBodyBytes = defaultMaxBodyBytes
	}
	if handler.writeTimeout <= 0 {
		handler.writeTimeout = defaultWriteTimeout
	}
	return handler, nil
}

// ServeHTTP 处理一次客户端请求：解析路径得到适配器，校验方法，读取并解码请求体，
// 注入 request_id 后按是否流式分派。
//
// 请求结束时写一条访问日志。错误一律经适配器编码并记入错误码，成功与失败请求都可被观测。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := newResponseRecorder(w)
	state := accessState{remoteAddr: r.RemoteAddr, userAgent: truncateUserAgent(r.UserAgent())}
	defer func() {
		h.observeRequest(rec, start, state)
	}()

	adapter, ok := h.adapters(r.URL.Path)
	if !ok || adapter == nil {
		// 未注册路径没有可归的协议，适配器无法编码错误体，只能回标准库的纯文本 404；
		// 已注册路径的其它错误一律经适配器编码。
		http.NotFound(rec, r)
		return
	}
	state.protocol = adapter.Protocol()
	if r.Method != http.MethodPost {
		// 路径已解析出协议，故 405 与其它错误一样经适配器编码，保证错误体与协议一致。
		h.writeError(rec, adapter, methodNotAllowedError(http.MethodPost))
		return
	}
	// MaxBytesReader 的第一个参数必须是 ServeHTTP 收到的原始 http.ResponseWriter：
	// net/http 靠对未导出接口 requestTooLarger 做类型断言感知请求体超限
	// （超限时设置 Connection: close），包装后的写出器无法通过该断言，会让超限路径不再关连接。
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.maxBodyBytes))
	if err != nil {
		h.writeError(rec, adapter, domain.NewError(domain.CodeInvalidRequest, "读取请求体失败").WithCause(err))
		return
	}
	req, err := adapter.DecodeRequest(body)
	if err != nil {
		h.writeError(rec, adapter, err)
		return
	}
	if req.RequestID == "" {
		req.RequestID = h.newRequestID()
	}
	state.requestID = req.RequestID
	state.model = req.Model
	state.stream = req.Stream
	if req.Stream {
		h.serveStream(rec, r, adapter, req)
		return
	}
	h.serveComplete(rec, r, adapter, req)
}

// accessState 是访问日志所需、在请求处理过程中逐步填齐的字段。
type accessState struct {
	requestID  string
	protocol   domain.Protocol
	model      string
	stream     bool
	remoteAddr string
	userAgent  string
}

// observeRequest 在请求结束时写一条访问日志；未注入记录器时跳过。
func (h *Handler) observeRequest(rec *responseRecorder, start time.Time, state accessState) {
	if h.logger == nil {
		return
	}
	h.logger.LogAccess(AccessRecord{
		RequestID:    state.requestID,
		Protocol:     state.protocol,
		Model:        state.model,
		Stream:       state.stream,
		HTTPStatus:   rec.status,
		DurationMS:   time.Since(start).Milliseconds(),
		WrittenBytes: rec.bytes,
		ErrorCode:    rec.errCode,
		RemoteAddr:   state.remoteAddr,
		UserAgent:    state.userAgent,
	})
}

// maxUserAgentRunes 是写进访问日志的 User-Agent 字符数上限。
//
// 客户端可自报任意长的头部，不截断会让一条日志被无界文本撑大；截断只影响可读性，
// 不影响任何转发事实。
const maxUserAgentRunes = 512

// truncateUserAgent 把 User-Agent 截断到 maxUserAgentRunes 个字符（按 Unicode 字符计）。
func truncateUserAgent(userAgent string) string {
	if utf8.RuneCountInString(userAgent) <= maxUserAgentRunes {
		return userAgent
	}
	runes := []rune(userAgent)
	return string(runes[:maxUserAgentRunes])
}

// serveComplete 处理非流式请求：转发时把响应体写入客户端。
//
// 与 serveStream 同构：先设 Content-Type，再用 out.wrote 判断是否仍可把错误编码为响应。
// 非流式响应体由流水线生产（同协议透传上游原始字节、跨协议按客户端协议重建）。
func (h *Handler) serveComplete(w *responseRecorder, r *http.Request, adapter domain.Adapter, req *domain.Request) {
	w.Header().Set(contentTypeHeader, adapter.ContentType())
	out := newFlushWriter(w, h.writeTimeout)
	if err := h.forwarder.Forward(r.Context(), adapter, req, out); err != nil && !out.wrote {
		h.writeError(w, adapter, err)
	}
}

// serveStream 处理流式请求：逐帧写出并 Flush，流建立后不再改动状态码。
//
// 固定注入防缓冲响应头（Cache-Control 与 X-Accel-Buffering）：逐帧推送必须立即到达客户端，
// 中间层一旦缓冲响应体，客户端在整段生成完成前收不到任何字节，流式体感完全丧失。
//
// 只有在尚未向客户端写出任何字节时才允许把错误编码为非流式错误响应；一旦写出过字节，
// 状态码与响应头已送达客户端，此时协议自身的流式错误帧由流水线负责下发。
func (h *Handler) serveStream(w *responseRecorder, r *http.Request, adapter domain.Adapter, req *domain.Request) {
	w.Header().Set(contentTypeHeader, adapter.StreamContentType())
	w.Header().Set(cacheControlHeader, sseCacheControl)
	w.Header().Set(xAccelBufferingHeader, xAccelBufferingDisabled)
	out := newFlushWriter(w, h.writeTimeout)
	if err := h.forwarder.Forward(r.Context(), adapter, req, out); err != nil && !out.wrote {
		h.writeError(w, adapter, err)
	}
}

// writeError 把错误按适配器自身的形态编码为 HTTP 状态码与响应体，
// 并把错误码记入响应记录器，供访问日志使用。
func (h *Handler) writeError(w *responseRecorder, adapter domain.Adapter, err error) {
	if e := domain.AsError(err); e != nil {
		w.errCode = string(e.Code)
	}
	status, body := adapter.EncodeError(err)
	w.Header().Set(contentTypeHeader, adapter.ContentType())
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// methodNotAllowedError 构造 HTTP 方法不允许时的统一错误，method 是该路径接受的方法。
//
// internal/domain 没有对应错误码，而新增对外错误码属于统一协议的契约变更，
// 因此复用 invalid_request 的分级与错误体，只把 HTTPStatus 单独定为 405。
func methodNotAllowedError(method string) *domain.Error {
	err := domain.NewError(domain.CodeInvalidRequest, fmt.Sprintf("仅支持 %s 请求", method))
	err.HTTPStatus = http.StatusMethodNotAllowed
	return err
}

// defaultRequestID 生成一个 128 位随机标识符，用作请求的 request_id。
func defaultRequestID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("req-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf[:])
}

// responseRecorder 包裹 http.ResponseWriter，记录状态码、已写出字节数、是否已提交响应与错误码，
// 供请求结束时的访问日志使用。它不改变写出行为，
// 并把 Flush 与 Unwrap 透传给底层写出目标，使 flushWriter 的写超时与 Flush 能力不受影响。
type responseRecorder struct {
	http.ResponseWriter
	status    int
	bytes     int
	errCode   string
	committed bool
}

// newResponseRecorder 包装响应写出目标；未显式调用 WriteHeader 时默认 200。
func newResponseRecorder(w http.ResponseWriter) *responseRecorder {
	return &responseRecorder{ResponseWriter: w, status: http.StatusOK}
}

// WriteHeader 记录状态码与已提交标记后交给底层写出目标。
func (r *responseRecorder) WriteHeader(status int) {
	r.status = status
	r.committed = true
	r.ResponseWriter.WriteHeader(status)
}

// Write 累加已写出字节数后交给底层写出目标。
//
// gosec 的污点分析因鉴权错误派生自请求头而保守标记。
//
//nolint:gosec // G705：写入内容由调用方（适配器按错误码编码的固定文案）决定，不反射请求输入。
func (r *responseRecorder) Write(p []byte) (int, error) {
	r.committed = true
	n, err := r.ResponseWriter.Write(p)
	r.bytes += n
	return n, err
}

// Flush 在底层支持时透传 Flush，保持逐帧推送语义。
func (r *responseRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// SetRoutedModel 在响应头尚未提交前写入最终履约模型标头。
//
// 空白串忽略；已提交（已写出状态码或响应体）时忽略——此时响应头已送达客户端，
// 再改只会造成响应头与实际状态不一致。
func (r *responseRecorder) SetRoutedModel(model string) {
	if strings.TrimSpace(model) == "" || r.committed {
		return
	}
	r.Header().Set(routedModelHeader, model)
}

// 编译期断言：响应写出目标与 Flush 包装都能接收最终履约模型注入。
var (
	_ domain.RoutedModelSink = (*responseRecorder)(nil)
	_ domain.RoutedModelSink = (*flushWriter)(nil)
)

// Unwrap 返回底层写出目标，供 http.ResponseController 向上查找写超时能力。
func (r *responseRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// flushWriter 是包在 http.ResponseWriter 外的写出目标：每次写出前设置写超时，
// 写出成功后立即 Flush，并记录是否已向客户端写出过字节。
type flushWriter struct {
	writer     http.ResponseWriter
	flusher    http.Flusher
	controller *http.ResponseController
	timeout    time.Duration
	wrote      bool
}

// newFlushWriter 包装响应写出目标；不支持 Flush 时仍可写出，只是不保证逐帧送达。
func newFlushWriter(w http.ResponseWriter, timeout time.Duration) *flushWriter {
	fw := &flushWriter{
		writer:     w,
		controller: http.NewResponseController(w),
		timeout:    timeout,
	}
	if flusher, ok := w.(http.Flusher); ok {
		fw.flusher = flusher
	}
	return fw
}

// Write 设置写超时后写出并 Flush；写超时到期时返回底层错误，由调用方停止继续写出。
func (f *flushWriter) Write(p []byte) (int, error) {
	f.wrote = true
	if f.timeout > 0 {
		// 不支持设置写超时的响应写出目标（例如测试替身）忽略该错误，退回为不设超时。
		_ = f.controller.SetWriteDeadline(time.Now().Add(f.timeout))
	}
	n, err := f.writer.Write(p)
	if err != nil {
		return n, err
	}
	if f.flusher != nil {
		f.flusher.Flush()
	}
	return n, nil
}

// SetRoutedModel 把最终履约模型注入透传给被包装的响应写出目标；
// 目标不支持该能力时为空操作。
func (f *flushWriter) SetRoutedModel(model string) {
	if sink, ok := f.writer.(domain.RoutedModelSink); ok {
		sink.SetRoutedModel(model)
	}
}
