package catalog

import "testing"

func TestMatchIsWholeIDMatching(t *testing.T) {
	tests := []struct {
		pattern string
		id      string
		want    bool
	}{
		// 整串匹配、两端隐式锚定：gpt* 是「以 gpt 开头」，不是「包含 gpt」。
		{pattern: "gpt*", id: "gpt-4", want: true},
		{pattern: "gpt*", id: "gpt-5-mini", want: true},
		{pattern: "gpt*", id: "openai/gpt-4o", want: false},
		{pattern: "gpt*", id: "my-gpt-4", want: false},
		{pattern: "openai/gpt-*", id: "openai/gpt-4o", want: true},
		{pattern: "openai/gpt-*", id: "gpt-4o", want: false},
		{pattern: "*gpt*", id: "openai/gpt-4o", want: true},
		{pattern: "*gpt*", id: "my-gpt-4", want: true},

		// 星号跨斜杠：模型 id 里的斜杠是名字的一部分，不是路径分隔符。
		{pattern: "*v3p1*", id: "accounts/fireworks/models/llama-v3p1-70b", want: true},
		{pattern: "*:free", id: "vendor/model:free", want: true},
		{pattern: "*:free", id: "vendor/model", want: false},
		{pattern: "accounts/*/models/*", id: "accounts/fireworks/models/llama", want: true},

		// 问号恰好一个字符。
		{pattern: "gpt-?", id: "gpt-4", want: true},
		{pattern: "gpt-?", id: "gpt-40", want: false},
		{pattern: "gpt-?", id: "gpt-", want: false},

		// 大小写不敏感。
		{pattern: "GPT-5", id: "gpt-5", want: true},
		{pattern: "gpt*", id: "GPT-5-mini", want: true},

		// 纯字面量与退化模式。
		{pattern: "gpt-5", id: "gpt-5", want: true},
		{pattern: "gpt-5", id: "gpt-50", want: false},
		{pattern: "*", id: "anything/at/all", want: true},
		{pattern: "**", id: "x", want: true},
		{pattern: "*a*b", id: "aXb", want: true},
		{pattern: "*a*b", id: "baXb", want: true},
		{pattern: "*a*b", id: "ab", want: true},
		{pattern: "*a*b", id: "ba", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.pattern+"/"+tt.id, func(t *testing.T) {
			if got := Match(tt.pattern, tt.id); got != tt.want {
				t.Errorf("Match(%q, %q) = %v，期望 %v", tt.pattern, tt.id, got, tt.want)
			}
		})
	}
}

// 多个星号下必须是线性回溯而不是指数枚举：模式里每一段都要能对上 id 的某个划分，
// 交替匹配的输入最容易把朴素的递归实现拖进指数时间。
func TestMatchHandlesManyStarsLinearly(t *testing.T) {
	pattern := "*a*a*a*a*a*a*a*a*a*a*a*a*a*a*a*a*b"
	id := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if Match(pattern, id) {
		t.Error("不含 b 的 id 不应命中以 b 结尾的模式")
	}
}
