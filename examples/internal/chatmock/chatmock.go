// Package chatmock 是示例使用的模拟上游：四种上游线协议各有一个端点，响应确定、可被断言。
//
// 它在示例里承担两件事：
//
//   - 每个例子都能离线跑：克隆仓库、起这份模拟上游、起 nova、发一次请求，不碰任何真实凭据；
//   - 断言有确定的事实可对：正文回显本次收到的上游模型名，用量固定为 11 / 7，
//     并记录每一次收到的请求（协议、路径、上游模型名、凭据、是否流式）。
//     于是「回退到了哪条渠道」「用了哪条账号」「地址末段的动作换没换」都能直接读出来，
//     不必从响应正文里反推。
//
// 故障由凭据后缀注入（见 Fault* 常量）：同一份模拟上游在同一次运行里既能回成功，
// 又能回限流、故障与超时。账号池与渠道回退的演示需要的正是「同一个端点下不同凭据表现不同」，
// 因此这不是它的缺陷，而是它在这批示例里的主要用途。
//
// 本包只依赖标准库，也不属于 nova 本身：只被 examples/ 下的示例与示例校验导入。
package chatmock

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Fault* 是凭据末尾可注入的故障开关。
//
// 四个取值与 internal/upstream/client.go 的状态码分档一一对应，因此覆盖了
// 「可重试」与「不可重试」两类：前三个让 nova 换下一个候选，FaultRejected 不换。
const (
	// FaultRateLimited 回 429。nova 归为上游限流，可重试（换账号或换渠道）。
	FaultRateLimited = "429"
	// FaultUnavailable 回 503。nova 归为上游不可用，可重试。
	FaultUnavailable = "5xx"
	// FaultSlow 保持连接不回应，由端点的 timeout 触发上游超时，可重试。
	FaultSlow = "slow"
	// FaultRejected 回 400。nova 归为上游拒绝请求，不可重试，不会换候选。
	FaultRejected = "bad"
)

// faultSuffixes 是故障开关的匹配顺序。四个取值互不为后缀，顺序因此不影响结果。
var faultSuffixes = []string{FaultRateLimited, FaultUnavailable, FaultSlow, FaultRejected}

// slowFaultHold 是 FaultSlow 下模拟上游保持连接而不回应的最长时间。
//
// 它只用于兜底：正常路径上请求方（nova）会先在端点的 timeout 处取消，
// 这里随之返回，因此不会真等到这个上限。
//
// 端点 timeout 是「最长无进展时间」，因此只要它小于这个上限，就能稳定观测到上游超时：
// examples/route-fallback 的 relay-slow 写成 200ms。
const slowFaultHold = 30 * time.Second

// 模拟上游回的固定用量。取两个互不相等的奇数，使「输入输出被写反」也能被断言发现。
const (
	InputTokens  = 11
	OutputTokens = 7
)

// 协议名。取值与 internal/domain 的协议字面量一致，断言失败时的输出因此可直接对照日志。
const (
	ProtocolOpenAIChat        = "openai_chat"
	ProtocolOpenAIResponses   = "openai_responses"
	ProtocolAnthropicMessages = "anthropic_messages"
	ProtocolGemini            = "gemini"
)

// Request 是一次收到的上游请求。
type Request struct {
	// Protocol 是按路径判出的上游线协议。
	Protocol string
	// Path 是请求路径与查询串，用来断言地址改写（动作段、alt=sse）确实发生了。
	Path string
	// Model 是上游收到的模型名；Gemini 的模型名只在路径里，因此从路径取出。
	Model string
	// Stream 报告本次上游调用是不是流式。
	Stream bool
	// Key 是本次请求携带的凭据原值，用来断言用的是哪条账号或哪条渠道。
	Key string
	// CredentialHeader 是承载凭据的请求头名：nova 按上游协议决定注入 Authorization、
	// x-api-key 还是 x-goog-api-key，这一项能直接证明用的是哪一种。
	CredentialHeader string
	// Fault 是凭据注入的故障开关，无故障时为空串。
	Fault string
}

// Mock 是模拟上游本身：可执行程序与示例校验共用同一份处理器。
type Mock struct {
	mu       sync.Mutex
	requests []Request
	models   []string
}

// DefaultModels 是清单端点的缺省模型集合。
//
// 五个 id 刻意覆盖四类：会被 allow 保留的、会被 deny 排除的、会被 expose 改名的，
// 以及既不被 allow 命中也不带后缀的。命令行的缺省值与示例校验共用这一份：
// 分成两份就会漂出「手工跑时清单有五条、校验跑时清单是空的」这类差异。
var DefaultModels = []string{"gpt-5", "gpt-5-mini", "gpt-4o-preview", "claude-sonnet-4", "llama-3"}

