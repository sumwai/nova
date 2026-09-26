package gemini

import (
	"strings"

	"github.com/sumwai/nova/examples/internal/demo"
)

const geminiBody = `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`

// Scenario 是 gemini 的脚本，证明 Gemini 的两侧都成立、形态差异都被正确吸收：
//
//   - 作为客户端协议：模型名与流式标记从路径读，回包按 Gemini 形状编码；
//   - 作为上游协议：地址模板里的 {model} 被换成选路结果，末段在两种动作之间互换，
//     流式另加 alt=sse，凭据以 x-goog-api-key 注入；
//   - 配置写错（url 缺 {model}）在加载期就被拒绝，而不是运行期静默发错模型。
func Scenario() demo.Scenario {
	return demo.Scenario{
		Name: "gemini",
		Steps: []demo.Step{
			{
				Title:  "Gemini 客户端到非 Gemini 上游",
				Expect: "客户端拿到 200 与 chatmock: gemini-2.5-flash；模拟上游收到的是 openai_chat 非流式，凭据头是 Authorization",
				Do: func(c *demo.Client) ([]string, error) {
					c.Reset()
					resp, err := c.PostGemini("/v1beta/models/gemini-2.5-flash:generateContent", geminiBody)
					if err != nil {
						return nil, err
					}
					if err := resp.Expect(200, "chatmock: gemini-2.5-flash"); err != nil {
						return nil, err
					}
					attempts := c.Attempts("gemini-2.5-flash")
					if err := demo.ExpectAttempts(attempts, 1); err != nil {
						return nil, err
					}
					if got := attempts[0].Protocol; got != "openai_chat" {
						return nil, demo.Fail("上游协议", got, "openai_chat")
					}
					if got := attempts[0].CredentialHeader; got != "Authorization" {
						return nil, demo.Fail("上游凭据头", got, "Authorization")
					}
					if attempts[0].Stream {
						return nil, demo.Fail("上游流式标记", "流式", "非流式")
					}
					return demo.Facts(resp, attempts), nil
				},
			},
			{
				Title:  "Gemini 客户端流式",
				Expect: "Content-Type 是 text/event-stream，正文含回显文本；模拟上游收到的是流式调用",
				Do: func(c *demo.Client) ([]string, error) {
					c.Reset()
					resp, err := c.PostGemini("/v1beta/models/gemini-2.5-flash:streamGenerateContent", geminiBody)
					if err != nil {
						return nil, err
					}
					if err := resp.Expect(200, "chatmock: gemini-2.5-flash"); err != nil {
						return nil, err
					}
					if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
						return nil, demo.Fail("Content-Type", got, "text/event-stream")
					}
					attempts := c.Attempts("gemini-2.5-flash")
					if err := demo.ExpectAttempts(attempts, 1); err != nil {
						return nil, err
					}
					if !attempts[0].Stream {
						return nil, demo.Fail("上游流式标记", "非流式", "流式")
					}
					return demo.Facts(resp, attempts), nil
				},
			},
			{
				Title: "非 Gemini 客户端到 Gemini 上游",
				Expect: "客户端拿到 200；上游路径是 /v1beta/models/gemini-2.5-pro-001:generateContent" +
					"（对外名 gem-pro 已被改写成上游名），凭据头是 x-goog-api-key",
				Do: func(c *demo.Client) ([]string, error) {
					c.Reset()
					resp, err := c.Post("/v1/chat/completions",
						`{"model":"gem-pro","messages":[{"role":"user","content":"hi"}]}`)
					if err != nil {
						return nil, err
					}
					if err := resp.Expect(200, "chatmock: gemini-2.5-pro-001"); err != nil {
						return nil, err
					}
					attempts := c.Attempts("gemini-2.5-pro-001")
					if err := demo.ExpectAttempts(attempts, 1); err != nil {
						return nil, err
					}
					want := "/v1beta/models/gemini-2.5-pro-001:generateContent"
					if got := attempts[0].Path; got != want {
						return demo.Facts(resp, attempts), demo.Fail("上游路径", got, want)
					}
					if got := attempts[0].CredentialHeader; got != "x-goog-api-key" {
						return demo.Facts(resp, attempts), demo.Fail("上游凭据头", got, "x-goog-api-key")
					}
					return demo.Facts(resp, attempts), nil
				},
			},
			{
				Title:  "流式请求把动作段换成 streamGenerateContent 并加 alt=sse",
				Expect: "上游路径是 /v1beta/models/gemini-2.5-pro-001:streamGenerateContent?alt=sse",
				Do: func(c *demo.Client) ([]string, error) {
					c.Reset()
					resp, err := c.Post("/v1/chat/completions",
						`{"model":"gem-pro","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
					if err != nil {
						return nil, err
					}
					if err := resp.Expect(200, "chatmock: gemini-2.5-pro-001"); err != nil {
						return nil, err
					}
					attempts := c.Attempts("gemini-2.5-pro-001")
					if err := demo.ExpectAttempts(attempts, 1); err != nil {
						return nil, err
					}
					want := "/v1beta/models/gemini-2.5-pro-001:streamGenerateContent?alt=sse"
					if got := attempts[0].Path; got != want {
						return demo.Facts(resp, attempts), demo.Fail("上游路径", got, want)
					}
					if !attempts[0].Stream {
						return demo.Facts(resp, attempts), demo.Fail("上游流式标记", "非流式", "流式")
					}
					return demo.Facts(resp, attempts), nil
				},
			},
			{
				Title: "地址缺 {model} 时加载期就拒绝",
				Expect: "nova config check 对 Novafile.invalid-url 非零退出，并指出缺 {model}：" +
					"缺失的后果是所有模型都发到同一个模型上，那种错不会被上游明确回绝",
				Do: func(c *demo.Client) ([]string, error) {
					output, code := c.CLI.CLI("config", "check", "-c", "Novafile.invalid-url")
					fact := demo.CLIFact([]string{"config", "check", "-c", "Novafile.invalid-url"}, output, code)
					if code == 0 {
						return []string{fact}, demo.Fail("config check 退出码", "0", "非 0")
					}
					if !strings.Contains(output, "{model}") {
						return []string{fact}, demo.Fail("报错输出", output, "含 {model}")
					}
					return []string{fact}, nil
				},
			},
		},
	}
}
