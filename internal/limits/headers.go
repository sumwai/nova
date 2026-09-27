package limits

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sumwai/nova/internal/profile"
)

// Warn 是一条不阻断流程的解析告警。
//
// ParseHeaders 的契约是「认不出的头忽略、解析失败不回滚」：少数头部写坏时，调用方
// 仍应拿到能用的那部分观测，同时有一条可记录的告警。用 error 承载是为了不新增返回
// 值；调用方用 errors.As(err, &warn) 取出，err 非 nil 不代表 doc 不可用。
type Warn struct {
	Msgs []string
}

// Error 把告警排成一句话。
func (w *Warn) Error() string {
	return "额度响应头告警：" + strings.Join(w.Msgs, "；")
}

// 响应头名。两家的命名不同，OpenAI 把 metric 放在后缀，Anthropic 放在中间。
const (
	headerOpenAIRequestLimit = "x-ratelimit-limit-requests"
	headerOpenAIRequestUsed  = "x-ratelimit-remaining-requests"
	headerOpenAIRequestReset = "x-ratelimit-reset-requests"
	headerOpenAITokenLimit   = "x-ratelimit-limit-tokens"
	headerOpenAITokenUsed    = "x-ratelimit-remaining-tokens"
	headerOpenAITokenReset   = "x-ratelimit-reset-tokens"

	headerAnthropicRequestLimit = "anthropic-ratelimit-requests-limit"
	headerAnthropicRequestUsed  = "anthropic-ratelimit-requests-remaining"
	headerAnthropicRequestReset = "anthropic-ratelimit-requests-reset"
	headerAnthropicTokenLimit   = "anthropic-ratelimit-tokens-limit"
	headerAnthropicTokenUsed    = "anthropic-ratelimit-tokens-remaining"
	headerAnthropicTokenReset   = "anthropic-ratelimit-tokens-reset"

	headerRetryAfter = "retry-after"
)

// ParseHeaders 把响应头解析成一份 rate 类观测文档。
//
// 两条厂商命名都认，认不出的头忽略；某个值解析失败只记一条 Warn，其余头部照常返回。
//
// 调用口径：结果作为观测交给 Table.MergeObserved，用于把「距上次权威观测以来的剩余量」
// 更新到本地状态。设计 §5.4 把请求级响应头列为只用于本请求准入、不写入账号状态；
// 额度层接入后改为合并观测，理由是条目自身的「恶化立即生效、改善需一次实测确认」
// 已经吸收了响应头造成的选路抖动。合并只落在已声明的键上：没有对应声明时
// MergeObserved 会拒绝该键并保留其余，因此它不会凭响应头新建条目。
//
// now 由调用方给出，而不是就地取 time.Now：观测时刻必须与额度表用于新鲜度判定的
// 那个时钟同源，否则注入假时钟的测试里，刚收到的观测会被当成源自未来或早已过期。
func ParseHeaders(h http.Header, now time.Time) (*profile.LimitsDoc, error) {
	now = now.UTC()
	doc := &profile.LimitsDoc{
		Schema:     profile.CurrentSchema,
		Source:     "headers",
		ObservedAt: now.Format(time.RFC3339),
	}
	var warns []string

	add := func(limitHeader, remainingHeader, resetHeader, metric string) {
		limitRaw := strings.TrimSpace(h.Get(limitHeader))
		remainingRaw := strings.TrimSpace(h.Get(remainingHeader))
		resetRaw := strings.TrimSpace(h.Get(resetHeader))
		if limitRaw == "" && remainingRaw == "" && resetRaw == "" {
			return
		}
		entry := profile.Limit{Kind: "rate", Metric: metric, Window: "1m"}
		if limitRaw != "" {
			if value, ok := parseHeaderNumber(limitRaw); ok {
				entry.Limit = floatPtr(value)
			} else {
				warns = append(warns, "响应头 "+limitHeader+" 的值 "+strconv.Quote(limitRaw)+" 无法解析为数字")
			}
		}
		if remainingRaw != "" {
			if value, ok := parseHeaderNumber(remainingRaw); ok {
				entry.Remaining = floatPtr(value)
			} else {
				warns = append(warns, "响应头 "+remainingHeader+" 的值 "+strconv.Quote(remainingRaw)+" 无法解析为数字")
			}
		}
		if resetRaw != "" {
			if at, ok := parseHeaderReset(resetRaw, now); ok {
				entry.ResetsAt = at.Format(time.RFC3339)
			} else {
				warns = append(warns, "响应头 "+resetHeader+" 的值 "+strconv.Quote(resetRaw)+" 无法解析为时长或时刻")
			}
		}
		// 缺少剩余量且给不出 limit 与 used 的条目无法确定剩余量，schema 不接受，
		// 也没法参与求值；跳过它，不构造一个形状非法的文档。
		if entry.Remaining == nil && entry.Limit == nil {
			return
		}
		doc.Account = append(doc.Account, entry)
	}

	add(headerOpenAIRequestLimit, headerOpenAIRequestUsed, headerOpenAIRequestReset, "requests")
	add(headerOpenAITokenLimit, headerOpenAITokenUsed, headerOpenAITokenReset, "tokens")
	add(headerAnthropicRequestLimit, headerAnthropicRequestUsed, headerAnthropicRequestReset, "requests")
	add(headerAnthropicTokenLimit, headerAnthropicTokenUsed, headerAnthropicTokenReset, "tokens")

	if raw := strings.TrimSpace(h.Get(headerRetryAfter)); raw != "" {
		if delay, ok := parseRetryAfter(raw, now); ok {
			applyRetryAfter(doc, now.Add(delay))
		} else {
			warns = append(warns, "响应头 "+headerRetryAfter+" 的值 "+strconv.Quote(raw)+" 无法解析为秒数或 HTTP 日期")
		}
	}

	if len(warns) > 0 {
		return doc, &Warn{Msgs: warns}
	}
	return doc, nil
}