// New 造一份模拟上游。models 是清单端点返回的模型 id，顺序原样保留；nil 时取 DefaultModels。
func New(models []string) *Mock {
	if models == nil {
		models = DefaultModels
	}
	return &Mock{models: append([]string(nil), models...)}
}

// Requests 返回截至目前收到的全部请求的副本。
func (m *Mock) Requests() []Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Request(nil), m.requests...)
}

// Reset 清空已记录请求，供一个例子里的多组断言分开计数。
func (m *Mock) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = nil
}

// ServeHTTP 按路径末段分派到四种上游协议、清单端点与 Gemini 的动作路径。
//
// 只按末段判定、不写死版本根：清单地址的推导规则是「裁掉协议端点段再拼 /models」，
// 版本根原样保留，模拟上游因此也要能在 /v1 之外的版本根下应答（示例里用到 /api/v4）。
func (m *Mock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case strings.HasSuffix(path, "/chat/completions"):
		m.serveOpenAIChat(w, r)
	case strings.HasSuffix(path, "/responses"):
		m.serveOpenAIResponses(w, r)
	case strings.HasSuffix(path, "/messages"):
		m.serveAnthropic(w, r)
	case isGeminiAction(path):
		m.serveGemini(w, r)
	case strings.HasSuffix(path, "/models"):
		m.serveListing(w, r)
	default:
		http.Error(w, "chatmock: 不认识的路径 "+path, http.StatusNotFound)
	}
}

// isGeminiAction 报告路径末段是不是 Gemini 的两个动作之一。
func isGeminiAction(path string) bool {
	return strings.HasSuffix(path, ":generateContent") || strings.HasSuffix(path, ":streamGenerateContent")
}

func (m *Mock) serveOpenAIChat(w http.ResponseWriter, r *http.Request) {
	var wire struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	decodeBody(r, &wire)
	header, key := credential(r)
	fault := m.record(Request{
		Protocol:         ProtocolOpenAIChat,
		Path:             r.URL.RequestURI(),
		Model:            wire.Model,
		Stream:           wire.Stream,
		Key:              key,
		CredentialHeader: header,
	})
	if respondFault(w, r, fault) {
		return
	}
	if wire.Stream {
		writeOpenAIChatStream(w, wire.Model)
		return
	}
	writeJSON(w, openAIChatResponse(wire.Model))
}

func (m *Mock) serveOpenAIResponses(w http.ResponseWriter, r *http.Request) {
	var wire struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	decodeBody(r, &wire)
	header, key := credential(r)
	fault := m.record(Request{
		Protocol:         ProtocolOpenAIResponses,
		Path:             r.URL.RequestURI(),
		Model:            wire.Model,
		Stream:           wire.Stream,
		Key:              key,
		CredentialHeader: header,
	})
	if respondFault(w, r, fault) {
		return
	}
	if wire.Stream {
		writeOpenAIResponsesStream(w, wire.Model)
		return
	}
	writeJSON(w, openAIResponsesResponse(wire.Model))
}

func (m *Mock) serveAnthropic(w http.ResponseWriter, r *http.Request) {
	var wire struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	decodeBody(r, &wire)
	header, key := credential(r)
	fault := m.record(Request{
		Protocol:         ProtocolAnthropicMessages,
		Path:             r.URL.RequestURI(),
		Model:            wire.Model,
		Stream:           wire.Stream,
		Key:              key,
		CredentialHeader: header,
	})
	if respondFault(w, r, fault) {
		return
	}
	if wire.Stream {
		writeAnthropicStream(w, wire.Model)
		return
	}
	writeJSON(w, anthropicResponse(wire.Model))
}

// serveGemini 处理 generateContent 与 streamGenerateContent 两条路径。
//
// 模型名与是否流式都只在路径里，请求体里没有这两项，因此这里从路径上取。
func (m *Mock) serveGemini(w http.ResponseWriter, r *http.Request) {
	model, stream := geminiPath(r.URL.Path)
	header, key := credential(r)
	fault := m.record(Request{
		Protocol:         ProtocolGemini,
		Path:             r.URL.RequestURI(),
		Model:            model,
		Stream:           stream,
		Key:              key,
		CredentialHeader: header,
	})
	if respondFault(w, r, fault) {
		return
	}
	if stream {
		writeGeminiStream(w, model)
		return
	}
	writeJSON(w, geminiResponse(model))
}

