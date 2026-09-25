package stats

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sumwai/nova/internal/pattern"
)

// 分组轴。顺序即 Breakdowns 里字段的顺序，也是缺省分组顺序。
var groupAxes = []string{"model", "provider", "upstream", "client", "status", "protocol", "error_code"}

// 查询参数名，用于「不认识的参数」报错时给出候选。
var queryParams = []string{
	"client", "agent", "model", "provider", "upstream", "status", "stream",
	"error_code", "since", "until", "group_by", "detail", "limit", "pretty",
}

// 逐请求明细的条数上限与缺省值。
const (
	defaultDetailLimit = 100
	maxDetailLimit     = 1000
)

// Query 是一次统计查询的条件。
//
// 除 group_by 与 detail 相关的几项外，每个字段都是一个可选过滤条件，空值表示不过滤。
// 取值含 `*` / `?` 时按通配匹配（语义见 internal/pattern），否则是精确匹配。
type Query struct {
	Client    string
	Agent     string
	Model     string
	Provider  string
	Upstream  string
	ErrorCode string
	Status    statusFilter
	Stream    *bool
	Since     time.Time
	Until     time.Time

	// GroupBy 是要计算的维度，缺省为 groupAxes 的全部。
	GroupBy []string
	// Detail 为真时附带逐请求明细。
	Detail bool
	// Limit 是明细条数上限，仅在 Detail 为真时生效。
	Limit int
	// Pretty 只影响输出排版，不参与过滤与聚合。
	Pretty bool
}

// statusFilter 是状态码过滤：按类（2xx）或按具体码（200）。
type statusFilter struct {
	set   bool
	class int
	code  int
}

func (f statusFilter) matches(status int) bool {
	switch {
	case !f.set:
		return true
	case f.class != 0:
		return status/100 == f.class
	default:
		return status == f.code
	}
}

// parseQuery 解析并校验查询参数。
//
// 不认识的参数直接报错而不是忽略：一次笔误（modle=gpt-5）在忽略语义下会静默变成
// 「没有过滤」，于是查询结果看起来正常、实际是另一个问题的答案。这与配置层
// 「未知指令报错」是同一个取舍。
func parseQuery(values url.Values, now time.Time) (Query, error) {
	for key := range values {
		if !containsString(queryParams, key) {
			return Query{}, fmt.Errorf("不认识的查询参数 %q；可用的是 %s",
				key, strings.Join(queryParams, ", "))
		}
	}

	q := Query{
		GroupBy: append([]string(nil), groupAxes...),
		Limit:   defaultDetailLimit,
		Until:   now,
	}

	var err error
	if q.Client, err = singleValue(values, "client"); err != nil {
		return Query{}, err
	}
	if q.Agent, err = singleValue(values, "agent"); err != nil {
		return Query{}, err
	}
	if q.Model, err = singleValue(values, "model"); err != nil {
		return Query{}, err
	}
	if q.Provider, err = singleValue(values, "provider"); err != nil {
		return Query{}, err
	}
	if q.Upstream, err = singleValue(values, "upstream"); err != nil {
		return Query{}, err
	}
	if q.ErrorCode, err = singleValue(values, "error_code"); err != nil {
		return Query{}, err
	}

	if raw, ok, err := optionalValue(values, "status"); err != nil {
		return Query{}, err
	} else if ok {
		filter, err := parseStatus(raw)
		if err != nil {
			return Query{}, err
		}
		q.Status = filter
	}

	if raw, ok, err := optionalValue(values, "stream"); err != nil {
		return Query{}, err
	} else if ok {
		value, err := parseBool(raw)
		if err != nil {
			return Query{}, err
		}
		q.Stream = &value
	}

	if raw, ok, err := optionalValue(values, "since"); err != nil {
		return Query{}, err
	} else if ok {
		if q.Since, err = parseTime(raw, now); err != nil {
			return Query{}, fmt.Errorf("since %s", err.Error())
		}
	}
	if raw, ok, err := optionalValue(values, "until"); err != nil {
		return Query{}, err
	} else if ok {
		if q.Until, err = parseTime(raw, now); err != nil {
			return Query{}, fmt.Errorf("until %s", err.Error())
		}
	}
	if !q.Since.IsZero() && q.Since.After(q.Until) {
		return Query{}, fmt.Errorf("since 晚于 until：没有任何记录落在这个区间里")
	}

	if raw, ok, err := optionalValue(values, "group_by"); err != nil {
		return Query{}, err
	} else if ok {
		axes, err := parseGroupBy(raw)
		if err != nil {
			return Query{}, err
		}
		q.GroupBy = axes
	}

	if raw, ok, err := optionalValue(values, "detail"); err != nil {
		return Query{}, err
	} else if ok {
		if q.Detail, err = parseBool(raw); err != nil {
			return Query{}, err
		}
	}

	if raw, ok, err := optionalValue(values, "limit"); err != nil {
		return Query{}, err
	} else if ok {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > maxDetailLimit {
			return Query{}, fmt.Errorf("limit %q 不是 %d 到 %d 之间的整数", raw, 1, maxDetailLimit)
		}
		q.Limit = limit
	}

	if raw, ok, err := optionalValue(values, "pretty"); err != nil {
		return Query{}, err
	} else if ok {
		if q.Pretty, err = parseBool(raw); err != nil {
			return Query{}, err
		}
	}

	return q, nil
}

