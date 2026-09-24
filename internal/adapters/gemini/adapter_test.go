package gemini

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/sumwai/nova/internal/domain"
)

// boundAdapter 绑定一条客户端路径，供解码方向的用例复用。
func boundAdapter(t *testing.T, path string, query url.Values) domain.Adapter {
	t.Helper()
	return New().BindRequest(path, query)
}

// outputLimit 返回指向给定取值的指针，用于构造改写选项里的渠道输出上限。
func outputLimit(value int) *int { return &value }

// TestMatchRequestPath 守护客户端路径的解析：模型名、流式标记与不受支持的路径。
func TestMatchRequestPath(t *testing.T) {
	cases := []struct {
		name       string
		path       string
		wantModel  string
		wantStream bool
		wantOK     bool
	}{
		{
			name:      "v1beta 非流式",
			path:      "/v1beta/models/gemini-2.5-flash:generateContent",
			wantModel: "gemini-2.5-flash",
			wantOK:    true,
		},
		{
			name:       "v1beta 流式",
			path:       "/v1beta/models/gemini-2.5-pro:streamGenerateContent",
			wantModel:  "gemini-2.5-pro",
			wantStream: true,
			wantOK:     true,
		},
		{
			name:      "v1 版本根",
			path:      "/v1/models/gemini-2.0-flash:generateContent",
			wantModel: "gemini-2.0-flash",
			wantOK:    true,
		},
		{
			name:   "未带 models 前缀",
			path:   "/v1beta/gemini-2.5-flash:generateContent",
			wantOK: false,
		},
		{
			name:   "未知动作",
			path:   "/v1beta/models/gemini-2.5-flash:countTokens",
			wantOK: false,
		},
		{
			name:   "其它协议的端点",
			path:   "/v1/chat/completions",
			wantOK: false,
		},
		{
			name:   "缺少版本根",
			path:   "/models/gemini-2.5-flash:generateContent",
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model, stream, ok := MatchRequestPath(tc.path)
			if ok != tc.wantOK {
				t.Fatalf("MatchRequestPath(%q) 命中应为 %v，实际 %v", tc.path, tc.wantOK, ok)
			}
			if model != tc.wantModel {
				t.Errorf("模型名 = %q，期望 %q", model, tc.wantModel)
			}
			if stream != tc.wantStream {
				t.Errorf("流式标记 = %v，期望 %v", stream, tc.wantStream)
			}
		})
	}
}

