package accounts

import (
	"strconv"
	"strings"

	"github.com/sumwai/nova/examples/internal/demo"
)

// Scenario 是 accounts 的脚本，证明账号池的三条边界：
//
//   - 可重试的上游失败（限流）会换到下一条账号，客户端最终拿到成功；
//   - 不可重试的上游失败（请求被拒绝）**不**换账号——换一条也不会让它成功；
//   - balance 让账号池按权重轮转起点，而不是固定先试第一条。
//
// 账号在日志里的引用（acct #1 / acct #2）也在这里被断言：多账号时它是
// 「这次尝试用的是谁」在运行时的唯一线索，不能只存在于实现里。
func Scenario() demo.Scenario {
	call := func(c *demo.Client, model string) (demo.Response, error) {
		return c.Post("/v1/chat/completions",
			`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`)
	}

	return demo.Scenario{
		Name: "accounts",
		Steps: []demo.Step{
			{
				Title:  "可重试失败换到下一条账号",
				Expect: "客户端拿到 200；模拟上游收到两次尝试（mock-key-429 后 mock-key-ok）；日志里出现 tries 2 与 acct #1 / acct #2",
				Do: func(c *demo.Client) ([]string, error) {
					c.Reset()
					before := c.Stack.Log()
					resp, err := call(c, "retry-me")
					if err != nil {
						return nil, err
					}
					if err := resp.Expect(200, "chatmock: retry-me"); err != nil {
						return nil, err
					}
					attempts := c.Attempts("retry-me")
					if err := demo.ExpectKeys(attempts, "mock-key-429", "mock-key-ok"); err != nil {
						return nil, err
					}
					lines := demo.Since(before, c.Stack.Log())
					facts := demo.Facts(resp, attempts)
					for _, want := range []string{"tries 2", "acct #1", "acct #2"} {
						if !strings.Contains(lines, want) {
							return facts, demo.Fail("本次请求的日志", lines, "含 "+want)
						}
					}
					return append(facts, "日志含 tries 2、acct #1、acct #2"), nil
				},
			},
			{
				Title:  "不可重试失败不换账号",
				Expect: "客户端拿到 502 与 upstream_rejected；模拟上游只有一次尝试，用的是第一条账号 mock-key-bad",
				Do: func(c *demo.Client) ([]string, error) {
					c.Reset()
					resp, err := call(c, "noretry-me")
					if err != nil {
						return nil, err
					}
					if err := resp.Expect(502, "upstream_rejected"); err != nil {
						return nil, err
					}
					attempts := c.Attempts("noretry-me")
					if err := demo.ExpectKeys(attempts, "mock-key-bad"); err != nil {
						return nil, err
					}
					return demo.Facts(resp, attempts), nil
				},
			},
			{
				Title:  "balance 轮转起点",
				Expect: "两次请求分别从 mock-key-ok-a、mock-key-ok-b 开始：起点每次前移一个位置",
				Do: func(c *demo.Client) ([]string, error) {
					c.Reset()
					facts := make([]string, 0, 4)
					for range 2 {
						resp, err := call(c, "balanced-me")
						if err != nil {
							return nil, err
						}
						if err := resp.Expect(200, "chatmock: balanced-me"); err != nil {
							return nil, err
						}
						facts = append(facts, resp.Fact())
					}
					attempts := c.Attempts("balanced-me")
					if err := demo.ExpectKeys(attempts, "mock-key-ok-a", "mock-key-ok-b"); err != nil {
						return nil, err
					}
					return append(facts, demo.AttemptFacts(attempts)...), nil
				},
			},
			{
				Title:  "配置校验给出账号池摘要",
				Expect: "nova config check 退出码 0；账号池那一行给出渠道数、账号数与调度口径（几个按权重轮询分摊、几个按声明顺序）",
				Do: func(c *demo.Client) ([]string, error) {
					output, code := c.CLI.CLI("config", "check", "-c", "Novafile")
					if code != 0 {
						return []string{demo.CLIFact([]string{"config", "check"}, output, code)}, demo.Fail("config check 退出码", strconv.Itoa(code), "0")
					}
					// 账号池摘要是第二行，CLIFact 只取首行（校验通过），因此单独取它：
					// 它既是被断言的对象，也是报告里要看的那行事实。
					pool := demo.LineContaining(output, "账号池：")
					if pool == "" {
						return nil, demo.Fail("config check 输出", output, "含 账号池 摘要行")
					}
					for _, want := range []string{"3 个渠道共 6 个账号", "按权重轮询分摊", "按声明顺序"} {
						if !strings.Contains(pool, want) {
							return []string{pool}, demo.Fail("账号池摘要行", pool, "含 "+want)
						}
					}
					return []string{pool}, nil
				},
			},
		},
	}
}
