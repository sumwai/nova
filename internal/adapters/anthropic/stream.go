package anthropic

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/sumwai/nova/internal/domain"
)

// streamEvent 是 Anthropic SSE 事件 data 的并集结构，按事件类型取用字段。
type streamEvent struct {
	Type         string           `json:"type"`
	Index        int              `json:"index"`
	Message      *streamMessage   `json:"message"`
	ContentBlock *contentBlock    `json:"content_block"`
	Delta        streamDelta      `json:"delta"`
	Usage        *deltaUsage      `json:"usage"`
	Error        *wireErrorDetail `json:"error"`
}

// deltaUsage 是 message_delta 事件顶层 usage 对象的解码结构。
//
// Anthropic 官方语义下，message_delta 的 token 计数是本次调用的累计值，且该事件
// 可能只带部分字段，也可能完全不带 usage。为把「上游未给出该字段」与「上游给出 0」
// 区分开，这里用指针字段：nil 表示未给出，非 nil 才参与覆盖累计用量。
type deltaUsage struct {
	InputTokens              *int `json:"input_tokens"`
	OutputTokens             *int `json:"output_tokens"`
	CacheReadInputTokens     *int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int `json:"cache_creation_input_tokens"`
	// OutputTokensDetails 指向与响应侧同一内层类型：nil、叶子缺失或形状非预期都表示「未给出」。
	OutputTokensDetails *outputTokensDetails `json:"output_tokens_details"`
}

type streamMessage struct {
	Model string     `json:"model"`
	Usage *wireUsage `json:"usage"`
}

type streamDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	PartialJSON string `json:"partial_json"`
	// Thinking 是 thinking_delta 的推理文本。signature_delta 的签名不进内容字段。
	Thinking   string `json:"thinking"`
	StopReason string `json:"stop_reason"`
}

// upstreamError 把上游错误事件的错误对象转为统一错误。
//
// 分级与另外两个适配器一致：请求级错误（参数/凭据/权限/资源）不可重试，
// 其余可换渠道重试。上游原文只进 Detail，不作为面向用户的 Message。
func upstreamError(errDetail *wireErrorDetail) error {
	errorType := ""
	detail := "上游返回错误事件"
	if errDetail != nil {
		errorType = errDetail.Type
		detail = appendUpstreamMessage(detail, errDetail.Message)
	}
	code := domain.CodeUpstreamUnavailable
	if domain.NonRetryableUpstreamErrorType(errorType) {
		code = domain.CodeUpstreamRejected
	}
	return domain.NewError(code, "上游返回错误").WithDetail(detail)
}

// appendUpstreamMessage 把上游原文追加到排障文案。上游未给出原文时原样返回。
func appendUpstreamMessage(detail, upstreamMessage string) string {
	if upstreamMessage == "" {
		return detail
	}
	return detail + "；上游原文: " + upstreamMessage
}

// streamDecoder 把一帧 Anthropic SSE 解码为内部统一分片，并跨帧保存累计状态。
//
// 一次上游流使用一个独立实例。它由 Adapter.decodeMu 保护，但正确用法仍是
// 每条流经 NewStream 派生独立实例，不得在流之间复用。
type streamDecoder struct {
	model string
	tools map[int]*domain.ToolCall
	// ignored 记录遇到未识别内容块类型的块下标：其后续增量与结束帧一律忽略，
	// 以免上游新增块类型（server_tool_use 等）把整条流打断。
	ignored map[int]struct{}
	usage   domain.Usage
	// inputTokens 保存 Anthropic 线格式的 input_tokens：该协议下它不含缓存 token，
	// 一次调用的输入总量是它与两个缓存计数之和。每次覆盖
	// （message_start 的基准值或message_delta 的累计覆盖）
	// 之后都经 syncInputTokens 重算 usage.InputTokens，
	// 不得把线格式的 input_tokens 直接写进 usage.InputTokens。
	inputTokens int
	// finishReason 是 message_delta 累积的结束原因，由 message_stop 处的结束分片一并产出。
	// 该协议可以有多个 message_delta，后一次给出非空 stop_reason 时覆盖前一次。
	finishReason domain.FinishReason
	// inputStated 记录上游是否已陈述过输入侧与缓存侧事实：message_start 给出 usage 对象，
	// 或 message_delta 给出任一输入/缓存字段时置位。它只表示「上游说过」：
	// 计数是否非零由调用方按 Usage 判据自行区分。
	inputStated bool
}

