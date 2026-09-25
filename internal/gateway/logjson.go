package gateway

import (
	"log/slog"
	"strings"
	"time"

	"github.com/sumwai/nova/internal/config"
	"github.com/sumwai/nova/internal/domain"
	"github.com/sumwai/nova/internal/transport"
)

// 本文件是 log_format json 的载荷形状。
//
// 与 text 模式相反，这里不做任何省略：字段该有就有，即使是零值与空串（结构体上标了
// omitempty 的除外——那些字段「不存在」与「是空串」对下游是同一件事，省掉更省字节）。
// 下游按字段索引，因此这里的每个键一旦发布就是契约，改名要当成不兼容变更对待。
//
// text 模式里的块组装、单次成功尝试折叠与重复失败压缩都不出现在这里：json 下每个请求
// 产出 access 与 upstream_attempt 两条独立记录，由下游按 request_id 关联。

// jsonBase 是 json 模式每条记录的公共前缀。
type jsonBase struct {
	Time  string `json:"time"`
	Level string `json:"level"`
	Msg   string `json:"msg"`
}

// levelTag 是级别在 json 模式下的取值：去掉人读标签用于对齐的尾部空格，
// 让下游可以直接按值过滤，不必先 trim。
func levelTag(level slog.Level) string {
	return strings.TrimSpace(levelName(level))
}

func jsonBaseFor(level slog.Level, msg string) jsonBase {
	return jsonBase{
		Time:  time.Now().Format(time.RFC3339),
		Level: levelTag(level),
		Msg:   msg,
	}
}

type jsonStartup struct {
	jsonBase
	Config             string `json:"config"`
	Schema             int    `json:"schema"`
	Listen             string `json:"listen"`
	Admin              string `json:"admin"`
	LogLevel           string `json:"log_level"`
	LogFormat          string `json:"log_format"`
	Providers          int    `json:"providers"`
	Models             int    `json:"models"`
	DiscoveryEndpoints int    `json:"discovery_endpoints"`
	DiscoveredModels   int    `json:"discovered_models"`
	FilteredModels     int    `json:"filtered_models"`
	DiscoveryDegraded  int    `json:"discovery_degraded"`
	ClientAuth         bool   `json:"client_auth"`
	ClientKeys         int    `json:"client_keys"`
	Reloaded           bool   `json:"reloaded"`
}

func startupPayload(cfg *config.Config, stats catalogStats, reloaded bool) any {
	msg := "startup"
	if reloaded {
		msg = "reload"
	}
	return jsonStartup{
		jsonBase:           jsonBaseFor(slog.LevelInfo, msg),
		Config:             cfg.Path,
		Schema:             cfg.Schema,
		Listen:             cfg.Listen,
		Admin:              cfg.Admin,
		LogLevel:           cfg.LogLevel,
		LogFormat:          cfg.LogFormat,
		Providers:          len(cfg.Providers),
		Models:             stats.Models,
		DiscoveryEndpoints: stats.DiscoveryEndpoints,
		DiscoveredModels:   stats.Discovered,
		FilteredModels:     stats.Filtered,
		DiscoveryDegraded:  stats.Degraded,
		ClientAuth:         len(cfg.ClientKeys) > 0,
		ClientKeys:         len(cfg.ClientKeys),
		Reloaded:           reloaded,
	}
}

// jsonDiscovery 是一条端点发现记录。
//
// 字段齐全、不做省略：下游要能用它回答「这条端点这次发现了什么、为什么没成」。
type jsonDiscovery struct {
	jsonBase
	Provider string `json:"provider"`
	Listing  string `json:"listing"`
	Protocol string `json:"protocol"`
	Shape    string `json:"shape,omitempty"`
	Found    int    `json:"found"`
	Kept     int    `json:"kept"`
	Filtered int    `json:"filtered"`
	// Models 是清单里保留的对外名（`expose` 改名后的结果），不截断：
	// 这边是给机器读的，采集侧要能用它做「模型集合变了」这类判断。
	Models []string `json:"models,omitempty"`
	// Declared 是这条端点显式声明的对外名。它与 Models 合起来才是客户端实际
	// 可用的集合：只列清单那一侧会让配下的 model 行看起来没生效。
	Declared   []string `json:"declared,omitempty"`
	HasMore    bool     `json:"has_more"`
	Degraded   bool     `json:"degraded"`
	DurationMS int64    `json:"duration_ms"`
	Error      string   `json:"error,omitempty"`
}

func discoveryPayload(rec DiscoveryReport) any {
	payload := jsonDiscovery{
		jsonBase:   jsonBaseFor(discoveryLevel(rec), "model_discovery"),
		Provider:   rec.Provider,
		Listing:    rec.Listing,
		Protocol:   string(rec.Protocol),
		Shape:      string(rec.Shape),
		Found:      rec.Found,
		Kept:       len(rec.Kept),
		Filtered:   rec.Filtered,
		HasMore:    rec.HasMore,
		Degraded:   rec.Degraded,
		DurationMS: rec.Duration.Milliseconds(),
	}
	if rec.Err != nil {
		payload.Error = rec.Err.Error()
	}
	for _, model := range rec.Kept {
		payload.Models = append(payload.Models, model.Name)
	}
	for _, model := range rec.Declared {
		payload.Declared = append(payload.Declared, model.Name)
	}
	return payload
}

type jsonWarning struct {
	jsonBase
	File    string `json:"file"`
	Line    int    `json:"line,omitempty"`
	Message string `json:"message"`
}

func warningPayload(warn config.Warning) any {
	return jsonWarning{
		jsonBase: jsonBaseFor(slog.LevelWarn, "warning"),
		File:     warn.File,
		Line:     warn.Line,
		Message:  warn.Msg,
	}
}

