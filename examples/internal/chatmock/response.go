package chatmock

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// text 是模拟上游回的内容。回显上游收到的模型名，使「nova 到底把哪个上游名发了出去」
// 能直接从响应正文里读出来，而不必依赖日志。
func text(model string) string {
	return "chatmock: " + model
}

// ---------------------------------------------------------------------------
// 非流式响应
// ---------------------------------------------------------------------------

// openAIChatResponse 是 Chat Completions 的完整响应。
func openAIChatResponse(model string) map[string]any {
	return map[string]any{
		"id":      "chatcmpl-chatmock",
		"object":  "chat.completion",
		"created": 0,
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": text(model)},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens":     InputTokens,
			"completion_tokens": OutputTokens,
			"total_tokens":      InputTokens + OutputTokens,
		},
	}
}

// anthropicResponse 是 Anthropic Messages 的完整响应。
//
// 线格式的 input_tokens 不含缓存 token，nova 在解码侧把它与两个缓存计数相加，
// 因此这里不写缓存字段（都为零），输入总数就是 InputTokens。
func anthropicResponse(model string) map[string]any {
	return map[string]any{
		"id":            "msg_chatmock",
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       []any{map[string]any{"type": "text", "text": text(model)}},
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  InputTokens,
			"output_tokens": OutputTokens,
		},
	}
}

// openAIResponsesResponse 是 OpenAI Responses 的完整响应。
func openAIResponsesResponse(model string) map[string]any {
	return map[string]any{
		"id":     "resp_chatmock",
		"object": "response",
		"model":  model,
		"status": "completed",
		"output": []any{map[string]any{
			"type":   "message",
			"id":     "msg_chatmock",
			"role":   "assistant",
			"status": "completed",
			"content": []any{map[string]any{
				"type": "output_text",
				"text": text(model),
			}},
		}},
		"usage": map[string]any{
			"input_tokens":  InputTokens,
			"output_tokens": OutputTokens,
			"total_tokens":  InputTokens + OutputTokens,
		},
	}
}

// geminiResponse 是 generateContent 的完整响应。
func geminiResponse(model string) map[string]any {
	return map[string]any{
		"candidates": []any{map[string]any{
			"content":      map[string]any{"role": "model", "parts": []any{map[string]any{"text": text(model)}}},
			"finishReason": "STOP",
			"index":        0,
		}},
		"modelVersion": model,
		"responseId":   "chatmock",
		"usageMetadata": map[string]any{
			"promptTokenCount":     InputTokens,
			"candidatesTokenCount": OutputTokens,
			"totalTokenCount":      InputTokens + OutputTokens,
		},
	}
}

// ---------------------------------------------------------------------------
// 流式响应
// ---------------------------------------------------------------------------

// sseWriter 按帧写 SSE，并在每帧之后立即刷出。
//
// 刷出是必须的：nova 按「空闲无字节」判超时，整条流攒到函数返回才写会让它看起来
// 一直在推进却收不到帧，也会让同协议透传路径上的客户端等到流结束才拿到第一帧。
type sseWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

// newSSEWriter 备好流式响应头，并取出 Flusher。
//
// 取不到 Flusher 时返回 false：那种 http.ResponseWriter 无法承载 SSE，
// 模拟上游宁可明确失败，也不写出一条被缓冲住、永远不刷出的「流」。
func newSSEWriter(w http.ResponseWriter) (*sseWriter, bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "chatmock: 这个 ResponseWriter 不支持流式", http.StatusInternalServerError)
		return nil, false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	return &sseWriter{w: w, f: flusher}, true
}

// frame 写一帧。event 为空时只写 data 字段（Gemini 与 Chat Completions 都没有事件名）。
func (s *sseWriter) frame(event string, payload any) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return
	}
	if event != "" {
		fmt.Fprintf(s.w, "event: %s\n", event)
	}
	fmt.Fprintf(s.w, "data: %s\n\n", encoded)
	s.f.Flush()
}

// raw 写一帧不由 JSON 编码的载荷（Chat Completions 的 [DONE] 哨兵）。
func (s *sseWriter) raw(line string) {
	fmt.Fprintf(s.w, "data: %s\n\n", line)
	s.f.Flush()
}

