package convert

import (
	"fmt"

	"github.com/sumwai/nova/examples/internal/demo"
)

// upstream 是一条上游渠道：对外模型名唯一地指向一种上游线协议，
// 因此它同时是「本次走哪条渠道」与「上游该收到哪种协议」的标识。
type upstream struct {
	model    string
	protocol string
}

func upstreams() []upstream {
	return []upstream{
		{model: "via-chat", protocol: "openai_chat"},
		{model: "via-responses", protocol: "openai_responses"},
		{model: "via-anthropic", protocol: "anthropic_messages"},
		{model: "via-gemini", protocol: "gemini"},
	}
}

// clientCase 是一种客户端协议：路径、请求体与提交凭据的形态都不一样。
type clientCase struct {
	protocol string
	path     func(model string, stream bool) string
	body     func(model string, stream bool) string
	send     func(c *demo.Client, path, body string) (demo.Response, error)
	shape    string
}

func clients() []clientCase {
	return []clientCase{
		{
			protocol: "openai_chat",
			path:     func(string, bool) string { return "/v1/chat/completions" },
			body: func(model string, stream bool) string {
				return fmt.Sprintf(`{"model":%q,"stream":%t,"messages":[{"role":"user","content":"hi"}]}`, model, stream)
			},
			send:  func(c *demo.Client, path, body string) (demo.Response, error) { return c.Post(path, body) },
			shape: `"object":"chat.completion"`,
		},
		{
			protocol: "openai_responses",
			path:     func(string, bool) string { return "/v1/responses" },
			body: func(model string, stream bool) string {
				return fmt.Sprintf(`{"model":%q,"stream":%t,"input":"hi"}`, model, stream)
			},
			send:  func(c *demo.Client, path, body string) (demo.Response, error) { return c.Post(path, body) },
			shape: `"object":"response"`,
		},
		{
			protocol: "anthropic_messages",
			path:     func(string, bool) string { return "/v1/messages" },
			body: func(model string, stream bool) string {
				return fmt.Sprintf(`{"model":%q,"stream":%t,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`, model, stream)
			},
			send: func(c *demo.Client, path, body string) (demo.Response, error) {
				return c.PostAnthropic(path, body)
			},
			shape: `"type":"message"`,
		},
		{
			protocol: "gemini",
			path: func(model string, stream bool) string {
				action := ":generateContent"
				if stream {
					action = ":streamGenerateContent"
				}
				return "/v1beta/models/" + model + action
			},
			body: func(string, bool) string {
				return `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
			},
			send: func(c *demo.Client, path, body string) (demo.Response, error) {
				return c.PostGemini(path, body)
			},
			shape: `"candidates"`,
		},
	}
}

// Scenario 是 convert 的脚本：客户端四种协议 × 上游四种协议 × 流式与非流式。
//
// 每一步都对两件事下判定：客户端侧看到的是**自己的**协议形状（Anthropic 客户端拿到
// Anthropic 报文），上游侧打到的**是该模型名对应的**那种协议。前者由响应正文回答，
// 后者只有模拟上游的记录能回答。
func Scenario() demo.Scenario {
	steps := make([]demo.Step, 0, 32)
	for _, client := range clients() {
		for _, upstream := range upstreams() {
			for _, stream := range []bool{false, true} {
				steps = append(steps, convertStep(client, upstream, stream))
			}
		}
	}
	return demo.Scenario{
		Name:  "convert",
		Steps: steps,
	}
}

func convertStep(client clientCase, upstream upstream, stream bool) demo.Step {
	mode := "非流式"
	if stream {
		mode = "流式"
	}
	path := client.path(upstream.model, stream)
	return demo.Step{
		Title: fmt.Sprintf("%s 客户端 → %s 上游（%s）", client.protocol, upstream.protocol, mode),
		Expect: fmt.Sprintf("客户端拿到 200 与 chatmock: %s；模拟上游恰好收到一次尝试，协议 %s、%s",
			upstream.model, upstream.protocol, mode),
		Do: func(c *demo.Client) ([]string, error) {
			c.Reset()
			resp, err := client.send(c, path, client.body(upstream.model, stream))
			if err != nil {
				return nil, err
			}
			if err := resp.Expect(200, "chatmock: "+upstream.model); err != nil {
				return nil, err
			}
			if !stream {
				// 非流式才有完整形状可言，流式形状由各自的 SSE 承载。
				if err := resp.Expect(200, client.shape); err != nil {
					return nil, err
				}
			}
			attempts := c.Attempts(upstream.model)
			if err := demo.ExpectAttempts(attempts, 1); err != nil {
				return nil, err
			}
			if got := attempts[0].Protocol; got != upstream.protocol {
				return nil, demo.Fail("上游协议", got, upstream.protocol)
			}
			if attempts[0].Stream != stream {
				return nil, demo.Fail("上游流式标记", fmt.Sprintf("%v", attempts[0].Stream), fmt.Sprintf("%v", stream))
			}
			return demo.Facts(resp, attempts), nil
		},
	}
}