// TestBindRequestStreamFromQuery 守护 alt=sse 也算流式。
func TestBindRequestStreamFromQuery(t *testing.T) {
	adapter := boundAdapter(t, "/v1beta/models/gemini-2.5-flash:generateContent",
		url.Values{"alt": {"sse"}})
	req, err := adapter.DecodeRequest([]byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
	if err != nil {
		t.Fatalf("解码请求失败：%v", err)
	}
	if !req.Stream {
		t.Fatal("alt=sse 的请求应判为流式")
	}
}

// TestDecodeRequest 覆盖客户端请求的解码：system、图片、工具调用与工具结果、生成参数与工具选择。
func TestDecodeRequest(t *testing.T) {
	body := `{
		"systemInstruction": {"parts": [{"text": "你是助手"}]},
		"contents": [
			{"role": "user", "parts": [
				{"text": "看图"},
				{"inlineData": {"mimeType": "image/png", "data": "QUJD"}}
			]},
			{"role": "model", "parts": [
				{"text": "内部推理", "thought": true},
				{"functionCall": {"id": "call_1", "name": "lookup", "args": {"q": "x"}}}
			]},
			{"role": "user", "parts": [
				{"functionResponse": {"id": "call_1", "name": "lookup", "response": {"ok": true}}}
			]}
		],
		"generationConfig": {"temperature": 0.5, "maxOutputTokens": 128},
		"tools": [{"functionDeclarations": [{
			"name": "lookup",
			"description": "查询",
			"parameters": {"type": "object", "additionalProperties": false, "properties": {"q": {"type": "string"}}}
		}]}],
		"toolConfig": {"functionCallingConfig": {"mode": "ANY", "allowedFunctionNames": ["lookup"]}}
	}`
	adapter := boundAdapter(t, "/v1beta/models/gemini-2.5-flash:generateContent", nil)
	req, err := adapter.DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("解码请求失败：%v", err)
	}

	if req.Protocol != domain.ProtocolGemini {
		t.Errorf("协议 = %q，期望 %q", string(req.Protocol), string(domain.ProtocolGemini))
	}
	if req.Model != "gemini-2.5-flash" {
		t.Errorf("模型 = %q，期望来自路径的 gemini-2.5-flash", req.Model)
	}
	if req.Stream {
		t.Error("非流式动作不应判为流式")
	}
	if req.MaxTokens != 128 {
		t.Errorf("MaxTokens = %d，期望 128", req.MaxTokens)
	}
	if req.Temperature == nil || *req.Temperature != 0.5 {
		t.Errorf("Temperature = %v，期望 0.5", req.Temperature)
	}
	if len(req.RawBody) == 0 {
		t.Error("RawBody 应保留原始请求体")
	}

	if len(req.Messages) != 4 {
		t.Fatalf("消息数 = %d，期望 4（system + user + model + user）", len(req.Messages))
	}
	if req.Messages[0].Role != domain.RoleSystem || req.Messages[0].Parts[0].Text != "你是助手" {
		t.Errorf("首条消息应为 system，实际 %+v", req.Messages[0])
	}
	// user 消息：文本 + 图片 data URI。
	if len(req.Messages[1].Parts) != 2 {
		t.Fatalf("user 片段数 = %d，期望 2", len(req.Messages[1].Parts))
	}
	if got := req.Messages[1].Parts[1].ImageURL; got != "data:image/png;base64,QUJD" {
		t.Errorf("图片 URL = %q", got)
	}
	// model 消息：推理片段在请求方向被跳过，只剩工具调用。
	if len(req.Messages[2].Parts) != 1 || req.Messages[2].Parts[0].Kind != domain.PartToolCall {
		t.Fatalf("model 消息应只保留工具调用，实际 %+v", req.Messages[2].Parts)
	}
	call := req.Messages[2].Parts[0].ToolCall
	if call.ID != "call_1" || call.Name != "lookup" || call.Arguments != `{"q":"x"}` {
		t.Errorf("工具调用 = %+v", call)
	}
	if req.Messages[3].Parts[0].ToolResult.ToolCallID != "call_1" {
		t.Errorf("工具结果 = %+v", req.Messages[3].Parts[0].ToolResult)
	}

	if len(req.Tools) != 1 || req.Tools[0].Name != "lookup" {
		t.Fatalf("工具表 = %+v", req.Tools)
	}
	if req.ToolChoice.Mode != domain.ToolChoiceTool || req.ToolChoice.Name != "lookup" {
		t.Errorf("工具选择 = %+v，期望指定 lookup", req.ToolChoice)
	}
}

