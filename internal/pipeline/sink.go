package pipeline

import (
	"context"
	"fmt"
	"io"

	"github.com/sumwai/nova/internal/domain"
)

// attemptSink 是单次流式尝试的下沉目标：除 domain.ChunkSink 外，还向流水线报告
//   - 是否已向客户端写出字节（决定能否换渠道重试）
//   - 是否写出失败（决定终态是否还能向该写出目标补发字节）
//   - 响应侧改写标注
//   - 本次尝试取得的用量
type attemptSink interface {
	domain.ChunkSink
	// wroteBytes 报告本次尝试是否已向客户端写出过字节。
	wroteBytes() bool
	// writeFailed 报告本次尝试是否曾向客户端写出并拿到错误，即终止错误是否本端写出失败。
	// 它不按已写字节数判断：写出目标在失败时报告的字节数不可信，可靠事实只有「调用是否返回错误」。
	writeFailed() bool
	// rewriteParts 返回响应侧改写标注。
	rewriteParts() domain.RewriteParts
	// collectedUsage 返回本次尝试取到的用量：取「最后一个携带用量的分片」，
	// 未取得时返回来源未知的零值。
	collectedUsage() domain.Usage
}

// passthroughSink 是「客户端协议与上游协议一致」时的下沉目标。
//
// 它实现 domain.FrameSink，把上游一帧的原始字节原样写给客户端；分片只用于记账，
// 不再被编码给客户端，避免出现「原始帧 + 网关自己编码的帧」的双写。
type passthroughSink struct {
	out io.Writer
	// usage 是最后一个携带用量的分片给出的用量；未取得时为零值。
	usage domain.Usage
	// wrote 记录是否已向客户端写出过字节。
	wrote bool
	// failed 记录向客户端写出时是否拿到过错误（写出失败）。
	failed bool
}

var (
	_ domain.FrameSink = (*passthroughSink)(nil)
	_ attemptSink      = (*passthroughSink)(nil)
)

// Send 只接受结束分片：EOF 收尾分片（domain.Adapter.FinishStream 的返回值）没有对应的
// 上游帧、没有原始字节可交付，上游调用方经本方法交付它。透传路径不得因收尾分片向客户端
// 多写一个字节，故这里只记录其用量，既不写出任何字节，也不据此把「已向客户端写出字节」
// 置位。其余分片类型走 SendFrame，经本方法到达属调用方错误，仍按内部错误拒绝。
func (s *passthroughSink) Send(_ context.Context, chunk domain.Chunk) error {
	if chunk.Kind != domain.ChunkStreamEnd {
		return domain.NewError(domain.CodeInternal, "透传下沉目标不得接收结束分片以外的逐分片调用")
	}
	s.recordUsage(chunk)
	return nil
}

// SendFrame 原样写出上游帧，并先按该帧解出的分片记账。
func (s *passthroughSink) SendFrame(_ context.Context, raw []byte, chunks []domain.Chunk) error {
	for _, chunk := range chunks {
		s.recordUsage(chunk)
	}
	s.wrote = true
	if _, err := s.out.Write(raw); err != nil {
		s.failed = true
		return clientWriteError(err)
	}
	return nil
}

// recordUsage 记下分片携带的用量；后到的用量覆盖先到的。
func (s *passthroughSink) recordUsage(chunk domain.Chunk) {
	if chunk.Usage != nil {
		s.usage = *chunk.Usage
	}
}

func (s *passthroughSink) wroteBytes() bool { return s.wrote }

func (s *passthroughSink) writeFailed() bool { return s.failed }

func (s *passthroughSink) collectedUsage() domain.Usage { return s.usage }

// rewriteParts 报告响应侧改写标注。透传路径逐字节写出上游原始帧、不做任何改写，故恒为空。
func (s *passthroughSink) rewriteParts() domain.RewriteParts { return nil }

// rebuildSink 是「客户端协议与上游协议不一致」时的下沉目标。
//
// 它只实现 domain.ChunkSink：把上游分片按面向客户端的协议重新编码后写出。
// 用量分片只用于记账，不交给编码方向。
type rebuildSink struct {
	out    io.Writer
	client domain.Adapter
	// startFrame 是流开始帧，延迟到首个字节真正要写出时才发，
	// 使「上游在写出前失败」仍可换渠道重试。
	startFrame []byte
	started    bool
	usage      domain.Usage
	wrote      bool
	// failed 记录向客户端写出时是否拿到过错误（写出失败）。
	failed bool
}

var _ attemptSink = (*rebuildSink)(nil)

// newRebuildSink 为一条流构造重建下沉目标，并预先生成流开始帧。
//
// model 是写入开始帧的模型名，由调用方按 Route.UpstreamModel 决定。
func newRebuildSink(out io.Writer, client domain.Adapter, model string) (*rebuildSink, error) {
	startFrame, err := client.EncodeStreamStart(model)
	if err != nil {
		return nil, err
	}
	return &rebuildSink{out: out, client: client, startFrame: startFrame}, nil
}

// Send 把一个上游分片编码为面向客户端的帧并写出。
//
// ChunkUsage 只出现在解码方向，必须在此跳过；结束分片按上游给出的用量原样交适配器编码。
func (s *rebuildSink) Send(_ context.Context, chunk domain.Chunk) error {
	if chunk.Usage != nil {
		s.usage = *chunk.Usage
	}
	if chunk.Kind == domain.ChunkUsage {
		return nil
	}
	var (
		frame []byte
		err   error
	)
	switch chunk.Kind {
	case domain.ChunkTextDelta, domain.ChunkToolCallDelta:
		frame, err = s.client.EncodeChunk(chunk)
	case domain.ChunkReasoningDelta:
		// 推理分片不产生客户端字节：编码方向不下发推理内容，
		// 因此不置位 wrote，不得因它关闭换渠道重试。
		return nil
	case domain.ChunkStreamEnd:
		frame, err = s.client.EncodeStreamEnd(chunk)
	default:
		return domain.NewError(domain.CodeInternal,
			fmt.Sprintf("流式分片类型 %q 无法下沉", string(chunk.Kind)))
	}
	if err != nil {
		return err
	}
	return s.write(frame)
}

// write 在首次写出字节前补发流开始帧，避免客户端收到缺少开头的事件序列。
func (s *rebuildSink) write(frame []byte) error {
	if !s.started {
		s.started = true
		if len(s.startFrame) > 0 {
			s.wrote = true
			if _, err := s.out.Write(s.startFrame); err != nil {
				s.failed = true
				return clientWriteError(err)
			}
		}
	}
	if len(frame) == 0 {
		return nil
	}
	s.wrote = true
	if _, err := s.out.Write(frame); err != nil {
		s.failed = true
		return clientWriteError(err)
	}
	return nil
}

func (s *rebuildSink) wroteBytes() bool { return s.wrote }

func (s *rebuildSink) writeFailed() bool { return s.failed }

func (s *rebuildSink) collectedUsage() domain.Usage { return s.usage }

// rewriteParts 报告响应侧改写标注：确实写出过网关编码字节（含开始帧）时，
// 面向客户端的响应由网关重建。
func (s *rebuildSink) rewriteParts() domain.RewriteParts {
	if !s.wrote {
		return nil
	}
	return domain.RewriteParts{domain.RewritePartResponseReencoded}
}

// clientWriteError 把面向客户端的写出失败包装为不可重试的平台内部错误。
func clientWriteError(err error) error {
	return domain.NewError(domain.CodeInternal, "写出客户端响应失败").WithCause(err)
}
