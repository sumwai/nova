package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/sumwai/nova/internal/domain"
	"github.com/sumwai/nova/internal/transport/sse"
)

// 编译期断言：Client 必须完整实现 domain.UpstreamCaller（含本文件的 Stream）。
var _ domain.UpstreamCaller = (*Client)(nil)

// Stream 读取上游 SSE 流并逐帧下沉，返回本次尝试的结果。
//
// 实现要点：
//
//  1. 用 internal/transport/sse 的逐帧读取器读取，读到一帧处理一帧，不把整条流缓冲进内存。
//  2. 每帧经 route.Protocol 对应适配器的 DecodeStreamFrame 解码。流式解码有跨帧状态，
//     故先用 NewStream 为本次流派生实例，再逐帧调用同一实例。
//  3. sink 实现 domain.FrameSink 时，对每一帧只调用 SendFrame 交付该帧原始字节与该帧解出的全部分片；
//     未实现时只逐分片调用 Send。EOF 收尾分片没有对应的上游帧与原始字节，两种模式下一律经 Send 交付。
//  4. 读取循环退出后调用一次 FinishStream：其返回的 ChunkStreamEnd 计入 sawEnd，
//     使「本轮是否产出过结束分片」同时覆盖帧内分片与 EOF 收尾分片，且仅在正常结束分支交付收尾分片。
//     该判定必须早于读错误分类，避免上游给出结束信号后连接挂住的空闲超时被误判为截断。
//  5. 读到 EOF 且本轮从未产出结束帧（domain.ChunkStreamEnd）时，说明上游在协议结束信号前断开，
//     返回可重试的上游错误。由流水线负责换渠道重试或下发错误帧，跨渠道重试循环不属于本包。
//  6. 单帧超过 sse.Reader 的上限时返回可重试的上游错误，不丢弃该情况。
//  7. 超时按空闲（无字节）口径：在指定超时（route.Timeout 或 DefaultTimeout）内底层没有任何字节到达，
//     即取消请求并返回可重试的上游超时，避免长流被整段超时误杀。
//     注释行与纯空块心跳不产生帧，但字节到达即视为有进展并重置看门狗，
//     使只发心跳的长思考流不会被误切断。
func (c *Client) Stream(ctx context.Context, route domain.Route, _ *domain.Request, body []byte, sink domain.ChunkSink) error {
	if sink == nil {
		return domain.NewError(domain.CodeInternal, "流式下沉目标为空")
	}
	adapter, err := c.adapterFor(route.Protocol)
	if err != nil {
		return err
	}
	streamAdapter := adapter.NewStream()
	if streamAdapter == nil {
		return domain.NewError(domain.CodeInternal, "派生流式适配器失败")
	}

	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	httpReq, err := c.newRequest(streamCtx, route, adapter, body, true)
	if err != nil {
		return err
	}

	// 空闲超时看门狗：上游在没有任何字节到达的时间内挂死时取消请求。
	// 定时器在任何底层字节到达时重置，而不是只在读到完整帧时重置：
	// 注释行与纯空块心跳不产生帧，但同属上游仍在推进的证据。
	var stalled atomic.Bool
	idleTimeout := c.timeoutFor(route)
	timer := time.AfterFunc(idleTimeout, func() {
		stalled.Store(true)
		cancel()
	})
	defer timer.Stop()

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		if stalled.Load() {
			return streamTimeoutError(err)
		}
		return mapTransportError(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		respBody, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return mapTransportError(ctx, readErr)
		}
		return classifyHTTPStatus(resp.StatusCode, resp.Header, respBody)
	}

	frameSink, passthrough := sink.(domain.FrameSink)
	// 看门狗按字节到达刷新：注释心跳被读取器内部跳过、不返回帧，
	// 若只在读到完整帧时重置，只发心跳的长思考流会被空闲超时误杀。
	reader := sse.NewReaderWithActivity(resp.Body, func() { timer.Reset(idleTimeout) })
	sawEnd := false
	for {
		frame, readErr := reader.Next()
		if readErr != nil {
			// 读取循环退出：给适配器一次 EOF 收尾机会。收尾分片参与「本轮是否产出过结束分片」判定，
			// 故必须在本轮读错误分类之前算出；它只在按正常结束处置的分支交付。
			finishChunks := streamAdapter.FinishStream()
			finishEnded := false
			for _, chunk := range finishChunks {
				if chunk.Kind == domain.ChunkStreamEnd {
					finishEnded = true
				}
			}
			if classifyErr := classifyStreamReadError(ctx, readErr, stalled.Load(), sawEnd || finishEnded); classifyErr != nil {
				return classifyErr
			}
			// 本轮已经产出过结束分片（sawEnd 在帧循环内按帧内分片置位）时，
			// EOF 收尾不得再交付第二个结束分片。上游先发 [DONE] 并在其后追加携带 finish_reason 的顽固帧时，
			// 解码器会在该帧起始处按「上一轮已结束」复位并重新记下结束信号，
			// 导致 FinishStream 仍会返回收尾分片。交付该分片会使跨协议重建路径向客户端写出第二个终止帧，违反
			// 「两条路径合计只产出一个」。本判据只读 sawEnd 且不区分协议，不承担按协议去重的职责。
			alreadyEnded := sawEnd
			sawEnd = sawEnd || finishEnded
			// 收尾分片没有对应的上游帧、没有原始字节，因此两种下沉模式下一律经 Send 交付。
			// 同协议透传路径不得因它向客户端多写一个字节；下沉目标的 Send 契约负责这一点。
			for _, chunk := range finishChunks {
				if alreadyEnded && chunk.Kind == domain.ChunkStreamEnd {
					continue
				}
				if sendErr := sink.Send(ctx, chunk); sendErr != nil {
					return sendErr
				}
			}
			break
		}

		chunks, decodeErr := streamAdapter.DecodeStreamFrame(frame.Event, frame.Data)
		if decodeErr != nil {
			if domainErr := domain.AsError(decodeErr); domainErr != nil {
				return domainErr
			}
			return domain.NewError(domain.CodeUpstreamUnavailable, "上游流式帧无法解码").WithCause(decodeErr)
		}
		// 结束语义与下沉模式无关：两种模式都按分片判定本轮是否收到结束帧。
		for _, chunk := range chunks {
			if chunk.Kind == domain.ChunkStreamEnd {
				sawEnd = true
			}
		}
		if passthrough {
			if sendErr := frameSink.SendFrame(ctx, frame.Raw, chunks); sendErr != nil {
				return sendErr
			}
			continue
		}
		for _, chunk := range chunks {
			if sendErr := sink.Send(ctx, chunk); sendErr != nil {
				return sendErr
			}
		}
	}

	if !sawEnd {
		return domain.NewError(domain.CodeUpstreamUnavailable, "上游流在结束标记出现前断开").
			WithDetail("本轮未收到结束帧，按截断处理")
	}
	return nil
}

