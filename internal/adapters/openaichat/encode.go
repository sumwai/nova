package openaichat

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sumwai/nova/internal/domain"
)

// sseFrameOverheadBytes 是 SSE 帧除 JSON 负载外的固定开销：
// "data: " 前缀 6 字节与结尾 "\n\n" 2 字节。
const sseFrameOverheadBytes = 8

// EncodeResponse 把内部统一响应编码为 OpenAI 非流式响应体。
//
// 响应 id 优先取上游标识（同协议透传时客户端看到的就是上游 id），上游未给出时
// 才用本适配器的生成器兜底。
func (a *Adapter) EncodeResponse(resp *domain.Response) ([]byte, error) {
	if resp == nil {
		return nil, domain.NewError(domain.CodeInternal, "响应为空")
	}
	message, err := encodeMessage(resp.Message)
	if err != nil {
		return nil, err
	}
	id := resp.UpstreamRequestID
	if id == "" {
		id = a.id()
	}
	wire := responseWire{
		ID:      id,
		Object:  objectChatCompletion,
		Created: a.timestamp(),
		Model:   resp.Model,
		Choices: []responseChoice{{
			Index:        0,
			Message:      message,
			FinishReason: string(resp.FinishReason),
		}},
		Usage: encodeUsage(resp.Usage),
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, domain.NewError(domain.CodeInternal, "编码响应失败").WithCause(err)
	}
	return body, nil
}

// encodeMessage 把内部消息编码为响应 message：文本片段拼接为 content，工具调用片段转为 tool_calls。
// 响应不允许出现图片、工具结果等片段；推理片段（PartReasoning）在编码方向不下发，显式跳过。
func encodeMessage(message domain.Message) (responseMessage, error) {
	role := string(message.Role)
	if role == "" {
		role = string(domain.RoleAssistant)
	}
	var text strings.Builder
	var toolCalls []toolCallWire
	for i, part := range message.Parts {
		switch part.Kind {
		case domain.PartText:
			text.WriteString(part.Text)
		case domain.PartToolCall:
			if part.ToolCall == nil {
				return responseMessage{}, domain.NewError(domain.CodeInternal, fmt.Sprintf("第 %d 个片段缺少工具调用", i))
			}
			toolCalls = append(toolCalls, toolCallWire{
				ID:   part.ToolCall.ID,
				Type: toolTypeFunction,
				Function: toolCallFunctionWire{
					Name:      part.ToolCall.Name,
					Arguments: part.ToolCall.Arguments,
				},
			})
		case domain.PartReasoning:
			// 推理内容不下发，跳过。
		default:
			return responseMessage{}, domain.NewError(domain.CodeInternal, fmt.Sprintf("响应片段类型 %q 无法编码", string(part.Kind)))
		}
	}
	out := responseMessage{Role: role, ToolCalls: toolCalls}
	// 仅含工具调用时 content 为 null；否则给出文本（允许为空串）。
	if text.Len() > 0 || len(toolCalls) == 0 {
		content := text.String()
		out.Content = &content
	}
	return out, nil
}

