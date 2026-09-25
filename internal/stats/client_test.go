package stats

import (
	"strings"
	"testing"
)

func TestNormalizeClientTakesProductSegment(t *testing.T) {
	tests := []struct {
		name      string
		userAgent string
		want      string
	}{
		{name: "带版本段", userAgent: "claude-cli/1.0.3 (external, cli)", want: "claude-cli"},
		{name: "python 客户端", userAgent: "python-requests/2.31.0", want: "python-requests"},
		{name: "curl", userAgent: "curl/8.5.0", want: "curl"},
		{name: "空格分隔", userAgent: "Some Client 1.0", want: "some"},
		{name: "括号开头无斜杠", userAgent: "WeirdClient (compatible)", want: "weirdclient"},
		{name: "浏览器", userAgent: "Mozilla/5.0 (X11; Linux x86_64)", want: "mozilla"},
		{name: "空串", userAgent: "", want: unknownClient},
		{name: "只有空白", userAgent: "   ", want: unknownClient},
		{name: "以斜杠开头", userAgent: "/1.0", want: unknownClient},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeClient(tt.userAgent); got != tt.want {
				t.Errorf("NormalizeClient(%q) = %q，期望 %q", tt.userAgent, got, tt.want)
			}
		})
	}
}

// 产品段异常长时要截断：它不该成为一条把输出撑大的聚合键。
func TestNormalizeClientTruncatesLongName(t *testing.T) {
	got := NormalizeClient(strings.Repeat("a", maxClientRunes+20) + "/1.0")
	if len([]rune(got)) != maxClientRunes {
		t.Errorf("截断后长度 = %d，期望 %d", len([]rune(got)), maxClientRunes)
	}
}
