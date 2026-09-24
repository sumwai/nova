package gemini

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sumwai/nova/internal/domain"
)

// ---------------------------------------------------------------------------
// 非流式响应解码（面向上游）
// ---------------------------------------------------------------------------

// DecodeResponse 把 Gemini 的 generateContent 响应体归一化为统一响应。
func (a *Adapter) DecodeResponse(body []byte) (*domain.Response, error) {
	var wire wireResponse
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, invalidRequest("响应体不是合法 JSON")
	}
	if len(wire.Candidates) == 0 {
		// 输入侧被拦截时上游只给 promptFeedback、不给候选。这不是「空响应」，
		// 而是一次有结论的拒绝，按内容过滤上报，让客户端拿到一个明确的原因。
		if wire.PromptFeedback != nil && wire.PromptFeedback.BlockReason != nil &&
			strings.TrimSpace(*wire.PromptFeedback.BlockReason) != "" {
			return &domain.Response{
				Model:             wire.ModelVersion,
				Message:           domain.Message{Role: domain.RoleAssistant},
				FinishReason:      domain.FinishContentFilter,
				Usage:             usageFromWire(wire.UsageMetadata),
				UpstreamRequestID: wire.ResponseID,
			}, nil
		}
		return nil, invalidRequest("响应没有 candidates")
	}

	// 统一内部格式只保留第一个候选。候选索引是上游的多候选编号，与内部消息无关。
	candidate := &wire.Candidates[0]
	parts, _, err := decodeParts(candidate.Content.Parts, true)
	if err != nil {
		return nil, err
	}
	reason := domain.FinishUnknown
	if candidate.FinishReason != nil {
		reason = decodeFinishReason(*candidate.FinishReason)
	}
	return &domain.Response{
		Model:             wire.ModelVersion,
		Message:           domain.Message{Role: domain.RoleAssistant, Parts: parts},
		FinishReason:      finishWithToolCalls(reason, parts),
		Usage:             usageFromWire(wire.UsageMetadata),
		UpstreamRequestID: wire.ResponseID,
	}, nil
}

// finishWithToolCalls 在响应确实携带工具调用时把结束原因提升为 tool_calls。
//
// Gemini 用 STOP 表示「模型这一轮说完了」，工具调用意图只体现在 content.parts 里的
// functionCall 片段上。内部枚举把「请求调用工具」单列一类，跨协议重建出去的
// finish_reason 才与另外两种协议一致。只在原本是 stop 或未知时提升：
// 长度截断与内容过滤是更强的结论，不能被一次工具调用覆盖。
func finishWithToolCalls(reason domain.FinishReason, parts []domain.Part) domain.FinishReason {
	if reason != domain.FinishStop && reason != domain.FinishUnknown {
		return reason
	}
	for i := range parts {
		if parts[i].Kind == domain.PartToolCall {
			return domain.FinishToolCalls
		}
	}
	return reason
}

// ---------------------------------------------------------------------------
// 流式响应解码（面向上游）
// ---------------------------------------------------------------------------

// streamDecoder 把一帧 Gemini 流式响应解码为内部统一分片，并跨帧保存累计状态。
//
// 一次上游流使用一个独立实例（由 NewStream 派生）。Gemini 的流没有开始帧，
// 因此没有「新的一轮」标记可用于重置状态：实例不得跨流复用。
type streamDecoder struct {
	// model 是最近一帧给出的模型版本，作为分片与结束分片的模型名。
	model string
	// usage 是最近一帧给出的用量。Gemini 的 usageMetadata 是累计值，
	// 后一帧覆盖前一帧，结束分片携带最后一帧的值。
	usage domain.Usage
	// finish 是累计的结束原因；finishSeen 报告是否见到过有效的结束原因。
	finish     domain.FinishReason
	finishSeen bool
	// sawToolCall 报告本轮是否产出过工具调用分片，用于把 STOP 提升为 tool_calls。
	sawToolCall bool
	// ended 报告本轮是否已产出结束分片。产出后不再处理任何帧，
	// 保证一条流恰好一个 ChunkStreamEnd（见 domain.Adapter 的约定）。
	ended bool
	// partials 是按候选下标累计的未完成工具调用（流式参数分片）。
	partials map[int]*geminiPartialCall
	// nextCall 是按候选下标计数的工具调用序号，用作内部 ToolCall.Index：
	// Gemini 的 functionCall 片段没有稳定下标，而内部格式要靠下标区分并行调用。
	nextCall map[int]int
}