// EncodeChunk 把一个内部统一分片编码为 OpenAI SSE 帧（含 "data: " 前缀与结尾空行）。
//
// 只处理文本增量与工具调用增量：用量、结束原因与流结束统一经 EncodeStreamEnd 下发，
// 遇到 ChunkUsage、ChunkFinish、ChunkStreamEnd 一律报错，避免同一语义出现两条下发路径
// （见 internal/domain/ports.go 对 EncodeChunk 的约定）。
func (a *Adapter) EncodeChunk(chunk domain.Chunk) ([]byte, error) {
	switch chunk.Kind {
	case domain.ChunkTextDelta:
		return a.encodeWireChunk(chunkWire{
			ID:      a.id(),
			Object:  objectChatCompletionChunk,
			Created: a.timestamp(),
			Model:   chunk.Model,
			Choices: []chunkChoice{{Index: 0, Delta: chunkDelta{Content: chunk.TextDelta}}},
		})
	case domain.ChunkToolCallDelta:
		if chunk.ToolCall == nil {
			return nil, domain.NewError(domain.CodeInternal, "工具调用分片缺少 tool_call")
		}
		call := chunk.ToolCall
		return a.encodeWireChunk(chunkWire{
			ID:      a.id(),
			Object:  objectChatCompletionChunk,
			Created: a.timestamp(),
			Model:   chunk.Model,
			Choices: []chunkChoice{{
				Index: 0,
				Delta: chunkDelta{ToolCalls: []toolCallDeltaWire{{
					// 写入分片携带的真实下标，并行工具调用才能按 index 正确拼接。
					Index: call.Index,
					ID:    call.ID,
					Type:  toolTypeFunction,
					Function: &toolCallFunctionDeltaWire{
						Name:      call.Name,
						Arguments: call.Arguments,
					},
				}}},
			}},
		})
	case domain.ChunkReasoningDelta:
		// 推理增量在编码方向不下发，显式跳过而不报错。
		return nil, nil
	case domain.ChunkUsage, domain.ChunkFinish, domain.ChunkStreamEnd:
		return nil, domain.NewError(domain.CodeInternal, fmt.Sprintf("分片类型 %q 必须经 EncodeStreamEnd 下发", string(chunk.Kind)))
	default:
		return nil, domain.NewError(domain.CodeInternal, fmt.Sprintf("未知分片类型 %q", string(chunk.Kind)))
	}
}

// encodeWireChunk 把一帧 chunkWire 序列化为 SSE 帧。
func (a *Adapter) encodeWireChunk(wire chunkWire) ([]byte, error) {
	payload, err := json.Marshal(wire)
	if err != nil {
		return nil, domain.NewError(domain.CodeInternal, "编码流式分片失败").WithCause(err)
	}
	frame := make([]byte, 0, len(payload)+sseFrameOverheadBytes)
	frame = append(frame, "data: "...)
	frame = append(frame, payload...)
	frame = append(frame, '\n', '\n')
	return frame, nil
}

// encodeFinishFrame 生成携带 finish_reason 的尾部帧。仅由 EncodeStreamEnd 调用。
func (a *Adapter) encodeFinishFrame(model string, reason domain.FinishReason) ([]byte, error) {
	literal := string(reason)
	return a.encodeWireChunk(chunkWire{
		ID:      a.id(),
		Object:  objectChatCompletionChunk,
		Created: a.timestamp(),
		Model:   model,
		Choices: []chunkChoice{{Index: 0, Delta: chunkDelta{}, FinishReason: &literal}},
	})
}

// encodeUsageFrame 生成 choices 为空、usage 非空的尾部帧。仅由 EncodeStreamEnd 调用。
func (a *Adapter) encodeUsageFrame(model string, usage *domain.Usage) ([]byte, error) {
	return a.encodeWireChunk(chunkWire{
		ID:      a.id(),
		Object:  objectChatCompletionChunk,
		Created: a.timestamp(),
		Model:   model,
		Choices: []chunkChoice{},
		Usage:   encodeUsage(*usage),
	})
}

// encodeUsage 把内部用量映射为 OpenAI usage。
//
// prompt_tokens 取输入 token，completion_tokens 取输出 token，total_tokens 为两者之和。
// 缓存与推理 token 不改变 total_tokens：cached_tokens 已包含在 prompt_tokens 内、
// reasoning_tokens 已包含在 completion_tokens 内，只写入对应的 details 子对象供客户端读取。
func encodeUsage(usage domain.Usage) *usageWire {
	wire := &usageWire{
		PromptTokens:     usage.InputTokens,
		CompletionTokens: usage.OutputTokens,
		TotalTokens:      usage.InputTokens + usage.OutputTokens,
	}
	// 子项全为零时不输出明细对象，避免制造没有信息的零值字段。
	if usage.CacheReadTokens != 0 || usage.CacheWriteTokens != 0 {
		wire.PromptTokensDetails = &promptTokensDetailsWire{
			CachedTokens:     usage.CacheReadTokens,
			CacheWriteTokens: usage.CacheWriteTokens,
		}
	}
	if usage.ReasoningTokens != 0 {
		wire.CompletionTokensDetails = &completionTokensDetailsWire{ReasoningTokens: usage.ReasoningTokens}
	}
	return wire
}