// reportedUsage 返回上游已陈述的输入侧与缓存侧事实。
//
// 返回的第二值为 false 表示上游尚未陈述任何输入侧事实（既没有 message_start 的 usage
// 对象，也没有 message_delta 的输入/缓存字段），调用方不得据此结算。输出计数不在此列：
// 它在 message_stop 之前不是最终值，由调用方按已解码内容估算。
func (d *streamDecoder) reportedUsage() (domain.Usage, bool) {
	if !d.inputStated {
		return domain.Usage{}, false
	}
	return domain.Usage{
		Source:           domain.UsageSourceUpstream,
		InputTokens:      d.usage.InputTokens,
		CacheReadTokens:  d.usage.CacheReadTokens,
		CacheWriteTokens: d.usage.CacheWriteTokens,
	}.BoundSubitemsToMain(), true
}

// syncInputTokens 按「线格式 input_tokens + 两个缓存计数」重算内部输入总数。
// 内部统一口径是「子项 ⊆ 主计数」，Anthropic 的线格式是并列相加，差异收敛在此。
func (d *streamDecoder) syncInputTokens() {
	d.usage.InputTokens = d.inputTokens + d.usage.CacheReadTokens + d.usage.CacheWriteTokens
}

// applyDeltaUsage 用 message_delta 的累计用量覆盖已累计状态。
//
// message_delta 的用量是本次调用的累计值（cumulative），也是最终值，因此这里用覆盖
// 而不是累加，并在此时才把来源标为上游。该事件可能只带部分字段或完全不带 usage：
// 只有上游明确给出的字段才覆盖 message_start 的基准值，字段缺失时保留基准值，不得把
// 「未给出」当成 0 清空。同一轮出现多个 message_delta 时，后一次覆盖前一次。
// 思考 token 子项只在嵌套对象与叶子都按预期形状给出时才覆盖。对象缺失、叶子缺失或
// 形状非预期都保留已有值，不清零。
func (d *streamDecoder) applyDeltaUsage(u *deltaUsage) {
	d.usage.Source = domain.UsageSourceUpstream
	if u.InputTokens != nil {
		d.inputTokens = *u.InputTokens
	}
	if u.InputTokens != nil || u.CacheReadInputTokens != nil || u.CacheCreationInputTokens != nil {
		// 上游在 message_delta 里明确给出输入或缓存字段，即视为已陈述输入侧事实。
		d.inputStated = true
	}
	if u.OutputTokens != nil {
		d.usage.OutputTokens = *u.OutputTokens
	}
	if u.CacheReadInputTokens != nil {
		d.usage.CacheReadTokens = *u.CacheReadInputTokens
	}
	if u.CacheCreationInputTokens != nil {
		d.usage.CacheWriteTokens = *u.CacheCreationInputTokens
	}
	if u.OutputTokensDetails != nil && u.OutputTokensDetails.ThinkingTokens.valid {
		d.usage.ReasoningTokens = u.OutputTokensDetails.ThinkingTokens.value
	}
	// 每次覆盖之后都按三者之和重算输入总数，保证内部口径恒成立。
	d.syncInputTokens()
	// 度量倒挂防护：上游把 reasoned 或缓存的子项报得比主计数还大时，抬高主计数后再交付，
	// 否则落库会违反「子项不得大于主计数」的检查约束。
	d.usage = d.usage.BoundSubitemsToMain()
}

