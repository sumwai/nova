// Package harness 把 demo 的脚本接到 go test 上。
//
// 它只有一件事要做：把脚本的逐步结果变成测试的通过或失败，并在失败时把
// 同一块报告加上 nova 的日志一起打出来。示例的请求、期望与判定都不在这里——
// 那些在各自的 demo.Scenario 里，交互式运行读的也是同一份。
package harness

import (
	"os"
	"testing"

	"github.com/sumwai/nova/examples/internal/demo"
)

// Run 在一个测试里跑一份示例脚本：任一步判定不成立即失败并给出该步的报告。
func Run(t *testing.T, s demo.Scenario) {
	t.Helper()
	if s.Name == "" {
		t.Fatalf("示例脚本没有名字")
	}
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("取当前目录失败：%v", err)
	}

	client, closeEnv, err := demo.Start(s, dir, nil)
	if err != nil {
		t.Fatalf("示例 %s 起不来：%v", s.Name, err)
	}
	t.Cleanup(closeEnv)

	results := demo.Execute(client, s.Steps)
	for i, result := range results {
		if result.Passed() {
			continue
		}
		t.Fatalf("示例 %s\n%s\nnova 日志：\n%s", s.Name, result.Report(i+1), novaLog(client))
	}
	if len(results) != len(s.Steps) {
		t.Fatalf("示例 %s：%d 步只跑了 %d 步", s.Name, len(s.Steps), len(results))
	}
}

// novaLog 取服务进程的输出；NoServe 的场景没有服务进程，此时给出空串。
func novaLog(client *demo.Client) string {
	if client.Stack == nil {
		return "（本示例不启动服务）"
	}
	return client.Stack.Log()
}
