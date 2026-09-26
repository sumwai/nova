// 命令 chatmock 是示例用的模拟上游。它是 examples/internal/chatmock 那份处理器的命令行外壳。
//
// 单机试一个例子时：
//
//	go run ./examples/chatmock
//	go run ./examples/chatmock -addr 127.0.0.1:18080 -models gpt-5,gpt-5-mini
//
// 不参与构建产物：nova 自身的二进制只有 bin/nova 与 $(PREFIX)/bin/nova 两个出口，
// 这个命令只在 examples/ 的说明与示例校验里被引用。
package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/sumwai/nova/examples/internal/chatmock"
)

// defaultModels 是清单端点的缺省模型集合。
const defaultModels = "gpt-5,gpt-5-mini,gpt-4o-preview,claude-sonnet-4,llama-3"

func main() {
	addr := flag.String("addr", "127.0.0.1:18080", "监听地址")
	models := flag.String("models", strings.Join(chatmock.DefaultModels, ","), "清单端点返回的模型 id，逗号分隔")
	flag.Parse()

	parsed := make([]string, 0, 4)
	for _, model := range strings.Split(*models, ",") {
		if trimmed := strings.TrimSpace(model); trimmed != "" {
			parsed = append(parsed, trimmed)
		}
	}

	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "chatmock: 无法监听 %s：%v\n", *addr, err)
		os.Exit(1)
	}

	fmt.Printf("chatmock 监听 %s\n", listener.Addr())
	fmt.Printf("  上游端点：/…/chat/completions  /…/responses  /…/messages  /v1beta/models/<模型>:generateContent 与 :streamGenerateContent\n")
	fmt.Printf("  清单端点：/…/models（带 anthropic-version 头时按 Anthropic 形状回答）\n")
	fmt.Printf("  清单内容：%s\n", strings.Join(parsed, " "))
	fmt.Printf("  故障注入：凭据以 -%s / -%s / -%s / -%s 结尾时分别回对应故障\n",
		chatmock.FaultRateLimited, chatmock.FaultUnavailable, chatmock.FaultSlow, chatmock.FaultRejected)

	if err := http.Serve(listener, chatmock.New(parsed)); err != nil {
		fmt.Fprintf(os.Stderr, "chatmock: 服务结束：%v\n", err)
		os.Exit(1)
	}
}