// TestDecodeRequestRejectsMissingPathBinding 守护「未绑定路径就解码」这一编程错误被显式拒绝。
func TestDecodeRequestRejectsMissingPathBinding(t *testing.T) {
	_, err := New().DecodeRequest([]byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
	if err == nil {
		t.Fatal("未绑定路径时解码应报错")
	}
	if e := domain.AsError(err); e == nil || e.Code != domain.CodeInvalidRequest {
		t.Fatalf("错误码 = %v", err)
	}
}

// TestDecodeResponse 覆盖上游非流式响应的解码：文本、推理、工具调用、结束原因与用量。
func TestDecodeResponse(t *testing.T) {
	body := `{
		"candidates": [{
			"content": {"role": "model", "parts": [
				{"text": "你好"},
				{"text": "想一想", "thought": true},
				{"functionCall": {"name": "lookup", "args": {"q": "x"}}}
			]},
			"finishReason": "STOP",
			"index": 0
		}],
		"usageMetadata": {
			"promptTokenCount": 10,
			"toolUsePromptTokenCount": 2,
			"candidatesTokenCount": 3,
			"thoughtsTokenCount": 2,
			"totalTokenCount": 17,
			"cachedContentTokenCount": 4
		},
		"modelVersion": "gemini-2.5-flash-001",
		"responseId": "resp-1"
	}`
	resp, err := New().DecodeResponse([]byte(body))
	if err != nil {
		t.Fatalf("解码响应失败：%v", err)
	}
	if resp.Model != "gemini-2.5-flash-001" {
		t.Errorf("模型 = %q", resp.Model)
	}
	if len(resp.Message.Parts) != 3 {
		t.Fatalf("片段数 = %d，期望 3", len(resp.Message.Parts))
	}
	if resp.Message.Parts[0].Kind != domain.PartText || resp.Message.Parts[0].Text != "你好" {
		t.Errorf("首片段 = %+v", resp.Message.Parts[0])
	}
	if resp.Message.Parts[1].Kind != domain.PartReasoning || resp.Message.Parts[1].Text != "想一想" {
		t.Errorf("推理片段 = %+v", resp.Message.Parts[1])
	}
	call := resp.Message.Parts[2].ToolCall
	if call == nil || call.Name != "lookup" || call.Arguments != `{"q":"x"}` {
		t.Fatalf("工具调用 = %+v", call)
	}
	if call.ID == "" {
		t.Error("缺少 id 的工具调用应补一个本地标识")
	}
	if resp.FinishReason != domain.FinishToolCalls {
		t.Errorf("结束原因 = %q，期望由 STOP 提升为 tool_calls", string(resp.FinishReason))
	}
	// 输入 = promptTokenCount + toolUsePromptTokenCount；输出 = candidates + thoughts。
	if resp.Usage.InputTokens != 12 || resp.Usage.OutputTokens != 5 {
		t.Errorf("用量 = %+v，期望输入 12、输出 5", resp.Usage)
	}
	if resp.Usage.ReasoningTokens != 2 || resp.Usage.CacheReadTokens != 4 {
		t.Errorf("用量子项 = %+v", resp.Usage)
	}
	if resp.Usage.Source != domain.UsageSourceUpstream {
		t.Errorf("用量来源 = %q", resp.Usage.Source)
	}
}

// TestDecodeResponseBlockedPrompt 覆盖输入被拦截、上游只回 promptFeedback 的情形。
func TestDecodeResponseBlockedPrompt(t *testing.T) {
	blocked := "SAFETY"
	wire := wireResponse{PromptFeedback: &wirePromptFeedback{BlockReason: &blocked}}
	body, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := New().DecodeResponse(body)
	if err != nil {
		t.Fatalf("解码被拦截响应失败：%v", err)
	}
	if resp.FinishReason != domain.FinishContentFilter {
		t.Errorf("结束原因 = %q，期望 content_filter", string(resp.FinishReason))
	}
}

// TestDecodeResponseRejectsEmptyCandidates 守护「既没有候选、也没有拦截原因」的响应被拒绝。
func TestDecodeResponseRejectsEmptyCandidates(t *testing.T) {
	if _, err := New().DecodeResponse([]byte(`{"candidates":[]}`)); err == nil {
		t.Fatal("空候选且无拦截原因时应报错")
	}
}

// TestDecodeStreamFrames 覆盖上游流式解码：文本、推理、用量与终止帧。
func TestDecodeStreamFrames(t *testing.T) {
	adapter := New().NewStream()
	frames := []string{
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"Hel"}]}}],"modelVersion":"gemini-2.5-flash-001"}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"想","thought":true}]}}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"lo"}]}}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":2,"totalTokenCount":6}}`,
	}
	var kinds []domain.ChunkKind
	var text strings.Builder
	var end *domain.Chunk
	for i, frame := range frames {
		chunks, err := adapter.DecodeStreamFrame("", []byte(frame))
		if err != nil {
			t.Fatalf("第 %d 帧解码失败：%v", i, err)
		}
		for _, chunk := range chunks {
			kinds = append(kinds, chunk.Kind)
			if chunk.Kind == domain.ChunkTextDelta {
				text.WriteString(chunk.TextDelta)
			}
			if chunk.Kind == domain.ChunkStreamEnd {
				copied := chunk
				end = &copied
			}
		}
	}
	if text.String() != "Hello" {
		t.Errorf("文本 = %q，期望 Hello", text.String())
	}
	if len(kinds) != 4 { // text + reasoning + text + stream_end
		t.Fatalf("分片序列 = %v", kinds)
	}
	if end == nil {
		t.Fatal("缺少结束分片")
	}
	if end.FinishReason != domain.FinishStop {
		t.Errorf("结束原因 = %q", string(end.FinishReason))
	}
	if end.Usage == nil || end.Usage.InputTokens != 4 || end.Usage.OutputTokens != 2 {
		t.Errorf("结束分片用量 = %+v", end.Usage)
	}
	if end.Model != "gemini-2.5-flash-001" {
		t.Errorf("结束分片模型 = %q", end.Model)
	}
	if extra := adapter.FinishStream(); len(extra) != 0 {
		t.Errorf("EOF 处不应再补收尾分片，实际 %d 个", len(extra))
	}
}