// writeOpenAIChatStream 写一条 Chat Completions 流。
//
// 帧序按官方形态：role 起始帧、内容帧、带 finish_reason 的帧、只承载用量的帧、[DONE]。
// 用量帧的 choices 为空数组而不是省略：nova 用它区分「只承载用量」的帧与探活帧。
func writeOpenAIChatStream(w http.ResponseWriter, model string) {
	stream, ok := newSSEWriter(w)
	if !ok {
		return
	}
	chunk := func(choices any, usage any) map[string]any {
		payload := map[string]any{
			"id":      "chatcmpl-chatmock",
			"object":  "chat.completion.chunk",
			"created": 0,
			"model":   model,
			"choices": choices,
		}
		if usage != nil {
			payload["usage"] = usage
		}
		return payload
	}
	stream.frame("", chunk([]any{map[string]any{
		"index": 0, "delta": map[string]any{"role": "assistant"}, "finish_reason": nil,
	}}, nil))
	stream.frame("", chunk([]any{map[string]any{
		"index": 0, "delta": map[string]any{"content": text(model)}, "finish_reason": nil,
	}}, nil))
	stream.frame("", chunk([]any{map[string]any{
		"index": 0, "delta": map[string]any{}, "finish_reason": "stop",
	}}, nil))
	stream.frame("", chunk([]any{}, map[string]any{
		"prompt_tokens":     InputTokens,
		"completion_tokens": OutputTokens,
		"total_tokens":      InputTokens + OutputTokens,
	}))
	stream.raw("[DONE]")
}

// writeAnthropicStream 写一条 Anthropic Messages 流。
//
// 用量分两处陈述：message_start 给输入侧基准值，message_delta 给累计的最终值。
// 结束信号是真实的 message_stop 帧：nova 只在这一帧产出结束分片，缺它的流按截断处理。
func writeAnthropicStream(w http.ResponseWriter, model string) {
	stream, ok := newSSEWriter(w)
	if !ok {
		return
	}
	stream.frame("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": "msg_chatmock", "type": "message", "role": "assistant", "model": model,
			"content": []any{},
			"usage":   map[string]any{"input_tokens": InputTokens, "output_tokens": 0},
		},
	})
	stream.frame("content_block_start", map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
	stream.frame("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": 0,
		"delta": map[string]any{"type": "text_delta", "text": text(model)},
	})
	stream.frame("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	stream.frame("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": OutputTokens},
	})
	stream.frame("message_stop", map[string]any{"type": "message_stop"})
}

// writeOpenAIResponsesStream 写一条 OpenAI Responses 流。
//
// Responses 的增量一律带事件名，帧内 type 与事件名同值：nova 两者都读，
// 只给其中一个也能工作，两个都给才能覆盖「事件名缺失时按 data.type 判定」那条路径。
func writeOpenAIResponsesStream(w http.ResponseWriter, model string) {
	stream, ok := newSSEWriter(w)
	if !ok {
		return
	}
	stream.frame("response.created", map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id": "resp_chatmock", "object": "response", "model": model, "status": "in_progress",
		},
	})
	stream.frame("response.output_text.delta", map[string]any{
		"type": "response.output_text.delta", "delta": text(model),
	})
	stream.frame("response.completed", map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id": "resp_chatmock", "object": "response", "model": model, "status": "completed",
			"usage": map[string]any{
				"input_tokens":  InputTokens,
				"output_tokens": OutputTokens,
				"total_tokens":  InputTokens + OutputTokens,
			},
		},
	})
}

// writeGeminiStream 写一条 streamGenerateContent 流。
//
// 没有独立的开始帧，也没有 [DONE] 一类的哨兵：一帧就是一份不完整的响应，
// 结束信号是最后一帧里候选的 finishReason。用量是累计值，随最后一帧一并给出。
func writeGeminiStream(w http.ResponseWriter, model string) {
	stream, ok := newSSEWriter(w)
	if !ok {
		return
	}
	stream.frame("", map[string]any{
		"candidates": []any{map[string]any{
			"content": map[string]any{"role": "model", "parts": []any{map[string]any{"text": text(model)}}},
			"index":   0,
		}},
		"modelVersion": model,
	})
	stream.frame("", map[string]any{
		"candidates": []any{map[string]any{
			"content":      map[string]any{"role": "model", "parts": []any{}},
			"finishReason": "STOP",
			"index":        0,
		}},
		"modelVersion": model,
		"usageMetadata": map[string]any{
			"promptTokenCount":     InputTokens,
			"candidatesTokenCount": OutputTokens,
			"totalTokenCount":      InputTokens + OutputTokens,
		},
	})
}
