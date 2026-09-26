// real-providers 的校验跑的就是 Scenario() 里的步骤。
//
// 它接的是真实上游，因此 NoServe：只构建二进制并跑 nova config check（该命令按设计不联网，
// 连发现型端点的清单地址也不请求）。能被校验的是配置本身的写法——地址、协议推导、语法、
// 指令清单、{env.NAME} 的缺失判定——而「上游是否真的可用」不属于离线校验能回答的问题。
package realproviders

import (
	"strconv"
	"strings"

	"github.com/sumwai/nova/examples/internal/demo"
)

// env 是配置里用到的变量名及其占位取值。config check 不连上游，因此不需要真实凭据；
// 取值刻意是占位串，避免有人误以为这里需要填一份真凭据才能跑。
var env = map[string]string{
	"NOVA_CLIENT_KEY":   "placeholder-client-key",
	"OPENAI_API_KEY":    "placeholder-openai-key",
	"ANTHROPIC_API_KEY": "placeholder-anthropic-key",
	"GEMINI_API_KEY":    "placeholder-gemini-key",
	"RELAY_API_KEY":     "placeholder-relay-key",
}

// Scenario 是 real-providers 的脚本。
func Scenario() demo.Scenario {
	return demo.Scenario{
		Name:    "real-providers",
		Env:     env,
		NoServe: true,
		Steps: []demo.Step{
			{
				Title: "四条真实渠道的配置写法成立",
				Expect: "nova config check 退出码 0：地址、协议推导、指令与 {env.NAME} 都被接受；" +
					"发现型端点被点名为「清单内容由上游决定，本次校验没有连上游」",
				Do: func(c *demo.Client) ([]string, error) {
					output, code := c.CLI.CLI("config", "check", "-c", "Novafile")
					fact := demo.CLIFact([]string{"config", "check"}, output, code)
					if code != 0 {
						return []string{fact}, demo.Fail("config check 退出码", strconv.Itoa(code), "0")
					}
					for _, want := range []string{"校验通过", "监听 127.0.0.1:8080", "没有连上游"} {
						if !strings.Contains(output, want) {
							return []string{fact}, demo.Fail("config check 输出", output, "含 "+want)
						}
					}
					return []string{fact}, nil
				},
			},
			{
				Title: "缺少凭据时加载期就拒绝",
				Expect: "去掉 OPENAI_API_KEY 后 config check 非零退出并指出变量名：" +
					"空密钥在加载期报错，而不是等到第一次真实调用变成 401",
				Do: func(c *demo.Client) ([]string, error) {
					// 这一步换一份环境跑：同一个配置文件，少一个变量。
					cli := c.WithEnv(map[string]string{"OPENAI_API_KEY": ""})
					output, code := cli.CLI("config", "check", "-c", "Novafile")
					fact := demo.CLIFact([]string{"config", "check"}, output, code)
					if code == 0 {
						return []string{fact}, demo.Fail("config check 退出码", "0", "非 0")
					}
					if !strings.Contains(output, "OPENAI_API_KEY") {
						return []string{fact}, demo.Fail("报错输出", output, "含 OPENAI_API_KEY")
					}
					return []string{fact}, nil
				},
			},
		},
	}
}
