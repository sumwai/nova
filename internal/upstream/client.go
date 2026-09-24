// Package upstream 实现调用上游渠道的 HTTP 客户端，实现 domain.UpstreamCaller。
//
// 职责边界：
//   - 只按流水线传入的渠道（domain.Route）与定稿后的请求体字节发送请求，不做选路、
//     不读数据库、不读环境变量，因此可以在没有仓储层的环境下单独测试；
//   - 凭据与渠道级请求头经 HeaderProvider 注入，真实读取与解密由实现负责；
//   - 上游协议差异不在本包表达：响应解码交给 route.Protocol 对应的适配器，
//     SSE（Server-Sent Events，服务器发送事件）分帧交给 internal/transport/sse；
//   - 上游错误统一转换为 domain.Error 并标注可重试性。
//
// 跨渠道重试不属于本包：本包只把一次尝试的结果（成功、错误或截断）报告给流水线，
// 由流水线决定是否换渠道并决定是否向客户端下发协议自身的错误帧。
package upstream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sumwai/nova/internal/domain"
)

const (
	// defaultChannelTimeout 是渠道未配置超时时使用的兜底超时。上游调用必须有超时，
	// 否则挂死的上游会一直占住网关连接。
	defaultChannelTimeout = 60 * time.Second
	// errorDetailLimit 是写进排障文案（domain.Error.Detail）的上游响应体最大字节数，
	// 避免超长报文进入日志。
	errorDetailLimit = 256
	// maxResponseBytes 是非流式上游响应体的最大字节数（32 MiB）。超过即判定为超出安全上限，
	// 不再把整段响应体读进内存，避免异常上游用超长响应体撑爆网关。
	maxResponseBytes = 32 << 20
	// jsonContentType 是上游请求体的 Content-Type。
	jsonContentType = "application/json"
)

// HeaderProvider 提供调用上游时要携带的渠道级请求头。
//
// 覆盖两类来源：渠道配置里的静态请求头，以及按 route.CredentialRef 解析出的凭据头
// （例如 OpenAI 的 Authorization、Anthropic 的 x-api-key）。真实读取与解密由注入的实现负责，
// 本包不读数据库与环境变量。返回的请求头随后交适配器的 UpstreamHeaders 合并协议内置必需头，
// 同名时渠道级取值覆盖内置值。
type HeaderProvider interface {
	UpstreamHeaders(ctx context.Context, route domain.Route) (http.Header, error)
}

// AdapterLookup 按上游协议返回响应解码所用的适配器。
//
// 上游协议差异只在适配器内表达，本包不解析任何协议字面量；未知协议必须返回错误。
type AdapterLookup func(protocol domain.Protocol) (domain.Adapter, error)

// Options 是构造 Client 的依赖与默认值。
type Options struct {
	// HTTPClient 是底层 HTTP 客户端；nil 时使用标准库默认客户端。超时由本包按渠道配置施加。
	HTTPClient *http.Client
	// Headers 提供渠道级请求头（含凭据）。必填。
	Headers HeaderProvider
	// Adapters 按上游协议返回适配器。必填。
	Adapters AdapterLookup
	// DefaultTimeout 是渠道未配置超时（Route.Timeout <= 0）时使用的超时；
	// 非正时取 defaultChannelTimeout。
	DefaultTimeout time.Duration
}

// Client 是 domain.UpstreamCaller 的 HTTP 实现。
type Client struct {
	httpClient     *http.Client
	headers        HeaderProvider
	adapters       AdapterLookup
	defaultTimeout time.Duration
}

// New 构造上游客户端。Headers 与 Adapters 必填，配置错误在构造时报出，不推迟到请求时。
func New(opts Options) (*Client, error) {
	if opts.Headers == nil {
		return nil, domain.NewError(domain.CodeInternal, "缺少上游请求头提供者")
	}
	if opts.Adapters == nil {
		return nil, domain.NewError(domain.CodeInternal, "缺少上游适配器查找函数")
	}
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	timeout := opts.DefaultTimeout
	if timeout <= 0 {
		timeout = defaultChannelTimeout
	}
	return &Client{
		httpClient:     httpClient,
		headers:        opts.Headers,
		adapters:       opts.Adapters,
		defaultTimeout: timeout,
	}, nil
}

