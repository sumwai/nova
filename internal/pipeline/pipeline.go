// Package pipeline 实现网关唯一的核心转发流水线：
//   - 请求校验
//   - 选路与请求定稿
//   - 上游调用与候选回退
//
// 各线协议共用本流水线，协议差异只由注入的适配器表达，流水线内不出现协议分支。
//
// 本包只依赖 internal/domain：上游客户端由装配层以 domain.UpstreamCaller 注入，
// 因此本包不得、也不需要导入 internal/upstream。
package pipeline

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/sumwai/nova/internal/domain"
)

// maxAttemptsDefault 是单请求最多发起的上游尝试次数默认上限。
const maxAttemptsDefault = 3

// AdapterLookup 按协议返回适配器，用于按上游协议取请求定稿能力。
//
// 与 internal/upstream 的适配器查找同形，便于装配层用同一个查找函数同时供给两处；
// 本流水线只把返回值当作无状态的请求构建器使用，不参与流式状态。
type AdapterLookup func(protocol domain.Protocol) (domain.Adapter, error)

// Options 是构造流水线的依赖。
type Options struct {
	// Adapters 按协议返回适配器。必填。
	Adapters AdapterLookup
	// Upstream 调用上游渠道。必填。
	Upstream domain.UpstreamCaller
	// Routes 返回候选渠道。必填。
	Routes domain.RouteResolver
	// Observer 记录每次上游尝试；可为 nil，记录失败不影响转发结果。
	Observer domain.Observer
	// Breaker 是渠道熔断器；可为 nil（不熔断，候选渠道按解析顺序逐个尝试）。
	// 非 nil 时，遍历候选时跳过处于熔断打开态的渠道，并在每次上游尝试结束后上报结果。
	Breaker Breaker
	// MaxAttempts 是单请求最多发起的上游尝试次数；<= 0 时取 maxAttemptsDefault。
	MaxAttempts int
	// Backoff 是换下一候选前的退避策略；零值字段取对应默认值。
	Backoff BackoffOptions
}

// Pipeline 是唯一的核心转发流水线。
type Pipeline struct {
	adapters    AdapterLookup
	upstream    domain.UpstreamCaller
	routes      domain.RouteResolver
	observer    domain.Observer
	breaker     Breaker
	maxAttempts int
	backoff     backoff
}

// attemptResult 是一次上游尝试的结果。Err 为 nil 表示成功。
type attemptResult struct {
	// Completion 是非流式尝试成功时的结果：原始响应字节与归一化响应。
	Completion *domain.UpstreamResult
	// Usage 是本次尝试取得的用量；未取得时为来源未知的零值。只用于尝试记录。
	Usage domain.Usage
	// ResponseParts 是响应侧改写标注，由下沉目标或非流式写出分支填写，
	// 与请求侧标注合并后写入尝试记录。
	ResponseParts domain.RewriteParts
	// WroteBytes 报告本次尝试是否已向客户端写出过字节：一旦写出就不再换渠道重试。
	WroteBytes bool
	// clientWriteFailed 报告本次尝试的终止错误是否本端向客户端写出失败：
	// 为真时不能再尝试向同一个写出目标补发任何字节。
	clientWriteFailed bool
	// StartedAt 与 EndedAt 是本次上游调用的两个边界时刻，由 doAttempt 紧贴上游调用前后采集。
	StartedAt time.Time
	EndedAt   time.Time
	// Err 是本次尝试的错误；nil 表示成功。
	Err error
}

// New 构造流水线。依赖缺失在构造时报出，不推迟到请求时。
func New(opts Options) (*Pipeline, error) {
	switch {
	case opts.Adapters == nil:
		return nil, domain.NewError(domain.CodeInternal, "缺少适配器查找函数")
	case opts.Upstream == nil:
		return nil, domain.NewError(domain.CodeInternal, "缺少上游调用器")
	case opts.Routes == nil:
		return nil, domain.NewError(domain.CodeInternal, "缺少候选渠道解析器")
	}
	maxAttempts := opts.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = maxAttemptsDefault
	}
	return &Pipeline{
		adapters:    opts.Adapters,
		upstream:    opts.Upstream,
		routes:      opts.Routes,
		observer:    opts.Observer,
		breaker:     opts.Breaker,
		maxAttempts: maxAttempts,
		backoff:     newBackoff(opts.Backoff),
	}, nil
}