// TestDecodeStreamPartialFunctionCall 覆盖流式工具调用的参数分片：分片到达时不产出，
// 完成时产出一次携带完整参数的分片。
func TestDecodeStreamPartialFunctionCall(t *testing.T) {
	adapter := New().NewStream()
	first := `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"f","partialArgs":[{"jsonPath":"$.city","stringValue":"San"}],"willContinue":true}}]}}]}`
	second := `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"f","partialArgs":[{"jsonPath":"$.city","stringValue":" Jose"}],"willContinue":false}}]}}]}`
	chunks, err := adapter.DecodeStreamFrame("", []byte(first))
	if err != nil {
		t.Fatalf("首片解码失败：%v", err)
	}
	if len(chunks) != 0 {
		t.Fatalf("未完成的工具调用不应产出分片，实际 %+v", chunks)
	}
	chunks, err = adapter.DecodeStreamFrame("", []byte(second))
	if err != nil {
		t.Fatalf("次片解码失败：%v", err)
	}
	if len(chunks) != 1 || chunks[0].Kind != domain.ChunkToolCallDelta {
		t.Fatalf("完成帧应产出一个工具调用分片，实际 %+v", chunks)
	}
	if got := chunks[0].ToolCall.Arguments; got != `{"city":"San Jose"}` {
		t.Errorf("参数 = %q，期望 {\"city\":\"San Jose\"}", got)
	}
}

// TestEncodeRequest 覆盖把内部请求重建为 Gemini 请求体。
func TestEncodeRequest(t *testing.T) {
	req := &domain.Request{
		Protocol:  domain.ProtocolGemini,
		Model:     "gemini-2.5-flash",
		MaxTokens: 100,
		Messages: []domain.Message{
			{Role: domain.RoleSystem, Parts: []domain.Part{{Kind: domain.PartText, Text: "sys"}}},
			{Role: domain.RoleUser, Parts: []domain.Part{{Kind: domain.PartText, Text: "hi"}}},
			{Role: domain.RoleAssistant, Parts: []domain.Part{{
				Kind:     domain.PartToolCall,
				ToolCall: &domain.ToolCall{ID: "call_1", Name: "lookup", Arguments: `{"q":"x"}`},
			}}},
			{Role: domain.RoleTool, Parts: []domain.Part{{
				Kind:       domain.PartToolResult,
				ToolResult: &domain.ToolResult{ToolCallID: "call_1", Content: `{"ok":true}`},
			}}},
		},
		Tools: []domain.ToolSpec{{
			Name:           "lookup",
			Description:    "查询",
			ParametersJSON: `{"type":"object","additionalProperties":false,"properties":{"q":{"type":"string","format":"uuid"}}}`,
		}},
		ToolChoice: domain.ToolChoice{Mode: domain.ToolChoiceTool, Name: "lookup"},
	}
	body, parts, err := New().EncodeRequest(req, domain.RewriteOptions{
		UpstreamModel:   "gemini-2.5-flash-001",
		MaxOutputTokens: outputLimit(50),
	})
	if err != nil {
		t.Fatalf("重建请求失败：%v", err)
	}
	if !parts.Has(domain.RewritePartRequestModel) || !parts.Has(domain.RewritePartRequestOutputLimit) {
		t.Errorf("改写标注 = %v，期望含模型名与输出上限", parts)
	}

	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("重建结果不是合法 JSON：%v", err)
	}
	// systemInstruction 上提，contents 只有 user / model / user，且没有系统提示。
	system, _ := decoded["systemInstruction"].(map[string]any)
	if system == nil {
		t.Fatal("缺少 systemInstruction")
	}
	contents, _ := decoded["contents"].([]any)
	roles := make([]string, 0, len(contents))
	for _, item := range contents {
		content, _ := item.(map[string]any)
		role, _ := content["role"].(string)
		roles = append(roles, role)
	}
	if strings.Join(roles, ",") != "user,model,user" {
		t.Errorf("contents 角色 = %v", roles)
	}
	// 工具结果的 name 由调用 id 映射补出。
	last, _ := contents[len(contents)-1].(map[string]any)
	lastParts, _ := last["parts"].([]any)
	functionResponse, _ := lastParts[0].(map[string]any)["functionResponse"].(map[string]any)
	if functionResponse == nil || functionResponse["name"] != "lookup" {
		t.Errorf("functionResponse = %+v", functionResponse)
	}
	// Schema 被裁剪成 Gemini 能接受的子集。
	tools, _ := decoded["tools"].([]any)
	tool, _ := tools[0].(map[string]any)
	declarations, _ := tool["functionDeclarations"].([]any)
	parameters, _ := declarations[0].(map[string]any)["parameters"].(map[string]any)
	if _, exists := parameters["additionalProperties"]; exists {
		t.Errorf("additionalProperties 应被裁掉：%v", parameters)
	}
	if parameters["type"] != "OBJECT" {
		t.Errorf("Schema 类型 = %v，期望归一化为 OBJECT", parameters["type"])
	}
	// 生成参数与工具选择。
	generation, _ := decoded["generationConfig"].(map[string]any)
	if generation["maxOutputTokens"] != float64(50) {
		t.Errorf("maxOutputTokens = %v，期望被渠道上限 50 钳制", generation["maxOutputTokens"])
	}
	toolConfig, _ := decoded["toolConfig"].(map[string]any)
	calling, _ := toolConfig["functionCallingConfig"].(map[string]any)
	if calling["mode"] != "ANY" {
		t.Errorf("toolConfig.mode = %v", calling["mode"])
	}
	allowed, _ := calling["allowedFunctionNames"].([]any)
	if len(allowed) != 1 || allowed[0] != "lookup" {
		t.Errorf("allowedFunctionNames = %v", allowed)
	}
}