// matches 报告一条记录是否满足全部过滤条件。
//
// provider 与 upstream 是请求级谓词：「尝试集合里存在命中候选」即保留该请求。
// 于是在 provider 过滤下，by_provider 仍会列出这些请求用过的其它渠道——那正是
// 「这些请求回退过」的信息，不应被过滤抹掉。
func (q Query) matches(req Request) bool {
	if !q.Since.IsZero() && req.Time.Before(q.Since) {
		return false
	}
	if !q.Until.IsZero() && req.Time.After(q.Until) {
		return false
	}
	if q.Client != "" && !pattern.Match(q.Client, req.Client) {
		return false
	}
	if q.Agent != "" && !pattern.Match(q.Agent, req.UserAgent) {
		return false
	}
	if q.Model != "" && !pattern.Match(q.Model, req.Model) {
		return false
	}
	if q.ErrorCode != "" && !pattern.Match(q.ErrorCode, req.ErrorCode) {
		return false
	}
	if !q.Status.matches(req.Status) {
		return false
	}
	if q.Stream != nil && req.Stream != *q.Stream {
		return false
	}
	if q.Provider != "" && !anyMatch(q.Provider, req.Providers) {
		return false
	}
	if q.Upstream != "" && !anyAttemptUpstream(q.Upstream, req.Attempts) {
		return false
	}
	return true
}

// groups 报告本次查询是否要计算某个分组轴。
func (q Query) groups(axis string) bool {
	return containsString(q.GroupBy, axis)
}

func parseStatus(raw string) (statusFilter, error) {
	trimmed := strings.TrimSpace(raw)
	if len(trimmed) == 3 && (trimmed[1] == 'x' || trimmed[1] == 'X') {
		class, err := strconv.Atoi(trimmed[:1])
		if err != nil || class < 1 || class > 5 {
			return statusFilter{}, fmt.Errorf("status %q 不是合法的状态类；形如 2xx", raw)
		}
		return statusFilter{set: true, class: class}, nil
	}
	code, err := strconv.Atoi(trimmed)
	if err != nil || code < 100 || code > 599 {
		return statusFilter{}, fmt.Errorf("status %q 既不是状态类（形如 2xx）也不是 100 到 599 的整数", raw)
	}
	return statusFilter{set: true, code: code}, nil
}

func parseGroupBy(raw string) ([]string, error) {
	var axes []string
	seen := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		axis := strings.TrimSpace(part)
		if axis == "" {
			continue
		}
		if !containsString(groupAxes, axis) {
			return nil, fmt.Errorf("不认识的分组维度 %q；可用的是 %s", axis, strings.Join(groupAxes, ", "))
		}
		if seen[axis] {
			continue
		}
		seen[axis] = true
		axes = append(axes, axis)
	}
	return axes, nil
}

// parseTime 解析时间取值：RFC3339，或以 `-` 开头的相对时长（相对 now）。
func parseTime(raw string, now time.Time) (time.Time, error) {
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, "-") {
		offset, err := time.ParseDuration(trimmed)
		if err != nil {
			return time.Time{}, fmt.Errorf("%q 不是合法的相对时长（形如 -10m、-24h）", raw)
		}
		return now.Add(offset), nil
	}
	parsed, err := time.Parse(time.RFC3339, trimmed)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q 不是 RFC3339 时刻，也不是相对时长（形如 -10m、-24h）", raw)
	}
	return parsed, nil
}

func parseBool(raw string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "1":
		return true, nil
	case "false", "0":
		return false, nil
	}
	return false, fmt.Errorf("%q 不是布尔取值（true / false）", raw)
}

// optionalValue 取一个可选参数：缺失或为空串时返回 ok=false。
func optionalValue(values url.Values, key string) (string, bool, error) {
	vals := values[key]
	if len(vals) == 0 {
		return "", false, nil
	}
	if len(vals) > 1 {
		return "", false, fmt.Errorf("%s 只接受一个取值", key)
	}
	if strings.TrimSpace(vals[0]) == "" {
		return "", false, nil
	}
	return vals[0], true, nil
}

func singleValue(values url.Values, key string) (string, error) {
	value, ok, err := optionalValue(values, key)
	if err != nil || !ok {
		return "", err
	}
	return value, nil
}

func anyMatch(pat string, values []string) bool {
	for _, value := range values {
		if pattern.Match(pat, value) {
			return true
		}
	}
	return false
}

func anyAttemptUpstream(pat string, attempts []AttemptSummary) bool {
	for _, attempt := range attempts {
		if pattern.Match(pat, attempt.Upstream) {
			return true
		}
	}
	return false
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