// streamTimeoutError 构造可重试的上游流式超时错误。
func streamTimeoutError(cause error) *domain.Error {
	return domain.NewError(domain.CodeUpstreamTimeout, "上游流式调用超时").WithCause(cause)
}

// classifyStreamReadError 把逐帧读取错误分类为可向上报告的统一错误。
//
// 返回 nil 表示流正常收尾：读到 io.EOF，或已产出结束帧后上游未及时关闭连接造成的空闲超时。
// stalled 表示空闲超时看门狗已触发，sawEnd 表示本轮已产出结束帧（含帧内分片与 EOF 收尾分片）。
func classifyStreamReadError(ctx context.Context, readErr error, stalled, sawEnd bool) error {
	if errors.Is(readErr, io.EOF) {
		return nil
	}
	if stalled {
		if sawEnd {
			return nil
		}
		return streamTimeoutError(readErr)
	}
	if ctx.Err() != nil {
		return mapTransportError(ctx, readErr)
	}
	if errors.Is(readErr, sse.ErrFrameTooLarge) {
		return domain.NewError(domain.CodeUpstreamUnavailable, "上游流式单帧超过上限").WithCause(readErr)
	}
	return domain.NewError(domain.CodeUpstreamUnavailable, "读取上游流式响应失败").WithCause(readErr)
}