// TestEncodeResponse 覆盖把内部响应编码为 Gemini 响应体。
func TestEncodeResponse(t *testing.T) {
	resp := &domain.Response{
		Model: "gemini-2.5-flash-001",
		Message: domain.Message{Role: domain.RoleAssistant, Parts: []domain.Part{
			{Kind: domain.PartText, Text: "你好"},
			{Kind: domain.PartToolCall, ToolCall: &domain.ToolCall{ID: "call_1", Name: "lookup", Arguments: `{"q":"x"}`}},
		}},
		FinishReason: domain.FinishToolCalls,
		Usage: domain.Usage{
			Source:          domain.UsageSourceUpstream,
			InputTokens:     5,
			OutputTokens:    3,
			ReasoningTokens: 1,
		},
	}
	body, err := New().EncodeResponse(resp)
	if err != nil {
		t.Fatalf("编码响应失败：%v", err)
	}
	var wire wireResponse
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("编码结果不是合法 JSON：%v", err)
	}
	if len(wire.Candidates) != 1 {
		t.Fatalf("候选数 = %d", len(wire.Candidates))
	}
	if wire.Candidates[0].FinishReason == nil || *wire.Candidates[0].FinishReason != "STOP" {
		t.Errorf("结束原因 = %v，期望 STOP", wire.Candidates[0].FinishReason)
	}
	parts := wire.Candidates[0].Content.Parts
	if len(parts) != 2 || parts[0].Text != "你好" || parts[1].FunctionCall == nil {
		t.Fatalf("片段 = %+v", parts)
	}
	if wire.UsageMetadata == nil {
		t.Fatal("缺少 usageMetadata")
	}
	// 线格式的候选计数不含推理，推理单列。
	if wire.UsageMetadata.CandidatesTokenCount != 2 || wire.UsageMetadata.ThoughtsTokenCount != 1 {
		t.Errorf("用量 = %+v", wire.UsageMetadata)
	}
	if wire.UsageMetadata.PromptTokenCount != 5 {
		t.Errorf("promptTokenCount = %d", wire.UsageMetadata.PromptTokenCount)
	}
}

