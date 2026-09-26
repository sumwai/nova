package discover

import (
	"strconv"
	"strings"

	"github.com/sumwai/nova/examples/internal/demo"
)

// Scenario 是 discover 的脚本，证明「上游清单 → 对外模型目录」这条链路的三段各自独立：
//
//   - allow / deny 决定保不保留（按上游 id 判断，先于改名）；
//   - expose 决定暴露成什么名字（替换，原名不再存在）；
//   - 显式 model 永远生效且优先，于是同一个上游 id 可以同时有两个对外名。
//
// 被过滤掉的模型与上游不存在的模型对客户端不可区分，都是 model_not_found：
// 网关不泄露「上游有哪些东西」。
func Scenario() demo.Scenario {
	call := func(c *demo.Client, model string) (demo.Response, error) {
		return c.Post("/v1/chat/completions",
			`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`)
	}

	return demo.Scenario{
		Name: "discover",
		Steps: []demo.Step{
			{
				Title: "对外目录只含保留与改名后的名字",
				Expect: "目录里出现 sensenova/gpt-5、sensenova/gpt-5-mini 与显式声明的 gpt-5；" +
					"被 deny 的 gpt-4o-preview、未被 allow 命中的 claude-sonnet-4 与 llama-3 都不出现",
				Do: func(c *demo.Client) ([]string, error) {
					resp, err := c.Get("/v1/models")
					if err != nil {
						return nil, err
					}
					facts := []string{resp.Fact()}
					for _, want := range []string{"sensenova/gpt-5", "sensenova/gpt-5-mini", `"gpt-5"`} {
						if err := resp.Expect(200, want); err != nil {
							return facts, err
						}
					}
					for _, absent := range []string{"gpt-4o-preview", "claude-sonnet-4", "llama-3"} {
						if err := resp.ExpectAbsent(absent); err != nil {
							return facts, err
						}
					}
					return facts, nil
				},
			},
			{
				Title:  "改名后的名字把原名指回上游",
				Expect: "请求 sensenova/gpt-5 时模拟上游收到的模型名是 gpt-5：expose 只是客户端侧的名字",
				Do: func(c *demo.Client) ([]string, error) {
					c.Reset()
					resp, err := call(c, "sensenova/gpt-5")
					if err != nil {
						return nil, err
					}
					if err := resp.Expect(200, "chatmock: gpt-5"); err != nil {
						return nil, err
					}
					attempts := c.Attempts("gpt-5")
					if err := demo.ExpectAttempts(attempts, 1); err != nil {
						return nil, err
					}
					return demo.Facts(resp, attempts), nil
				},
			},
			{
				Title:  "显式声明与发现同名时两个名字同时存在",
				Expect: "请求 gpt-5 时上游收到的是被改写后的 gpt-5-2025-01-01：显式那条胜出并改写上游名",
				Do: func(c *demo.Client) ([]string, error) {
					c.Reset()
					resp, err := call(c, "gpt-5")
					if err != nil {
						return nil, err
					}
					if err := resp.Expect(200, "chatmock: gpt-5-2025-01-01"); err != nil {
						return nil, err
					}
					attempts := c.Attempts("gpt-5-2025-01-01")
					if err := demo.ExpectAttempts(attempts, 1); err != nil {
						return nil, err
					}
					return demo.Facts(resp, attempts), nil
				},
			},
			{
				Title:  "被 deny 排除的模型不可见",
				Expect: "gpt-4o-preview 得到 404 与 model_not_found，模拟上游一次请求都没收到",
				Do: func(c *demo.Client) ([]string, error) {
					c.Reset()
					resp, err := call(c, "gpt-4o-preview")
					if err != nil {
						return nil, err
					}
					if err := resp.Expect(404, "model_not_found"); err != nil {
						return nil, err
					}
					if err := demo.ExpectAttempts(c.AllAttempts(), 0); err != nil {
						return nil, err
					}
					return []string{resp.Fact()}, nil
				},
			},
			{
				Title:  "没被 allow 命中的模型不可见",
				Expect: "claude-sonnet-4 得到 404 与 model_not_found，模拟上游一次请求都没收到",
				Do: func(c *demo.Client) ([]string, error) {
					c.Reset()
					resp, err := call(c, "claude-sonnet-4")
					if err != nil {
						return nil, err
					}
					if err := resp.Expect(404, "model_not_found"); err != nil {
						return nil, err
					}
					if err := demo.ExpectAttempts(c.AllAttempts(), 0); err != nil {
						return nil, err
					}
					return []string{resp.Fact()}, nil
				},
			},
			{
				Title: "命令行能列出保留与排除的明细",
				Expect: "nova models --verbose 给出清单地址（/api/v4/models，版本根原样保留）、" +
					"发现 5 保留 2 排除 3、改名映射与显式声明的改写、以及目录汇总",
				Do: func(c *demo.Client) ([]string, error) {
					output, code := c.CLI.CLI("models", "-c", "Novafile", "--verbose")
					fact := demo.CLIFact([]string{"models", "--verbose"}, output, code)
					if code != 0 {
						return nil, demo.Fail("nova models 退出码", strconv.Itoa(code), "0")
					}
					for _, want := range []string{
						"/api/v4/models",
						"发现 5 保留 2 排除 3",
						"sensenova/gpt-5  → gpt-5  discovered",
						"gpt-5  → gpt-5-2025-01-01",
						"gpt-4o-preview",
						"对外模型 3 个（显式 1 个，发现 2 个）",
					} {
						if !contains(output, want) {
							return []string{fact}, demo.Fail("nova models 输出", output, "含 "+want)
						}
					}
					return []string{fact}, nil
				},
			},
		},
	}
}

func contains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}
