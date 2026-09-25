package stats

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

func testNow() time.Time {
	return time.Date(2026, 9, 26, 4, 0, 0, 0, time.UTC)
}

func TestParseQueryDefaults(t *testing.T) {
	query, err := parseQuery(url.Values{}, testNow())
	if err != nil {
		t.Fatalf("parseQuery 意外失败：%v", err)
	}
	if len(query.GroupBy) != len(groupAxes) {
		t.Errorf("缺省分组轴 = %v，期望全部轴", query.GroupBy)
	}
	if query.Limit != defaultDetailLimit {
		t.Errorf("缺省明细上限 = %d，期望 %d", query.Limit, defaultDetailLimit)
	}
	if !query.Until.Equal(testNow()) {
		t.Errorf("缺省 until = %v，期望 now", query.Until)
	}
}

func TestParseQueryRejectsBadInput(t *testing.T) {
	tests := []struct {
		name   string
		values url.Values
		want   string
	}{
		{name: "未知参数", values: url.Values{"modle": {"gpt-5"}}, want: "不认识的查询参数"},
		{name: "重复取值", values: url.Values{"model": {"a", "b"}}, want: "只接受一个取值"},
		{name: "状态非法", values: url.Values{"status": {"abc"}}, want: "status"},
		{name: "布尔非法", values: url.Values{"stream": {"maybe"}}, want: "布尔"},
		{name: "时间非法", values: url.Values{"since": {"昨天"}}, want: "since"},
		{name: "区间反了", values: url.Values{"since": {"2026-09-26T05:00:00Z"}}, want: "晚于 until"},
		{name: "分组轴非法", values: url.Values{"group_by": {"model,nope"}}, want: "不认识的分组维度"},
		{name: "明细上限越界", values: url.Values{"limit": {"0"}}, want: "limit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseQuery(tt.values, testNow())
			if err == nil {
				t.Fatalf("期望报错，实际通过")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("错误 = %q，期望含 %q", err.Error(), tt.want)
			}
		})
	}
}

// 相对时长按 now 换算；RFC3339 原样解析。
func TestParseQueryAcceptsRelativeAndAbsoluteTimes(t *testing.T) {
	query, err := parseQuery(url.Values{"since": {"-10m"}}, testNow())
	if err != nil {
		t.Fatalf("parseQuery 意外失败：%v", err)
	}
	if want := testNow().Add(-10 * time.Minute); !query.Since.Equal(want) {
		t.Errorf("since = %v，期望 %v", query.Since, want)
	}

	query, err = parseQuery(url.Values{"until": {"2026-09-26T03:00:00Z"}}, testNow())
	if err != nil {
		t.Fatalf("parseQuery 意外失败：%v", err)
	}
	if want := time.Date(2026, 9, 26, 3, 0, 0, 0, time.UTC); !query.Until.Equal(want) {
		t.Errorf("until = %v，期望 %v", query.Until, want)
	}
}

func TestQueryMatchesFilters(t *testing.T) {
	record := Request{
		Time:      testNow().Add(-time.Minute),
		Client:    "claude-cli",
		UserAgent: "claude-cli/1.0.3 (external, cli)",
		Model:     "gpt-5",
		Status:    200,
		Stream:    true,
		Providers: []string{"relay-a", "relay-b"},
		Attempts: []AttemptSummary{
			{Provider: "relay-a", Upstream: "relay-a api.example.com"},
		},
	}

	tests := []struct {
		name   string
		values string
		want   bool
	}{
		{name: "客户端 glob 命中", values: "client=claude-*", want: true},
		{name: "客户端不符", values: "client=curl", want: false},
		{name: "UA 通配命中", values: "agent=*external*", want: true},
		{name: "UA 不符", values: "agent=curl*", want: false},
		{name: "模型命中", values: "model=gpt-*", want: true},
		{name: "渠道是请求级谓词", values: "provider=relay-b", want: true},
		{name: "渠道未用过", values: "provider=relay-z", want: false},
		{name: "上游命中", values: "upstream=relay-a*", want: true},
		{name: "状态类命中", values: "status=2xx", want: true},
		{name: "状态类不符", values: "status=5xx", want: false},
		{name: "流式命中", values: "stream=true", want: true},
		{name: "非流式不符", values: "stream=false", want: false},
		{name: "时间窗外的记录被排除", values: "since=-1s", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values, err := url.ParseQuery(tt.values)
			if err != nil {
				t.Fatalf("构造参数失败：%v", err)
			}
			query, err := parseQuery(values, testNow())
			if err != nil {
				t.Fatalf("parseQuery 意外失败：%v", err)
			}
			if got := query.matches(record); got != tt.want {
				t.Errorf("matches = %v，期望 %v", got, tt.want)
			}
		})
	}
}