// TestEncodeStream 覆盖流式编码：内容帧与终止帧。
func TestEncodeStream(t *testing.T) {
	adapter := New()
	if _, err := adapter.EncodeStreamStart("gemini-2.5-flash-001"); err != nil {
		t.Fatalf("开始帧失败：%v", err)
	}
	frame, err := adapter.EncodeChunk(domain.Chunk{Kind: domain.ChunkTextDelta, TextDelta: "hi"})
	if err != nil {
		t.Fatalf("编码内容帧失败：%v", err)
	}
	if !strings.HasPrefix(string(frame), "data: ") || !strings.HasSuffix(string(frame), "\n\n") {
		t.Fatalf("帧格式 = %q", frame)
	}
	if !strings.Contains(string(frame), `"text":"hi"`) {
		t.Errorf("帧缺少文本：%s", frame)
	}
	if !strings.Contains(string(frame), `"modelVersion":"gemini-2.5-flash-001"`) {
		t.Errorf("帧缺少模型名：%s", frame)
	}

	usage := domain.Usage{Source: domain.UsageSourceUpstream, InputTokens: 3, OutputTokens: 2}
	end, err := adapter.EncodeStreamEnd(domain.Chunk{
		Kind: domain.ChunkStreamEnd, FinishReason: domain.FinishStop, Usage: &usage,
	})
	if err != nil {
		t.Fatalf("编码终止帧失败：%v", err)
	}
	if !strings.Contains(string(end), `"finishReason":"STOP"`) {
		t.Errorf("终止帧缺少结束原因：%s", end)
	}
	if !strings.Contains(string(end), `"usageMetadata"`) {
		t.Errorf("终止帧缺少用量：%s", end)
	}
	// 结束后不得再编码。
	if _, err := adapter.EncodeStreamEnd(domain.Chunk{Kind: domain.ChunkStreamEnd}); err == nil {
		t.Error("重复编码终止帧应报错")
	}
}

// TestEncodeChunkSkipsReasoning 守护推理分片在编码方向被跳过而不是报错。
func TestEncodeChunkSkipsReasoning(t *testing.T) {
	adapter := New()
	if _, err := adapter.EncodeStreamStart("m"); err != nil {
		t.Fatal(err)
	}
	frame, err := adapter.EncodeChunk(domain.Chunk{Kind: domain.ChunkReasoningDelta, TextDelta: "think"})
	if err != nil {
		t.Fatalf("推理分片不应报错：%v", err)
	}
	if len(frame) != 0 {
		t.Errorf("推理分片不应产出字节，实际 %q", frame)
	}
}

// TestUpstreamURL 覆盖上游地址构造：占位符替换与动作改写。
func TestUpstreamURL(t *testing.T) {
	base := "https://generativelanguage.googleapis.com/v1beta/models/{model}:generateContent"
	cases := []struct {
		name   string
		base   string
		stream bool
		want   string
	}{
		{
			name:   "非流式写 generateContent",
			base:   base,
			stream: false,
			want:   "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent",
		},
		{
			name:   "流式改写动作并加 alt=sse",
			base:   base,
			stream: true,
			want:   "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse",
		},
		{
			name:   "配置写流式动作时非流式请求改回 generateContent",
			base:   "https://example.com/v1beta/models/{model}:streamGenerateContent",
			stream: false,
			want:   "https://example.com/v1beta/models/gemini-2.5-flash:generateContent",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := New().UpstreamURL(domain.Route{
				BaseURL:       tc.base,
				UpstreamModel: "gemini-2.5-flash",
			}, tc.stream)
			if err != nil {
				t.Fatalf("构造上游地址失败：%v", err)
			}
			if got != tc.want {
				t.Errorf("上游地址 = %q，期望 %q", got, tc.want)
			}
		})
	}
}

// TestUpstreamURLRejectsBadTemplate 守护缺少占位符或动作段的地址被显式拒绝。
func TestUpstreamURLRejectsBadTemplate(t *testing.T) {
	cases := []string{
		"https://example.com/v1beta/models/gemini-2.5-flash:generateContent",
		"https://example.com/v1beta/models/{model}:countTokens",
	}
	for _, base := range cases {
		if _, err := New().UpstreamURL(domain.Route{BaseURL: base, UpstreamModel: "m"}, false); err == nil {
			t.Errorf("地址 %q 应被拒绝", base)
		}
	}
}