// decode 把一帧 SSE 归一化为零个或多个内部分片。
//
// 返回空切片表示该帧只更新内部状态或不产生分片
// （如 message_start、content_block_start、content_block_stop 且当前块不是工具调用、message_delta、ping）。
// 未知事件名与未识别内容块类型按向前兼容忽略（返回空切片），避免上游新增事件导致整条流失败。
// 上游错误帧必须返回错误（可重试性按错误类型分级），不得静默吞掉。
//
// 结束分片与协议自身的终止标记一一对应：Anthropic 只在 message_stop 产出 ChunkStreamEnd，
// message_delta 只累积用量与结束原因、不产出分片。这样共享层才能用一个与协议无关的信号
// （本轮是否产出过 ChunkStreamEnd）判定上游是否在终止标记出现前断开。缺 message_stop 的流
// 按截断处理。该协议可以有多个 message_delta，其用量与结束原因均由 message_stop 处的结束
// 分片一并产出。同一轮出现多个 message_delta 时后一次覆盖前一次。
//
// 关于一帧多载荷：Anthropic 的 delta 是单个对象，其 type 字段只能取一个值，
// 故 content_block_delta 不可能同时携带 text_delta 与 input_json_delta。message_stop
// 的用量与结束原因来自此前累积的状态，同属一个结束分片，不构成多个载荷。
// 因此当前协议下每帧至多产出一个分片。返回值用切片表达，是为满足 domain.Adapter
// 契约中「一帧可产多个分片」的形状（参见 OpenAI 并行工具调用）。
func (d *streamDecoder) decode(event string, data []byte) ([]domain.Chunk, error) {
	var ev streamEvent
	if len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, invalidRequest("流式事件 data 不是合法 JSON")
		}
	}

	// 上游错误帧：event 名或 data.type 任一为 error 都不能返回空切片，否则错误会被静默吞掉。
	// 分级与文案口径与另外两个适配器一致：请求级错误（参数/凭据/权限/资源）不可重试，
	// 其余可换渠道重试。上游原文只进 Detail，不作为面向用户的 Message。
	if event == eventError || ev.Type == eventError {
		return nil, upstreamError(ev.Error)
	}

	switch event {
	case "message_start":
		// 新的一轮流：重置上一轮的累计状态。
		d.tools = nil
		d.ignored = nil
		d.usage = domain.Usage{}
		d.inputTokens = 0
		d.finishReason = ""
		d.model = ""
		d.inputStated = false
		if ev.Message != nil {
			d.model = ev.Message.Model
			// 这里把 message_start 的计数记录为基准值，不标来源：它不是最终值
			// （输出计数尚未产生），只有 message_delta 携带的累计用量才能代表本次
			// 调用的完整用量。流在 message_delta 之前断开时来源保持未取得用量，
			// 结算走兜底分支，而不是把只含输入计数的半份数据当成完整用量。
			if ev.Message.Usage != nil {
				// 上游陈述了本次调用的输入侧事实：即便随后断开，这三项仍是输入侧的权威来源。
				d.inputStated = true
				d.inputTokens = ev.Message.Usage.InputTokens
				d.usage.CacheReadTokens = ev.Message.Usage.CacheReadInputTokens
				d.usage.CacheWriteTokens = ev.Message.Usage.CacheCreationInputTokens
			}
		}
		// 输入总量按线格式 input_tokens 与两个缓存计数之和重算。
		d.syncInputTokens()
		return nil, nil

	case "content_block_start":
		if ev.ContentBlock == nil {
			return nil, invalidRequest("content_block_start 缺少 content_block")
		}
		switch ev.ContentBlock.Type {
		case blockTypeText:
			return nil, nil
		case blockTypeThinking:
			// thinking 块与文本块同一口径：起始帧只登记、不产出内容，后续增量帧
			// 产出 ChunkReasoningDelta。不记入 ignored。
			return nil, nil
		case blockTypeToolUse:
			if ev.ContentBlock.ID == "" || ev.ContentBlock.Name == "" {
				return nil, invalidRequest("tool_use 块缺少 id 或 name")
			}
			if d.tools == nil {
				d.tools = make(map[int]*domain.ToolCall)
			}
			// 用 content_block 下标写入 ToolCall.Index，保证并行工具调用能正确区分与拼接。
			d.tools[ev.Index] = &domain.ToolCall{Index: ev.Index, ID: ev.ContentBlock.ID, Name: ev.ContentBlock.Name}
			return nil, nil
		default:
			// 未识别块类型（redacted_thinking、server_tool_use 及上游新增块）按向前兼容忽略，
			// 并记下下标以便同时忽略它的增量帧。thinking 块不在此列：它已在上方的 case 中登记。
			if d.ignored == nil {
				d.ignored = make(map[int]struct{})
			}
			d.ignored[ev.Index] = struct{}{}
			return nil, nil
		}

	case eventContentBlockDelta:
		if _, skip := d.ignored[ev.Index]; skip {
			return nil, nil
		}
		switch ev.Delta.Type {
		case "text_delta":
			return []domain.Chunk{{Kind: domain.ChunkTextDelta, Model: d.model, TextDelta: ev.Delta.Text}}, nil
		case "thinking_delta":
			if ev.Delta.Thinking == "" {
				return nil, nil
			}
			return []domain.Chunk{{Kind: domain.ChunkReasoningDelta, Model: d.model, TextDelta: ev.Delta.Thinking}}, nil
		case "input_json_delta":
			call, ok := d.tools[ev.Index]
			if !ok {
				return nil, invalidRequest(fmt.Sprintf("第 %d 个内容块没有对应的 tool_use", ev.Index))
			}
			call.Arguments += ev.Delta.PartialJSON
			return nil, nil
		case "signature_delta", "citations_delta":
			// 签名与引用标注不是内容：签名是 thinking 块的附属载荷，引用不进入统一内部格式，两者都忽略。
			return nil, nil
		default:
			// 未识别的增量类型按向前兼容忽略，避免上游新增 delta 类型（如 citations_delta）打断整条流。
			return nil, nil
		}

	case "content_block_stop":
		if _, skip := d.ignored[ev.Index]; skip {
			delete(d.ignored, ev.Index)
			return nil, nil
		}
		call, ok := d.tools[ev.Index]
		if !ok {
			return nil, nil
		}
		delete(d.tools, ev.Index)
		if call.Arguments == "" {
			call.Arguments = "{}"
		}
		return []domain.Chunk{{Kind: domain.ChunkToolCallDelta, Model: d.model, ToolCall: call}}, nil

	case "message_delta":
		// message_delta 只累积状态、不产出任何分片：该协议真正的终止帧是随后的message_stop，
		// 只有它才产出 ChunkStreamEnd。若在此产出结束分片，上游恰好在这两个事件之间断开时，
		// 共享层会把截断流误判为正常结束。非最终message_delta 也会让其后的用量与结束原因被丢弃。
		if ev.Delta.StopReason != "" {
			d.finishReason = decodeFinishReason(ev.Delta.StopReason)
		}
		if ev.Usage != nil {
			d.applyDeltaUsage(ev.Usage)
		}
		return nil, nil

	case "message_stop":
		// message_stop 是 Anthropic 自身的终止标记，与 ChunkStreamEnd 一一对应：
		// 每条流恰好在此产出一个结束分片，携带此前累积的用量与结束原因。用量对象
		// 恒非 nil，未取得用量时是来源未知的零值。
		usage := d.usage
		return []domain.Chunk{{
			Kind:         domain.ChunkStreamEnd,
			Model:        d.model,
			Usage:        &usage,
			FinishReason: d.finishReason,
		}}, nil

	case "ping":
		return nil, nil

	default:
		// 未知事件名按向前兼容忽略，避免上游新增事件导致整条流失败。
		return nil, nil
	}
}