// Complete 发送定稿后的上游请求体并返回归一化结果。
//
// 超时口径：优先用 route.Timeout，未配置时用 DefaultTimeout，覆盖整段调用
// （连接、请求与响应体读取）。
//
// 上游返回非 2xx 时按状态码分级：
//   - 429 记为可重试的上游限流
//   - 408 记为可重试的上游超时
//   - 其余 4xx 记为不可重试的上游拒绝
//   - 5xx 与其它非 2xx 记为可重试的上游不可用
//
// 响应头中携带合法 Retry-After 时，返回的错误额外实现 RetryAfter 能力（见 retryAfterError），
// 把上游建议的退避时长交给调用方；是否据此等待由调用方决定。
//
// 2xx 响应体原样放进返回值的 Raw，同一份字节再交 route.Protocol 对应适配器的
// DecodeResponse 归一化为 Response。响应体读取上限为 maxResponseBytes，超过该上限即判定为
// 超出安全上限并归为可重试的上游不可用，不把整段响应体读进内存。
// 解码失败说明上游返回了本网关无法识别的响应，归为可重试的上游不可用，而不是客户端的参数错误。
func (c *Client) Complete(ctx context.Context, route domain.Route, _ *domain.Request, body []byte) (*domain.UpstreamResult, error) {
	adapter, err := c.adapterFor(route.Protocol)
	if err != nil {
		return nil, err
	}

	callCtx, cancel := context.WithTimeout(ctx, c.timeoutFor(route))
	defer cancel()

	httpReq, err := c.newRequest(callCtx, route, adapter, body, false)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, mapTransportError(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, tooLarge, err := readResponseBody(resp.Body, maxResponseBytes)
	if err != nil {
		return nil, mapTransportError(ctx, err)
	}
	if tooLarge {
		return nil, responseTooLargeError()
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, classifyHTTPStatus(resp.StatusCode, resp.Header, respBody)
	}
	decoded, err := adapter.DecodeResponse(respBody)
	if err != nil {
		// 上游返回了 2xx 但不是本协议认得的报文。这类故障的成因几乎都在上游侧
		// （地址写错被重定向到别处、上游用 200 回了错误信封），只报「无法解析」无从定位，
		// 因此把适配器的判定与响应体片段一并写进排障详情。
		// Detail 只进服务端日志，不进面向客户端的错误体。
		return nil, domain.NewError(domain.CodeUpstreamUnavailable, "上游响应无法解析").
			WithDetail(fmt.Sprintf("%s；上游响应前 %d 字节：%s", err.Error(), errorDetailLimit, errorSnippet(respBody))).
			WithCause(err)
	}
	return &domain.UpstreamResult{Raw: respBody, Response: decoded}, nil
}

// errResponseTooLarge 表示上游非流式响应体超过 maxResponseBytes。
var errResponseTooLarge = errors.New("上游响应体超过上限")

// readResponseBody 读取上游响应体，最多保留 limit 字节，并报告响应体是否超过该上限。
//
// 读取上限取 limit+1：只用 io.LimitReader(body, limit) 时「恰好读满」与「还有更多字节」
// 都以 EOF 结束，无法区分；多读一个字节才能判定超限。
func readResponseBody(body io.Reader, limit int64) (data []byte, tooLarge bool, err error) {
	data, err = io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) > limit {
		return nil, true, nil
	}
	return data, false, nil
}

// responseTooLargeError 构造响应体超限的上游错误。
//
// 归为 upstream_unavailable（可重试）：换渠道可能拿到体量正常的上游响应。
// 该判定早于状态码分级：超限时无法安全读完整响应体，状态码文案的取材也不完整。
func responseTooLargeError() *domain.Error {
	return domain.NewError(domain.CodeUpstreamUnavailable, "上游响应体超过上限").
		WithDetail(fmt.Sprintf("非流式响应体上限 %d 字节", maxResponseBytes)).
		WithCause(errResponseTooLarge)
}

// newRequest 按渠道、适配器与定稿后的请求体构造上游请求。
//
// 请求头合并顺序：
//
//  1. 先渠道级请求头（含凭据）
//  2. 再交适配器的 UpstreamHeaders 补协议内置必需头（同名时渠道级取值覆盖内置值）
//
// Content-Type 与 Accept 由本包按响应形态设定，保证与 body 的实际形态一致。
func (c *Client) newRequest(ctx context.Context, route domain.Route, adapter domain.Adapter, body []byte, stream bool) (*http.Request, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, route.BaseURL, bytes.NewReader(body))
	if err != nil {
		return nil, domain.NewError(domain.CodeInternal, "构造上游请求失败").WithCause(err)
	}
	channelHeaders, err := c.headers.UpstreamHeaders(ctx, route)
	if err != nil {
		return nil, domain.NewError(domain.CodeInternal, "解析上游请求头失败").WithCause(err)
	}
	headers := channelHeaders
	if provider, ok := adapter.(domain.UpstreamHeaderProvider); ok {
		headers = provider.UpstreamHeaders(channelHeaders)
	}
	for name, values := range headers {
		for _, value := range values {
			httpReq.Header.Add(name, value)
		}
	}
	httpReq.Header.Set("Content-Type", jsonContentType)
	accept := adapter.ContentType()
	if stream {
		accept = adapter.StreamContentType()
	}
	httpReq.Header.Set("Accept", accept)
	return httpReq, nil
}

// adapterFor 按上游协议取适配器；未注册协议或返回空适配器一律按平台内部错误处理。
func (c *Client) adapterFor(protocol domain.Protocol) (domain.Adapter, error) {
	adapter, err := c.adapters(protocol)
	if err != nil {
		return nil, domain.NewError(domain.CodeInternal, fmt.Sprintf("没有协议 %q 的适配器", string(protocol))).WithCause(err)
	}
	if adapter == nil {
		return nil, domain.NewError(domain.CodeInternal, fmt.Sprintf("协议 %q 的适配器为空", string(protocol)))
	}
	return adapter, nil
}