// DecodeStreamFrame 把上游一帧解码为零个或多个内部分片。
//
// Gemini 的 SSE 帧没有事件名（event 为空），帧载荷就是一份不完整的响应，
// 可能只带片段、只带结束原因或只带用量。event 因此不参与判定，保留入参只为满足统一接口。
//
// 结束分片与协议的终止信号一一对应：Gemini 用「候选给出有效 finishReason」作为终止，
// 因此本方法在该帧产出唯一的 ChunkStreamEnd。EOF 处不补收尾分片（见 FinishStream），
// 缺 finishReason 的流因此仍按截断处理。
func (a *Adapter) DecodeStreamFrame(_ string, data []byte) ([]domain.Chunk, error) {
	a.decodeMu.Lock()
	defer a.decodeMu.Unlock()
	return a.decoder.decode(data)
}

// FinishStream 恒返回空。
//
// Gemini 的结束信号是真实帧里的 finishReason，Decoder 已在 DecodeStreamFrame 中产出
// 唯一的 ChunkStreamEnd；没有 finishReason 就说明流在上游给出结论前断了，
// 按截断处理，不得由 EOF 补一个「正常结束」。
func (a *Adapter) FinishStream() []domain.Chunk { return nil }

// decode 把一帧 SSE 载荷归一化为零个或多个内部分片。
func (d *streamDecoder) decode(data []byte) ([]domain.Chunk, error) {
	if d.ended {
		// 已产出结束分片：终止信号之后的帧不再有语义。忽略而不是报错，
		// 兼容上游在结束帧后追加用量汇总帧的写法。
		return nil, nil
	}
	var wire wireResponse
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, invalidRequest("流式帧不是合法 JSON")
	}
	if wire.ModelVersion != "" {
		d.model = wire.ModelVersion
	}
	// usageMetadata 是累计值，后到的覆盖先到的；缺失时保留已累计的值。
	if wire.UsageMetadata != nil {
		d.usage = usageFromWire(wire.UsageMetadata)
	}

	var chunks []domain.Chunk
	for i := range wire.Candidates {
		candidate := &wire.Candidates[i]
		parts, err := d.decodeCandidateParts(candidate)
		if err != nil {
			return nil, err
		}
		chunks = append(chunks, parts...)
		if candidate.FinishReason == nil {
			continue
		}
		literal := strings.TrimSpace(*candidate.FinishReason)
		if literal == "" || literal == finishReasonUnspecified {
			continue
		}
		d.finish = decodeFinishReason(literal)
		d.finishSeen = true
	}
	if !d.finishSeen {
		return chunks, nil
	}

	reason := d.finish
	if d.sawToolCall && reason == domain.FinishStop {
		// 与解码非流式响应同一口径：本轮调用过工具时把 STOP 提升为 tool_calls。
		reason = domain.FinishToolCalls
	}
	usage := d.usage
	d.ended = true
	chunks = append(chunks, domain.Chunk{
		Kind:         domain.ChunkStreamEnd,
		Model:        d.model,
		Usage:        &usage,
		FinishReason: reason,
	})
	return chunks, nil
}

