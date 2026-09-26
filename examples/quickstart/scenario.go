package quickstart

import (
	"github.com/sumwai/nova/examples/internal/demo"
)

// Scenario 是 quickstart 的脚本。
//
// 它证明最小的那条路能走通：配置能被装配、客户端凭据生效、请求被转发到上游、上游的真实回包
// 被送回客户端，流式与非流式都如此。它是 examples 里唯一不演示「某个特性」的例子，
// 给出的是后面所有例子的基线。
func Scenario() demo.Scenario {
	const chatBody = `{"model":"echo-1","messages":[{"role":"user","content":"hi"}]}`

	return demo.Scenario{
		Name: "quickstart",
		Steps: []demo.Step{
			{
				Title:  "存活探针不经鉴权",
				Expect: "GET /healthz 回 200 与 ok，完全不带凭据也放行：探活只回答进程是否在线",
				Do: func(c *demo.Client) ([]string, error) {
					resp, err := c.Do(demo.Request{Method: "GET", Path: "/healthz", NoKey: true})
					if err != nil {
						return nil, err
					}
					if err := resp.Expect(200, "ok"); err != nil {
						return nil, err
					}
					return []string{resp.Fact()}, nil
				},
			},
			{
				Title:  "缺少客户端凭据被挡回",
				Expect: "不带凭据的转发请求得到 401：命中鉴权，而不是被当成一个能转发的请求",
				Do: func(c *demo.Client) ([]string, error) {
					resp, err := c.Do(demo.Request{Path: "/v1/chat/completions", Body: chatBody, NoKey: true})
					if err != nil {
						return nil, err
					}
					if err := resp.Expect(401, ""); err != nil {
						return nil, err
					}
					return []string{resp.Fact()}, nil
				},
			},
			{
				Title:  "三种凭据头任一种都放行",
				Expect: "Authorization: Bearer（其它步骤都在用）、x-api-key、x-goog-api-key 三种提交方式都被接受：换了头也照样拿到 chatmock: echo-1",
				Do: func(c *demo.Client) ([]string, error) {
					c.Reset()
					facts := make([]string, 0, 2)
					for _, header := range []string{"x-api-key", "x-goog-api-key"} {
						resp, err := c.Do(demo.Request{
							Path:    "/v1/chat/completions",
							Body:    chatBody,
							Headers: map[string]string{"Content-Type": "application/json", header: c.Key},
							NoKey:   true,
						})
						if err != nil {
							return facts, err
						}
						if err := resp.Expect(200, "chatmock: echo-1"); err != nil {
							return facts, err
						}
						facts = append(facts, header+" 提交："+resp.Fact())
					}
					return facts, nil
				},
			},
			{
				Title:  "非流式请求被转发并回显上游模型名",
				Expect: "客户端拿到 200；模拟上游恰好收到一次尝试，协议 openai_chat、非流式、上游模型名与对外名相同",
				Do: func(c *demo.Client) ([]string, error) {
					c.Reset()
					resp, err := c.Post("/v1/chat/completions", chatBody)
					if err != nil {
						return nil, err
					}
					if err := resp.Expect(200, "chatmock: echo-1"); err != nil {
						return nil, err
					}
					attempts := c.Attempts("echo-1")
					if err := demo.ExpectAttempts(attempts, 1); err != nil {
						return nil, err
					}
					if attempts[0].Protocol != "openai_chat" {
						return nil, demo.Fail("上游协议", attempts[0].Protocol, "openai_chat")
					}
					if attempts[0].Stream {
						return nil, demo.Fail("上游流式标记", "流式", "非流式")
					}
					return demo.Facts(resp, attempts), nil
				},
			},
			{
				Title:  "流式请求按 SSE 下发",
				Expect: "Content-Type 是 text/event-stream，正文含回显文本并以 data: [DONE] 收尾（同协议走原报文透传）",
				Do: func(c *demo.Client) ([]string, error) {
					c.Reset()
					resp, err := c.Post("/v1/chat/completions",
						`{"model":"echo-1","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
					if err != nil {
						return nil, err
					}
					if err := resp.Expect(200, "chatmock: echo-1"); err != nil {
						return nil, err
					}
					if err := resp.Expect(200, "data: [DONE]"); err != nil {
						return nil, err
					}
					if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
						return nil, demo.Fail("Content-Type", got, "text/event-stream")
					}
					attempts := c.Attempts("echo-1")
					if err := demo.ExpectAttempts(attempts, 1); err != nil {
						return nil, err
					}
					return demo.Facts(resp, attempts), nil
				},
			},
			{
				Title:  "模型清单按请求头给出两种形状",
				Expect: "不带 anthropic-version 回 OpenAI 形状（owned_by），带上回 Anthropic 形状（display_name），两种都含 echo-1",
				Do: func(c *demo.Client) ([]string, error) {
					openai, err := c.Get("/v1/models")
					if err != nil {
						return nil, err
					}
					if err := openai.Expect(200, `"owned_by":"nova"`); err != nil {
						return nil, err
					}
					if err := openai.Expect(200, "echo-1"); err != nil {
						return nil, err
					}
					anthropic, err := c.GetAnthropic("/v1/models")
					if err != nil {
						return nil, err
					}
					if err := anthropic.Expect(200, `"display_name":"echo-1"`); err != nil {
						return nil, err
					}
					return []string{openai.Fact(), anthropic.Fact()}, nil
				},
			},
			{
				Title:  "未知模型不被猜着转发",
				Expect: "得到 404 与 model_not_found，且模拟上游一次请求都没收到",
				Do: func(c *demo.Client) ([]string, error) {
					c.Reset()
					resp, err := c.Post("/v1/chat/completions",
						`{"model":"no-such-model","messages":[{"role":"user","content":"hi"}]}`)
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
		},
	}
}
