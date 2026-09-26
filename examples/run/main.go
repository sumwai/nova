// 命令 run 一条命令跑完一个示例：起好环境、按脚本发请求、打印每一步的期望与实际。
//
//	go run ./examples/run                  # 列出可用示例
//	go run ./examples/run quickstart       # 跑完并打印结果，然后退出
//	go run ./examples/run quickstart -serve # 跑完不退出，保持服务并打印可直接粘贴的 curl
//
// 它不需要任何凭据、也不连真实上游：模拟上游由脚本按配置里的本机地址起出来，
// 客户端凭据用 stack.DefaultKey。
//
// 脚本本身在 examples/<示例名>/scenario.go，示例的校验读的是同一份，
// 因此「跑一遍看到的」与「make check 守住的」不会漂成两件事。
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/sumwai/nova/examples/accounts"
	"github.com/sumwai/nova/examples/convert"
	"github.com/sumwai/nova/examples/discover"
	"github.com/sumwai/nova/examples/gemini"
	"github.com/sumwai/nova/examples/internal/demo"
	"github.com/sumwai/nova/examples/internal/stack"
	"github.com/sumwai/nova/examples/quickstart"
	realproviders "github.com/sumwai/nova/examples/real-providers"
	routefallback "github.com/sumwai/nova/examples/route-fallback"
)

// scenarios 是全部可跑的示例。名字与目录名一致，因为例子本身就是「一个目录一份配置」。
//
// 这里显式列出来而不是扫描目录：每个示例的脚本都是 Go 代码，能被列出来的前提是
// 它被编译进来了；扫描目录只会得到一个「有配置但没脚本」的名字。
var scenarios = map[string]demo.Scenario{
	"quickstart":     quickstart.Scenario(),
	"convert":        convert.Scenario(),
	"route-fallback": routefallback.Scenario(),
	"accounts":       accounts.Scenario(),
	"discover":       discover.Scenario(),
	"gemini":         gemini.Scenario(),
	"real-providers": realproviders.Scenario(),
}

func main() {
	// 输出被下游截断时（例如管道接到 head）不要因此死掉。
	// Go 的默认行为是写坏了 fd 1 就死于 SIGPIPE，而那时 defer 里的收尾不会跑：
	// 留下的 nova 会占着端口，下一次运行得到的是一场「端口被占用」的假故障。
	signal.Ignore(syscall.SIGPIPE)

	name, serve, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：%v\n\n", err)
		usage()
		os.Exit(2)
	}
	if name == "" {
		usage()
		fmt.Printf("\n每个示例演示什么、校验在哪，见 examples/README.md。\n")
		return
	}
	if err := run(name, serve); err != nil {
		fmt.Fprintf(os.Stderr, "错误：%v\n", err)
		os.Exit(1)
	}
}

// parseArgs 解析位置参数与开关。
//
// 不交给 flag 包：它遇到第一个非开关参数就停止解析，于是 `run quickstart -serve`
// 里的 -serve 会被当成第二个示例名——而那是人最自然的写法。
func parseArgs(args []string) (name string, serve bool, err error) {
	for _, arg := range args {
		switch arg {
		case "-serve", "--serve":
			serve = true
		case "-h", "--help":
			return "", false, nil
		default:
			if strings.HasPrefix(arg, "-") {
				return "", false, fmt.Errorf("不认识的参数 %q", arg)
			}
			if name != "" {
				return "", false, fmt.Errorf("只接受一个示例名，收到 %q 与 %q", name, arg)
			}
			name = arg
		}
	}
	return name, serve, nil
}

func usage() {
	fmt.Printf("用法：go run ./examples/run <示例名> [-serve]\n\n可用示例：\n")
	for _, name := range names() {
		fmt.Printf("  %s\n", name)
	}
	fmt.Printf("\n不带示例名时只列出这个清单。加 -serve 则跑完不退出，保持服务并打印可直接粘贴的 curl。\n")
}

func run(name string, serve bool) error {
	scenario, ok := scenarios[name]
	if !ok {
		return fmt.Errorf("没有示例 %q；可用：%s", name, strings.Join(names(), "、"))
	}
	if serve && scenario.NoServe {
		return fmt.Errorf("示例 %s 不启动服务（它接真实上游，只做配置校验），因此没有可保持的服务", name)
	}

	root, err := stack.RepoRoot()
	if err != nil {
		return err
	}
	dir := filepath.Join(root, "examples", name)

	// 日志不直接串到 stdout：报告在前，日志只在失败时打出来。两者混在一起时，
	// 真正要看的那几行会被启动横幅与访问记录推走。
	//
	// `-serve` 是例外：那时人会自己 curl，nova 那一侧的日志得能实时看到，
	// 于是它走 stderr——报告与日志分流到两个流，管道接给别的命令时也互不干扰。
	var logSink io.Writer
	if serve {
		logSink = os.Stderr
	}
	client, closeEnv, err := demo.Start(scenario, dir, logSink)
	if err != nil {
		return err
	}
	defer closeEnv()

	results := demo.Execute(client, scenario.Steps)
	fmt.Printf("示例 %s（%s）\n", name, dir)
	demo.Print(os.Stdout, results)

	failed := 0
	for _, result := range results {
		if !result.Passed() {
			failed++
		}
	}
	if failed > 0 {
		printNovaLog(client)
		return fmt.Errorf("%d 步判定不成立", failed)
	}
	if len(results) != len(scenario.Steps) {
		return fmt.Errorf("%d 步只跑了 %d 步", len(scenario.Steps), len(results))
	}
	fmt.Printf("\n%d 步全部通过\n", len(results))

	if !serve {
		return nil
	}
	return hold(client)
}