// Forward 处理一次转发：req.Stream 为 false 时把面向客户端的响应体写入 out，
// 为 true 时把面向客户端的流式字节写入 out。
func (p *Pipeline) Forward(ctx context.Context, client domain.Adapter, req *domain.Request, out io.Writer) error {
	if req == nil {
		return domain.NewError(domain.CodeInvalidRequest, "请求为空")
	}
	if client == nil {
		return domain.NewError(domain.CodeInternal, "缺少客户端适配器")
	}
	if out == nil {
		return domain.NewError(domain.CodeInternal, "转发缺少写出目标")
	}
	if req.Stream {
		return p.forward(ctx, client, req, out, func(attemptCtx context.Context, route domain.Route, body []byte, _ domain.RewriteParts) attemptResult {
			return p.streamAttempt(attemptCtx, client, req, route, body, out)
		})
	}
	return p.forward(ctx, client, req, out, func(attemptCtx context.Context, route domain.Route, body []byte, _ domain.RewriteParts) attemptResult {
		return p.completeAttempt(attemptCtx, client, req, route, body, out)
	})
}

// completeAttempt 执行一次非流式尝试，并在成功时把面向客户端的响应体写入 out。
//
// 字节来源由本次尝试的 route.Protocol 与 req.Protocol 是否一致决定：一致时逐字节写出
// 上游原始响应体，不一致时写出按客户端协议重建的结果。写出权归流水线，因为只有它同时
// 知道两侧协议，也只有它能保证重试时不对客户端重复写出。
func (p *Pipeline) completeAttempt(
	ctx context.Context,
	client domain.Adapter,
	req *domain.Request,
	route domain.Route,
	body []byte,
	out io.Writer,
) attemptResult {
	result := attemptResult{StartedAt: time.Now()}
	completion, callErr := p.upstream.Complete(ctx, route, req, body)
	result.EndedAt = time.Now()
	if callErr != nil {
		result.Err = callErr
		return result
	}
	result.Completion = completion
	result.Usage = completion.Response.Usage
	parts, writeErr := p.writeCompletion(client, req, route, completion, out)
	result.ResponseParts = parts
	if writeErr != nil {
		result.Err = writeErr
		result.WroteBytes = true
		result.clientWriteFailed = true
	}
	return result
}

// writeCompletion 把一次成功的非流式尝试的响应体写入 out，并返回本次写出的响应侧改写标注。
func (p *Pipeline) writeCompletion(
	client domain.Adapter,
	req *domain.Request,
	route domain.Route,
	completion *domain.UpstreamResult,
	out io.Writer,
) (domain.RewriteParts, error) {
	if completion == nil || completion.Response == nil {
		return nil, domain.NewError(domain.CodeInternal, "上游返回了空响应")
	}
	if route.Protocol == req.Protocol {
		if _, err := out.Write(completion.Raw); err != nil {
			return nil, clientWriteError(err)
		}
		return nil, nil
	}
	body, err := client.EncodeResponse(completion.Response)
	if err != nil {
		return nil, err
	}
	if _, err := out.Write(body); err != nil {
		return domain.RewriteParts{domain.RewritePartResponseReencoded}, clientWriteError(err)
	}
	return domain.RewriteParts{domain.RewritePartResponseReencoded}, nil
}

// streamAttempt 执行一次流式尝试：按协议是否一致选择透传或重建下沉目标，再把上游分片交给它。
func (p *Pipeline) streamAttempt(
	ctx context.Context,
	client domain.Adapter,
	req *domain.Request,
	route domain.Route,
	body []byte,
	out io.Writer,
) attemptResult {
	streamClient := client.NewStream()
	if streamClient == nil {
		return attemptResult{Err: domain.NewError(domain.CodeInternal, "派生流式适配器失败")}
	}
	sink, err := newSink(req, route, out, streamClient)
	if err != nil {
		return attemptResult{Err: err}
	}
	result := attemptResult{StartedAt: time.Now()}
	result.Err = p.upstream.Stream(ctx, route, req, body, sink)
	result.EndedAt = time.Now()
	result.Usage = sink.collectedUsage()
	result.WroteBytes = sink.wroteBytes()
	result.clientWriteFailed = sink.writeFailed()
	result.ResponseParts = sink.rewriteParts()
	return result
}

// newSink 按客户端协议与上游协议是否一致选择下沉目标：一致时按原始帧透传，不一致时按客户端协议重建。
func newSink(req *domain.Request, route domain.Route, out io.Writer, streamClient domain.Adapter) (attemptSink, error) {
	if route.Protocol == req.Protocol {
		return &passthroughSink{out: out}, nil
	}
	model := domain.UpstreamModelName(req.Model, domain.RewriteOptions{UpstreamModel: route.UpstreamModel})
	return newRebuildSink(out, streamClient, model)
}