// TestRewriteRawBody 覆盖同协议透传路径上的输出上限改写（字段嵌在 generationConfig 里）。
func TestRewriteRawBody(t *testing.T) {
	limit := 100
	body := `{"contents":[{"parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":1000,"temperature":0.2}}`
	out, parts, err := New().RewriteRawBody([]byte(body), domain.RewriteOptions{MaxOutputTokens: &limit})
	if err != nil {
		t.Fatalf("改写失败：%v", err)
	}
	if !parts.Has(domain.RewritePartRequestOutputLimit) {
		t.Errorf("改写标注 = %v", parts)
	}
	var decoded struct {
		GenerationConfig map[string]any `json:"generationConfig"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("改写结果不是合法 JSON：%v", err)
	}
	if decoded.GenerationConfig["maxOutputTokens"] != float64(100) {
		t.Errorf("maxOutputTokens = %v", decoded.GenerationConfig["maxOutputTokens"])
	}
	if decoded.GenerationConfig["temperature"] != 0.2 {
		t.Errorf("未涉及的字段应原样保留，temperature = %v", decoded.GenerationConfig["temperature"])
	}

	// 没有 generationConfig 时补齐该对象。
	out, parts, err = New().RewriteRawBody([]byte(`{"contents":[]}`), domain.RewriteOptions{MaxOutputTokens: &limit})
	if err != nil {
		t.Fatalf("补齐失败：%v", err)
	}
	if !parts.Has(domain.RewritePartRequestOutputLimit) {
		t.Errorf("补齐应产生改写标注，实际 %v", parts)
	}
	if !strings.Contains(string(out), `"maxOutputTokens":100`) {
		t.Errorf("补齐结果 = %s", out)
	}

	// 未配置输出上限时逐字节返回。
	out, parts, err = New().RewriteRawBody([]byte(body), domain.RewriteOptions{})
	if err != nil || string(out) != body || len(parts) != 0 {
		t.Errorf("无改写项时应逐字节返回，out=%s parts=%v err=%v", out, parts, err)
	}
}

// TestEncodeError 覆盖错误信封的形状与状态名。
func TestEncodeError(t *testing.T) {
	status, body := New().EncodeError(domain.NewError(domain.CodeInvalidRequest, "参数不对"))
	if status != 400 {
		t.Errorf("状态码 = %d，期望 400", status)
	}
	var decoded wireError
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("错误体不是合法 JSON：%v", err)
	}
	if decoded.Error.Status != "INVALID_ARGUMENT" {
		t.Errorf("status = %q", decoded.Error.Status)
	}
	if decoded.Error.Message != "参数不对" {
		t.Errorf("message = %q", decoded.Error.Message)
	}
}

// TestEncodeStreamErrorUnsupported 守护本协议没有流式错误帧。
func TestEncodeStreamErrorUnsupported(t *testing.T) {
	frame, supported := New().EncodeStreamError(domain.NewError(domain.CodeUpstreamUnavailable, "x"))
	if supported || len(frame) != 0 {
		t.Errorf("Gemini 没有流式错误帧，got frame=%q supported=%v", frame, supported)
	}
}

// TestCleanFunctionSchemaDropsUnsupportedFields 守护 Schema 裁剪只保留白名单字段。
func TestCleanFunctionSchemaDropsUnsupportedFields(t *testing.T) {
	cleaned, err := cleanFunctionSchema(`{
		"type": "object",
		"$schema": "http://json-schema.org/draft-07/schema#",
		"additionalProperties": false,
		"properties": {
			"when": {"type": ["string", "null"], "description": "时间"}
		},
		"required": ["when"]
	}`)
	if err != nil {
		t.Fatalf("裁剪失败：%v", err)
	}
	object, ok := cleaned.(map[string]any)
	if !ok {
		t.Fatalf("裁剪结果类型 = %T", cleaned)
	}
	for _, dropped := range []string{"$schema", "additionalProperties"} {
		if _, exists := object[dropped]; exists {
			t.Errorf("%s 应被裁掉：%v", dropped, object)
		}
	}
	properties, _ := object["properties"].(map[string]any)
	when, _ := properties["when"].(map[string]any)
	if when["type"] != "STRING" || when["nullable"] != true {
		t.Errorf("联合类型应归一化为 STRING + nullable：%v", when)
	}
}
