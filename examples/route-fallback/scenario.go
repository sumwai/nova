package routefallback

import (
	"strings"

	"github.com/sumwai/nova/examples/internal/demo"
)

// Scenario 是 route-fallback 的脚本，证明四件事：
//
//   - 主用候选失败后请求确实落到了回退候选上，客户端看到的是成功（回退对客户端不可见）；
//   - 上游想超时（不回包）走的是与 5xx 同一条回退路径；
//   - route 规则写下的顺序就是最终顺序，可以覆盖渠道的声明顺序（优先级重排）；
//   - balance 与权重把主用候选按比例轮转起点，而不是随机挑一个。
//
// 判定全部落在模拟上游的记录上：客户端只看到「一次成功」，而「先试了谁、换了几次」只有记录能回答。
func Scenario() demo.Scenario {
	call := func(c *demo.Client, model string) (demo.Response, error) {
		return c.Post("/v1/chat/completions",
			`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`)
	}

	return demo.Scenario{
		Name: "route-fallback",
		Steps: []demo.Step{
			{
				Title:  "主用失败后换回退候选",
				Expect: "客户端拿到 200；模拟上游收到两次尝试，先是失败的 relay-a（mock-key-5xx），后是成功的 relay-b（mock-key-ok）",
				Do: func(c *demo.Client) ([]string, error) {
					c.Reset()
					resp, err := call(c, "shared")
					if err != nil {
						return nil, err
					}
					if err := resp.Expect(200, "chatmock: shared"); err != nil {
						return nil, err
					}
					attempts := c.Attempts("shared")
					if err := demo.ExpectKeys(attempts, "mock-key-5xx", "mock-key-ok"); err != nil {
						return nil, err
					}
					return demo.Facts(resp, attempts), nil
				},
			},
			{
				Title:  "规则顺序覆盖渠道声明顺序",
				Expect: "只有一次尝试：规则把成功的 relay-b 写在前面，因此不需要第二次尝试",
				Do: func(c *demo.Client) ([]string, error) {
					c.Reset()
					resp, err := call(c, "reordered")
					if err != nil {
						return nil, err
					}
					if err := resp.Expect(200, "chatmock: reordered"); err != nil {
						return nil, err
					}
					attempts := c.Attempts("reordered")
					if err := demo.ExpectKeys(attempts, "mock-key-ok"); err != nil {
						return nil, err
					}
					return demo.Facts(resp, attempts), nil
				},
			},
			{
				Title:  "上游超时后换回退候选",
				Expect: "客户端拿到 200；模拟上游两次尝试：第一次注入故障=slow（端点 timeout 200ms 后判为上游超时），第二次成功；日志里出现 upstream_timeout",
				Do: func(c *demo.Client) ([]string, error) {
					c.Reset()
					before := c.Stack.Log()
					resp, err := call(c, "timed-out")
					if err != nil {
						return nil, err
					}
					if err := resp.Expect(200, "chatmock: timed-out"); err != nil {
						return nil, err
					}
					attempts := c.Attempts("timed-out")
					if err := demo.ExpectKeys(attempts, "mock-key-slow", "mock-key-ok"); err != nil {
						return nil, err
					}
					lines := demo.Since(before, c.Stack.Log())
					facts := demo.Facts(resp, attempts)
					if !strings.Contains(lines, "upstream_timeout") {
						return facts, demo.Fail("本次请求的日志", lines, "含 upstream_timeout")
					}
					return append(facts, "日志含 upstream_timeout"), nil
				},
			},
			{
				Title:  "加权轮转起点",
				Expect: "4 次请求的起点序列是 relay-c、relay-c、relay-c、relay-d：3:1 的环按每次一个位置前移",
				Do: func(c *demo.Client) ([]string, error) {
					c.Reset()
					facts := make([]string, 0, 8)
					for range 4 {
						resp, err := call(c, "balanced")
						if err != nil {
							return nil, err
						}
						if err := resp.Expect(200, "chatmock: balanced"); err != nil {
							return nil, err
						}
						facts = append(facts, resp.Fact())
					}
					attempts := c.Attempts("balanced")
					if err := demo.ExpectKeys(attempts,
						"mock-key-ok-a", "mock-key-ok-a", "mock-key-ok-a", "mock-key-ok-b"); err != nil {
						return nil, err
					}
					return append(facts, demo.AttemptFacts(attempts)...), nil
				},
			},
		},
	}
}