type jsonReload struct {
	jsonBase
	Config string `json:"config"`
}

func reloadPayload(path string) any {
	return jsonReload{
		jsonBase: jsonBaseFor(slog.LevelInfo, "reload_request"),
		Config:   path,
	}
}

type jsonFailure struct {
	jsonBase
	Message string `json:"message"`
	Error   string `json:"error,omitempty"`
}

func failurePayload(message string, err error) any {
	payload := jsonFailure{
		jsonBase: jsonBaseFor(slog.LevelWarn, "warning"),
		Message:  message,
	}
	if err != nil {
		payload.Error = err.Error()
	}
	return payload
}

type jsonAccess struct {
	jsonBase
	RequestID  string `json:"request_id,omitempty"`
	Protocol   string `json:"client_protocol,omitempty"`
	Model      string `json:"model,omitempty"`
	Stream     bool   `json:"stream"`
	HTTPStatus int    `json:"http_status"`
	DurationMS int64  `json:"duration_ms"`
	Bytes      int    `json:"written_bytes"`
	ErrorCode  string `json:"error_code,omitempty"`
	RemoteAddr string `json:"remote_addr,omitempty"`
	UserAgent  string `json:"user_agent,omitempty"`
}

func accessPayload(record transport.AccessRecord) any {
	return jsonAccess{
		jsonBase:   jsonBaseFor(accessLevel(record.HTTPStatus), "access"),
		RequestID:  record.RequestID,
		Protocol:   string(record.Protocol),
		Model:      record.Model,
		Stream:     record.Stream,
		HTTPStatus: record.HTTPStatus,
		DurationMS: record.DurationMS,
		Bytes:      record.WrittenBytes,
		ErrorCode:  record.ErrorCode,
		RemoteAddr: record.RemoteAddr,
		UserAgent:  record.UserAgent,
	}
}

// jsonUsage 是用量在 json 里的分组。
//
// 分组而不是平铺，是因为这五项都是「本次调用的计数」，平铺进顶层后会与
// request_id、http_status 这类请求级事实混在同一层，下游按前缀取用时容易漏掉一个。
type jsonUsage struct {
	Source           string `json:"source,omitempty"`
	InputTokens      int    `json:"input_tokens"`
	OutputTokens     int    `json:"output_tokens"`
	CacheReadTokens  int    `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int    `json:"cache_write_tokens,omitempty"`
	ReasoningTokens  int    `json:"reasoning_tokens,omitempty"`
	ServerToolUses   int    `json:"server_tool_uses,omitempty"`
}

// jsonAttempt 是一条上游尝试记录。
//
// provider 与 upstream 并存：后者是「渠道名 + 主机名」的展示串，不保证单射，
// 不得由它反推渠道；前者未经拼接，可按值分组。
type jsonAttempt struct {
	jsonBase
	RequestID        string     `json:"request_id,omitempty"`
	Attempt          int        `json:"attempt"`
	Upstream         string     `json:"upstream"`
	Provider         string     `json:"provider"`
	ClientProtocol   string     `json:"client_protocol"`
	UpstreamProtocol string     `json:"upstream_protocol"`
	ProtocolSwitched bool       `json:"protocol_switched"`
	Model            string     `json:"model"`
	UpstreamModel    string     `json:"upstream_model"`
	Account          string     `json:"account,omitempty"`
	Outcome          string     `json:"outcome"`
	DurationMS       int64      `json:"duration_ms"`
	ErrorCode        string     `json:"error_code,omitempty"`
	ErrorDetail      string     `json:"error_detail,omitempty"`
	Usage            *jsonUsage `json:"usage,omitempty"`
	RewrittenParts   []string   `json:"rewritten_parts,omitempty"`
}

func attemptPayload(rec domain.AttemptRecord) any {
	payload := jsonAttempt{
		jsonBase:         jsonBaseFor(attemptLevel(rec.Outcome), "upstream_attempt"),
		RequestID:        rec.RequestID,
		Attempt:          rec.Attempt,
		Upstream:         rec.UpstreamID,
		Provider:         rec.Provider,
		ClientProtocol:   string(rec.ClientProtocol),
		UpstreamProtocol: string(rec.UpstreamProtocol),
		ProtocolSwitched: rec.ClientProtocol != rec.UpstreamProtocol,
		Model:            rec.RequestedModel,
		UpstreamModel:    rec.UpstreamModel,
		Account:          rec.AccountRef,
		Outcome:          string(rec.Outcome),
		DurationMS:       rec.EndedAt.Sub(rec.StartedAt).Milliseconds(),
		ErrorCode:        rec.ErrorCode,
		ErrorDetail:      rec.ErrorDetail,
	}
	// 用量未知时整个 usage 分组不出现：一个全零的 usage 对象看起来像「这次没花钱」，
	// 与「上游没有给出用量」是两件相反的事（见 domain.Usage.Known）。
	if rec.Usage.Known() {
		payload.Usage = &jsonUsage{
			Source:           string(rec.Usage.Source),
			InputTokens:      rec.Usage.InputTokens,
			OutputTokens:     rec.Usage.OutputTokens,
			CacheReadTokens:  rec.Usage.CacheReadTokens,
			CacheWriteTokens: rec.Usage.CacheWriteTokens,
			ReasoningTokens:  rec.Usage.ReasoningTokens,
			ServerToolUses:   rec.Usage.ServerToolUses,
		}
	}
	for _, part := range rec.RewrittenParts {
		payload.RewrittenParts = append(payload.RewrittenParts, string(part))
	}
	return payload
}