// writeStreamError 向客户端下发协议自身的流式错误帧；协议没有该能力时不下发，
// 由入口层按「结束后关闭连接」降级处理。
func writeStreamError(client domain.Adapter, out io.Writer, err error) {
	encoder, ok := client.(domain.StreamErrorEncoder)
	if !ok {
		return
	}
	frame, supported := encoder.EncodeStreamError(err)
	if !supported || len(frame) == 0 {
		return
	}
	_, _ = out.Write(frame)
}

// forward 是流式与非流式两条路径的公共骨架：
//
//  1. 请求校验；
//  2. 解析候选渠道；
//  3. 按候选顺序执行尝试，可重试失败换下一个候选，不可重试或已向客户端写出字节立即进入终态。
//
// 每次尝试的具体动作由 doAttempt 执行。out 实现 domain.RoutedModelSink 时被注入本次尝试
// 实际使用的事由模型名，使客户端能读到网关最终履约的模型。
func (p *Pipeline) forward(
	ctx context.Context,
	client domain.Adapter,
	req *domain.Request,
	out io.Writer,
	doAttempt func(ctx context.Context, route domain.Route, body []byte, requestParts domain.RewriteParts) attemptResult,
) error {
	if err := req.Validate(); err != nil {
		return err
	}

	candidateRoutes, err := p.routes.Candidates(ctx, req)
	if err != nil {
		return domain.NewError(domain.CodeInternal, "选路失败").WithCause(err)
	}
	if len(candidateRoutes) == 0 {
		return domain.NewError(domain.CodeModelNotFound, fmt.Sprintf("模型 %q 没有可用渠道", req.Model))
	}

	attemptLimit := p.maxAttempts
	var lastErr error
	attemptsMade := 0
	for i := 0; i < len(candidateRoutes) && attemptsMade < attemptLimit; i++ {
		route := candidateRoutes[i]
		// 熔断跳过：打开态渠道不参与调度，不计入尝试次数。
		if !p.allowRoute(route) {
			continue
		}
		body, requestParts, finalizeErr := p.finalizeRequest(req, route)
		if finalizeErr != nil {
			return finalizeErr
		}
		attemptsMade++
		p.markRoutedModel(out, route)
		attempt := doAttempt(ctx, route, body, requestParts)
		p.recordRouteOutcome(route, attempt.Err)
		attempt.ResponseParts = mergeParts(requestParts, attempt.ResponseParts)
		p.recordAttempt(ctx, req, route, attemptsMade, attempt)
		if attempt.Err == nil {
			return nil
		}
		lastErr = attempt.Err
		if !domain.Retryable(attempt.Err) || ctx.Err() != nil || attempt.WroteBytes {
			if attempt.WroteBytes && !attempt.clientWriteFailed {
				// 状态码与响应头已送达客户端，只能下发协议自身的流式错误帧。
				writeStreamError(client, out, attempt.Err)
			}
			return attempt.Err
		}
		if i+1 < len(candidateRoutes) && attemptsMade < attemptLimit {
			if delay, ok := p.backoff.delay(attemptsMade, attempt.Err); ok {
				if waitErr := p.backoff.wait(ctx, delay); waitErr != nil {
					break
				}
			}
		}
	}
	if lastErr == nil {
		if attemptsMade == 0 {
			return domain.NewError(domain.CodeUpstreamUnavailable, "所有候选渠道均处于熔断状态")
		}
		return domain.NewError(domain.CodeUpstreamUnavailable, "候选渠道或尝试次数耗尽")
	}
	return lastErr
}

// markRoutedModel 把本次尝试实际使用的事由模型名注入客户端写出目标；目标不支持该能力时为空操作。
func (p *Pipeline) markRoutedModel(out io.Writer, route domain.Route) {
	sink, ok := out.(domain.RoutedModelSink)
	if !ok {
		return
	}
	sink.SetRoutedModel(domain.UpstreamModelName("", domain.RewriteOptions{UpstreamModel: route.UpstreamModel}))
}

// allowRoute 询问渠道熔断器是否放行该候选；未装配熔断器时恒放行。
func (p *Pipeline) allowRoute(route domain.Route) bool {
	if p.breaker == nil {
		return true
	}
	return p.breaker.Allow(route.BreakerKey())
}