// hold 保持服务直到收到中断信号，并打印可直接粘贴的 curl。
//
// 与「跑完即退」分开：报告回答的是「这个例子成不成立」，而这里回答的是
// 「想自己动手试两下时从哪开始」。两件事都要，但不必每次都做后一件。
func hold(client *demo.Client) error {
	model, err := firstModel(client)
	if err != nil {
		return err
	}
	printCurls(client, model)
	fmt.Printf("\n按 Ctrl-C 结束（nova 收到 TERM 后正常收尾）。\n")

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	<-signals
	fmt.Printf("\n正在停止\n")
	return nil
}

// firstModel 问运行中的 nova 有哪些对外模型，取第一个。
//
// 用网关自己的 GET /v1/models 而不是解析 nova models 的文本输出：那是同一份目录的
// 机器可读形态，而这里的模型名要精确地拼进 curl 的路径里。
func firstModel(client *demo.Client) (string, error) {
	resp, err := client.Get("/v1/models")
	if err != nil {
		return "", err
	}
	if err := resp.Expect(200, ""); err != nil {
		return "", err
	}
	var listing struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(resp.Body), &listing); err != nil {
		return "", fmt.Errorf("解析 GET /v1/models 失败：%w", err)
	}
	if len(listing.Data) == 0 {
		return "", fmt.Errorf("这个示例当前没有对外模型")
	}
	return listing.Data[0].ID, nil
}

// printCurls 打印四条按客户端协议分的访问命令，模型名取目录里的第一个。
//
// 同一个模型名在四种路径下都能用：客户端协议由路径决定，与上游协议无关，
// 这正是 examples/convert 要证明的那件事。
func printCurls(client *demo.Client, model string) {
	base, key := client.BaseURL, client.Key
	fmt.Printf("\n对外模型目录里的第一个：%s\n", model)
	fmt.Printf("下面四条都能用（客户端协议由路径决定，与上游协议无关）：\n\n")
	fmt.Printf("# OpenAI Chat Completions\n")
	fmt.Printf("curl -sS %s/v1/chat/completions \\\n  -H 'Authorization: Bearer %s' -H 'Content-Type: application/json' \\\n  -d '{\"model\":%q,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}'\n\n", base, key, model)
	fmt.Printf("# 同一协议的流式（逐帧 SSE）\n")
	fmt.Printf("curl -sS -N %s/v1/chat/completions \\\n  -H 'Authorization: Bearer %s' -H 'Content-Type: application/json' \\\n  -d '{\"model\":%q,\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}'\n\n", base, key, model)
	fmt.Printf("# Anthropic Messages\n")
	fmt.Printf("curl -sS %s/v1/messages \\\n  -H 'Authorization: Bearer %s' -H 'anthropic-version: 2023-06-01' -H 'Content-Type: application/json' \\\n  -d '{\"model\":%q,\"max_tokens\":64,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}'\n\n", base, key, model)
	fmt.Printf("# Gemini（模型名与动作在路径上，凭据用 x-goog-api-key）\n")
	fmt.Printf("curl -sS '%s/v1beta/models/%s:generateContent' \\\n  -H 'x-goog-api-key: %s' -H 'Content-Type: application/json' \\\n  -d '{\"contents\":[{\"role\":\"user\",\"parts\":[{\"text\":\"hi\"}]}]}'\n\n", base, model, key)
	fmt.Printf("# 模型目录、统计与存活\n")
	fmt.Printf("curl -sS %s/v1/models -H 'Authorization: Bearer %s' | head -c 400\n", base, key)
	fmt.Printf("curl -sS '%s/debug/stats?pretty=true&since=-1h' | head -c 400\n", base)
	fmt.Printf("curl -sS %s/healthz\n", base)
}

// printNovaLog 在失败时把服务进程的日志打出来。
//
// 判定不成立时的第一现场在日志里：哪条渠道被打到、上游回了什么、有没有重试。
func printNovaLog(client *demo.Client) {
	if client.Stack == nil {
		return
	}
	fmt.Printf("\nnova 日志：\n%s\n", client.Stack.Log())
}

// names 返回排好序的示例名，输出因此稳定可比。
func names() []string {
	out := make([]string, 0, len(scenarios))
	for name := range scenarios {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