// timeoutFor 返回本次调用的超时：渠道配置优先，未配置时用默认值。
func (c *Client) timeoutFor(route domain.Route) time.Duration {
	if route.Timeout > 0 {
		return route.Timeout
	}
	return c.defaultTimeout
}

// mapTransportError 把 HTTP 传输层错误转换为统一错误。
//
// deadline 到期记为可重试的上游超时。调用方主动取消（例如客户端断开）记为不可重试的平台错误，
// 避免换渠道重试。其余连接类错误记为可重试的上游不可用。
func mapTransportError(ctx context.Context, err error) *domain.Error {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return domain.NewError(domain.CodeUpstreamTimeout, "上游调用超时").WithCause(err)
	case errors.Is(err, context.Canceled) && ctx.Err() != nil:
		return domain.NewError(domain.CodeInternal, "上游调用已取消").WithCause(err)
	default:
		return domain.NewError(domain.CodeUpstreamUnavailable, "上游连接失败").WithCause(err)
	}
}

// classifyHTTPStatus 把上游非 2xx 状态码分级为统一错误。
//
// header 用于提取 Retry-After 提示：头缺失或取值非法时返回的错误不含该提示，
// 调用方按自身退避策略处理。
func classifyHTTPStatus(status int, header http.Header, body []byte) error {
	detail := fmt.Sprintf("上游 HTTP 状态码 %d", status)
	if snippet := errorSnippet(body); snippet != "" {
		detail += "：" + snippet
	}
	var err *domain.Error
	switch {
	case status == http.StatusTooManyRequests:
		err = domain.NewError(domain.CodeUpstreamRateLimited, "上游限流").WithDetail(detail)
	case status == http.StatusRequestTimeout:
		err = domain.NewError(domain.CodeUpstreamTimeout, "上游超时").WithDetail(detail)
	case status >= http.StatusBadRequest && status < http.StatusInternalServerError:
		err = domain.NewError(domain.CodeUpstreamRejected, "上游拒绝请求").WithDetail(detail)
	default:
		err = domain.NewError(domain.CodeUpstreamUnavailable, "上游不可用").WithDetail(detail)
	}
	return withRetryAfter(err, header, time.Now())
}

// retryAfterError 在统一错误之上附加上游通过 Retry-After 响应头给出的建议退避时长。
//
// 之所以包装而不在 domain.Error 上新增字段：该提示只对带外等待有意义，
// 包装后既有的错误码、分级与可重试性判定不受影响——Unwrap 暴露的仍是同一个 *domain.Error，
// domain.AsError 与 domain.Retryable 照常工作。
//
// 调用方按结构匹配读取该能力（声明形如 RetryAfter() (time.Duration, bool) 的接口），
// 生产者与消费者之间不因此新增对具体类型的依赖。
type retryAfterError struct {
	err        *domain.Error
	retryAfter time.Duration
}

// Error 实现 error 接口，直接转发被包装统一错误的面向排障文案。
func (e *retryAfterError) Error() string { return e.err.Error() }

// RetryAfter 返回上游建议的退避时长，并报告该错误确实携带该提示。
func (e *retryAfterError) RetryAfter() (time.Duration, bool) {
	return e.retryAfter, true
}

// Unwrap 返回被包装的统一错误，使 domain.AsError 与 domain.Retryable 仍能取到错误分级。
func (e *retryAfterError) Unwrap() error { return e.err }

// withRetryAfter 在上游给出合法 Retry-After 时把退避提示附加到统一错误上；否则原样返回。
func withRetryAfter(err *domain.Error, header http.Header, now time.Time) error {
	delay, ok := parseRetryAfter(header.Get("Retry-After"), now)
	if !ok {
		return err
	}
	return &retryAfterError{err: err, retryAfter: delay}
}

// parseRetryAfter 解析上游 Retry-After 响应头的两种合法取值，返回相对 now 的退避时长。
//
//   - delay-seconds：非负整数秒，例如 Retry-After: 5；
//   - HTTP-date：RFC 9110 的三种日期格式，例如 Retry-After: Wed, 21 Oct 2015 07:28:00 GMT。
//
// 第二个返回值报告解析是否成功。取值缺失、格式非法或秒数为负时返回 false：
// 上游没有给出可用的等待建议，调用方不得据此等待。HTTP-date 已经过去时返回 0 与 true，
// 表示「不必等待」而不是「没有建议」。
func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, false
	}
	if seconds, err := strconv.Atoi(trimmed); err == nil {
		if seconds < 0 {
			return 0, false
		}
		return time.Duration(seconds) * time.Second, true
	}
	when, err := http.ParseTime(trimmed)
	if err != nil {
		return 0, false
	}
	delay := when.Sub(now)
	if delay < 0 {
		delay = 0
	}
	return delay, true
}

// errorSnippet 返回上游响应体在排障文案中的截断片段。
func errorSnippet(body []byte) string {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > errorDetailLimit {
		trimmed = trimmed[:errorDetailLimit]
	}
	return string(trimmed)
}