// recordRouteOutcome 把一次上游尝试的结果上报给渠道熔断器；未装配熔断器时为空操作。
func (p *Pipeline) recordRouteOutcome(route domain.Route, err error) {
	if p.breaker == nil {
		return
	}
	p.breaker.Record(route.BreakerKey(), err)
}

// recordAttempt 把一次上游尝试写入观测记录；未配置观测器时不做任何事。
func (p *Pipeline) recordAttempt(
	ctx context.Context,
	req *domain.Request,
	route domain.Route,
	attempt int,
	result attemptResult,
) {
	if p.observer == nil {
		return
	}
	outcome := domain.AttemptOK
	if result.Err != nil {
		outcome = domain.AttemptFailed
		if ctx.Err() != nil {
			outcome = domain.AttemptCancelled
		}
	}
	rec := domain.AttemptRecord{
		RequestID:      req.RequestID,
		Attempt:        attempt,
		ClientProtocol: req.Protocol,
		// 上游协议取自本次尝试实际使用的渠道：它与客户端协议不同即表示走了跨协议重建，
		// 这是「响应为什么和客户端请求的形态不一样」的答案。
		UpstreamProtocol: route.Protocol,
		RequestedModel:   req.Model,
		UpstreamID:       route.UpstreamID,
		UpstreamModel:    route.UpstreamModel,
		AccountRef:       route.AccountRef,
		Outcome:          outcome,
		Usage:            result.Usage,
		ErrorCode:        errorCode(result.Err),
		ErrorDetail:      errorDetail(result.Err),
		StartedAt:        result.StartedAt,
		EndedAt:          result.EndedAt,
		RewrittenParts:   result.ResponseParts,
	}
	_ = p.observer.RecordAttempt(ctx, rec)
}

// mergeParts 合并请求侧与响应侧改写标注，去重并保持出现顺序。
func mergeParts(base, extra domain.RewriteParts) domain.RewriteParts {
	if len(extra) == 0 {
		return base
	}
	merged := append(domain.RewriteParts(nil), base...)
	for _, part := range extra {
		if !merged.Has(part) {
			merged = append(merged, part)
		}
	}
	return merged
}

// errorCode 返回错误码字符串；非统一错误返回空串。
func errorCode(err error) string {
	if domainErr := domain.AsError(err); domainErr != nil {
		return string(domainErr.Code)
	}
	return ""
}

// attemptErrorDetailMaxRunes 是写进尝试日志的排障细节字数上限。
const attemptErrorDetailMaxRunes = 256

// errorDetail 返回写进尝试日志的排障细节；非统一错误返回空串。
func errorDetail(err error) string {
	domainErr := domain.AsError(err)
	if domainErr == nil {
		return ""
	}
	return truncateRunes(domainErr.Detail, attemptErrorDetailMaxRunes)
}

// finalizeRequest 把内部请求定稿为上游请求体：同协议走报文改写、跨协议走重建。
func (p *Pipeline) finalizeRequest(req *domain.Request, route domain.Route) ([]byte, domain.RewriteParts, error) {
	builder, err := p.builderFor(route.Protocol)
	if err != nil {
		return nil, nil, err
	}
	options := domain.RewriteOptions{
		MaxOutputTokens: route.OutputLimit,
		UpstreamModel:   route.UpstreamModel,
	}
	if route.Protocol == req.Protocol {
		return builder.RewriteRawBody(req.RawBody, options)
	}
	return builder.EncodeRequest(req, options)
}

// builderFor 按上游协议取请求构建能力。
func (p *Pipeline) builderFor(protocol domain.Protocol) (domain.UpstreamRequestBuilder, error) {
	adapter, err := p.adapters(protocol)
	if err != nil {
		return nil, domain.NewError(domain.CodeInternal,
			fmt.Sprintf("取协议 %q 的适配器失败", string(protocol))).WithCause(err)
	}
	if adapter == nil {
		return nil, domain.NewError(domain.CodeInternal, fmt.Sprintf("协议 %q 没有适配器", string(protocol)))
	}
	builder, ok := adapter.(domain.UpstreamRequestBuilder)
	if !ok {
		return nil, domain.NewError(domain.CodeInternal,
			fmt.Sprintf("协议 %q 的适配器不支持请求构建", string(protocol)))
	}
	return builder, nil
}

// truncateRunes 把字符串按 Unicode 字符数截断到 limit。
func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}