// geminiPath 从动作路径里取出模型名与是否流式。
//
// 路径形如 <版本根>/models/<模型>:generateContent；模型名允许含 `/`（发布者形态），
// 因此取 `/models/` 与最后一个 `:` 之间的整段。
func geminiPath(path string) (model string, stream bool) {
	action := ":generateContent"
	if strings.HasSuffix(path, ":streamGenerateContent") {
		action = ":streamGenerateContent"
		stream = true
	}
	trimmed := strings.TrimSuffix(path, action)
	if index := strings.Index(trimmed, "/models/"); index >= 0 {
		model = trimmed[index+len("/models/"):]
	}
	return model, stream
}

// record 登记一次请求，并回填由凭据推出的故障开关。
func (m *Mock) record(req Request) string {
	req.Fault = faultOf(req.Key)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = append(m.requests, req)
	return req.Fault
}

// faultOf 取凭据末尾的故障开关；没有开关时返回空串。
func faultOf(key string) string {
	for _, fault := range faultSuffixes {
		if strings.HasSuffix(key, "-"+fault) {
			return fault
		}
	}
	return ""
}

// respondFault 按故障开关回应，第二个返回值报告本次请求是否已被故障处理。
//
// 错误体的字段刻意保持中性（type 为 upstream_error）：nova 的非重试与否由 HTTP 状态码
// 判定，而模拟上游给出的 type 若恰好命中 domain.NonRetryableUpstreamErrorType，// 就会让「状态码决定可重试性」这条断言在流式路径上得出另一种结果。
func respondFault(w http.ResponseWriter, r *http.Request, fault string) bool {
	var status int
	switch fault {
	case FaultRateLimited:
		status = http.StatusTooManyRequests
	case FaultUnavailable:
		status = http.StatusServiceUnavailable
	case FaultRejected:
		status = http.StatusBadRequest
	case FaultSlow:
		// 不回应，直到请求方放弃或等够上限。等仍能给响应时回 504，
		// 而不是让 http 包替我们写一个空 200——空 200 在日志里看起来像一次成功。
		select {
		case <-r.Context().Done():
		case <-time.After(slowFaultHold):
		}
		status = http.StatusGatewayTimeout
	default:
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody(fault))
	return true
}

// errorBody 是故障响应的错误体，形状取 OpenAI 系的最小形态。
//
// 四种上游协议的错误体形状不同，但 nova 的 HTTP 状态分档只看状态码，
// 因此这里给一份最小的公共形状，避免为四种协议各写一份用不上的错误编码。
func errorBody(fault string) map[string]any {
	return map[string]any{
		"error": map[string]any{
			"message": "chatmock 注入的故障：" + fault,
			"type":    "upstream_error",
		},
	}
}

// serveListing 按请求头区分清单形状：带 anthropic-version 的按 Anthropic 形状回答。
//
// 与 nova 自己的 GET /v1/models 同一口径，因此上游清单的两种形状都能在离线状态下演示。
func (m *Mock) serveListing(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	models := append([]string(nil), m.models...)
	m.mu.Unlock()

	items := make([]map[string]any, 0, len(models))
	anthropicShape := r.Header.Get("anthropic-version") != ""
	for _, model := range models {
		if anthropicShape {
			items = append(items, map[string]any{
				"id": model, "type": "model", "display_name": model,
				"created_at": "2026-01-01T00:00:00Z",
			})
			continue
		}
		items = append(items, map[string]any{
			"id": model, "object": "model", "owned_by": "chatmock", "created": 0,
		})
	}
	body := map[string]any{"data": items, "has_more": false}
	if anthropicShape {
		body["type"] = "list"
	} else {
		body["object"] = "list"
	}
	writeJSON(w, body)
}

// credential 取出本次请求携带的凭据，返回承载它的头名与原值。
//
// 三种头都要看：nova 按上游协议决定注入哪一种（Gemini 用 x-goog-api-key，
// Anthropic 用 x-api-key，OpenAI 系用 Authorization）。
func credential(r *http.Request) (header, key string) {
	if value := r.Header.Get("Authorization"); strings.HasPrefix(value, "Bearer ") {
		return "Authorization", strings.TrimPrefix(value, "Bearer ")
	}
	if value := r.Header.Get("x-api-key"); value != "" {
		return "x-api-key", value
	}
	return "x-goog-api-key", r.Header.Get("x-goog-api-key")
}

// decodeBody 尽力解析请求体。它只读自己需要的两个字段，解析失败按字段缺失处理：
// 它的职责是「回一份可断言的成功响应」，不是校验 nova 发来的报文是否规范。
func decodeBody(r *http.Request, target any) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return
	}
	_ = json.Unmarshal(body, target)
}

func writeJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(payload)
}