// applyRetryAfter 把 retry-after 写进 requests 的 rate 条目：剩余量归零、重置时刻取退避终点。
//
// 已有 requests rate 条目时覆盖它，避免同一批响应头产出两条同键条目。
func applyRetryAfter(doc *profile.LimitsDoc, until time.Time) {
	for i := range doc.Account {
		entry := &doc.Account[i]
		if entry.Kind == "rate" && entry.Metric == "requests" {
			entry.Remaining = floatPtr(0)
			entry.ResetsAt = until.Format(time.RFC3339)
			return
		}
	}
	doc.Account = append(doc.Account, profile.Limit{
		Kind:      "rate",
		Metric:    "requests",
		Window:    "1m",
		Remaining: floatPtr(0),
		ResetsAt:  until.Format(time.RFC3339),
	})
}

// parseHeaderNumber 把响应头里的数值解析成浮点数。
func parseHeaderNumber(raw string) (float64, bool) {
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}

// parseHeaderReset 解析重置头。
//
// OpenAI 给的是时长（1s、6m0s、1m30s），Anthropic 给的是 RFC3339 时刻；两种都认，
// 也接受裸秒数（部分代理会把它规范化成整数）。返回值一律是绝对时刻。
func parseHeaderReset(raw string, now time.Time) (time.Time, bool) {
	if at, err := time.Parse(time.RFC3339, raw); err == nil {
		return at, true
	}
	if delay, err := time.ParseDuration(raw); err == nil {
		return now.Add(delay), true
	}
	if seconds, err := strconv.ParseFloat(raw, 64); err == nil {
		return now.Add(time.Duration(seconds * float64(time.Second))), true
	}
	return time.Time{}, false
}

// parseRetryAfter 解析 retry-after，接受秒数与 HTTP 日期两种形态。
func parseRetryAfter(raw string, now time.Time) (time.Duration, bool) {
	if seconds, err := strconv.ParseFloat(raw, 64); err == nil {
		if seconds < 0 {
			return 0, false
		}
		return time.Duration(seconds * float64(time.Second)), true
	}
	if at, err := http.ParseTime(raw); err == nil {
		delay := at.Sub(now)
		if delay < 0 {
			delay = 0
		}
		return delay, true
	}
	return 0, false
}