// decodeCandidateParts 解码一个候选的片段。
//
// 未完成的工具调用只更新累计状态、不产出分片；完成时才产出一个携带完整参数的分片。
func (d *streamDecoder) decodeCandidateParts(candidate *wireCandidate) ([]domain.Chunk, error) {
	var chunks []domain.Chunk
	for i := range candidate.Content.Parts {
		part := &candidate.Content.Parts[i]
		switch {
		case part.FunctionCall != nil:
			chunk, err := d.decodeFunctionCall(candidate.Index, part.FunctionCall)
			if err != nil {
				return nil, err
			}
			if chunk != nil {
				d.sawToolCall = true
				chunks = append(chunks, *chunk)
			}
		case part.InlineData != nil:
			if part.InlineData.MimeType == "" || part.InlineData.Data == "" {
				continue
			}
			// 内部统一分片没有图片类型。把内联图片写成一个 markdown data URI 文本分片：
			// 图片生成类模型的流式结果因此不会在跨协议重建时被整段丢掉。
			chunks = append(chunks, domain.Chunk{
				Kind:      domain.ChunkTextDelta,
				Model:     d.model,
				TextDelta: markdownMedia(part.InlineData),
			})
		case part.Text != "":
			kind := domain.ChunkTextDelta
			if part.Thought {
				kind = domain.ChunkReasoningDelta
			}
			chunks = append(chunks, domain.Chunk{Kind: kind, Model: d.model, TextDelta: part.Text})
		}
	}
	return chunks, nil
}

// decodeFunctionCall 处理一个 functionCall 片段，未完成时返回 nil。
//
// 完整调用（没有 partialArgs、也没有 willContinue）直接产出分片；分片形态先写进
// 按候选下标累计的参数对象，最后一个分片（willContinue 为假或缺失）到达时才产出。
func (d *streamDecoder) decodeFunctionCall(candidate int, call *wireFunctionCall) (*domain.Chunk, error) {
	if len(call.PartialArgs) == 0 && call.WillContinue == nil {
		return d.newToolCallChunk(candidate, call.ID, call.FunctionName, call.Arguments), nil
	}
	if d.partials == nil {
		d.partials = make(map[int]*geminiPartialCall)
	}
	partial := d.partials[candidate]
	if partial == nil {
		partial = &geminiPartialCall{}
		d.partials[candidate] = partial
	}
	if strings.TrimSpace(call.ID) != "" {
		partial.id = call.ID
	}
	if name := strings.TrimSpace(call.FunctionName); name != "" {
		partial.name = name
	}
	if err := partial.applyPartialArgs(call.PartialArgs); err != nil {
		return nil, invalidRequest(fmt.Sprintf("流式工具调用参数分片非法：%s", err))
	}
	if call.WillContinue != nil && *call.WillContinue {
		return nil, nil
	}
	delete(d.partials, candidate)
	if strings.TrimSpace(partial.name) == "" {
		return nil, invalidRequest("流式工具调用在完成时仍没有函数名")
	}
	return d.newToolCallChunk(candidate, partial.id, partial.name, partial.args), nil
}

// newToolCallChunk 产出一个携带完整参数的工具调用分片。
func (d *streamDecoder) newToolCallChunk(candidate int, id, name string, arguments any) *domain.Chunk {
	if d.nextCall == nil {
		d.nextCall = make(map[int]int)
	}
	index := d.nextCall[candidate]
	d.nextCall[candidate] = index + 1
	encoded := compactJSON(marshalRaw(arguments))
	if encoded == "" {
		encoded = jsonObjectEmpty
	}
	return &domain.Chunk{
		Kind:  domain.ChunkToolCallDelta,
		Model: d.model,
		ToolCall: &domain.ToolCall{
			Index:     index,
			ID:        orGeneratedCallID(id),
			Name:      name,
			Arguments: encoded,
		},
	}
}

// markdownMedia 把一个内联媒体片段渲染成 markdown data URI 文本。
//
// 图片与其它媒体都写成 markdown：OpenAI 与 Anthropic 的文本增量里没有内联二进制的位置，
// 这是内部文本分片能承载它的唯一形态。
func markdownMedia(data *wireInlineData) string {
	label := "[media]"
	if strings.HasPrefix(strings.ToLower(data.MimeType), "image/") {
		label = "![image]"
	}
	return fmt.Sprintf("%s(data:%s;base64,%s)", label, data.MimeType, data.Data)
}
