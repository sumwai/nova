package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/nova/internal/catalog"
	"github.com/sumwai/nova/internal/config"
	"github.com/sumwai/nova/internal/domain"
)

// keptN 造 N 个对外名为 m0…m(n-1) 的模型，让名单断言有个可预期的取值。
func keptN(n int) []config.Model {
	models := make([]config.Model, n)
	for i := range models {
		models[i] = config.Model{Name: fmt.Sprintf("m%d", i)}
	}
	return models
}

func TestDiscoveryLogTextReportsCounts(t *testing.T) {
	log, buf := newTestLogger(t, "info", "text")
	log.discovery(DiscoveryReport{
		Provider: "relay",
		Listing:  "https://relay.example.com/v1/models",
		Protocol: domain.ProtocolOpenAIChat,
		Shape:    catalog.ShapeOpenAI,
		Found:    128,
		Kept:     keptN(42),
		// 显式声明的模型不在清单视角里，但同样是客户端可用的名字，必须列出来。
		Declared: []config.Model{{Name: "configured", Upstream: "vendor/configured"}},
		Filtered: 86,
		Duration: 320 * time.Millisecond,
	})

	out := buf.String()
	for _, want := range []string{"catalog", "relay", "https://relay.example.com/v1/models",
		"openai", "发现 128", "保留 42", "排除 86", "320ms",
		// 名单直接列在行上，超过上限时截断并给出总数。
		"显式：configured", "保留：m0 m1", "共 42 个"} {
		if !strings.Contains(out, want) {
			t.Errorf("日志 = %q，期望含 %q", out, want)
		}
	}
	// 降级的那一句只在真的退回时出现：成功记录不该带它。
	if strings.Contains(out, "退回显式模型") {
		t.Errorf("成功的发现记录不该写降级：%q", out)
	}
}

// 发现失败记 error 级别，并写清原因与处置：这条端点在客户端看来就是少了（或没有）模型。
func TestDiscoveryLogFailureCarriesCauseAndDegradation(t *testing.T) {
	log, buf := newTestLogger(t, "info", "text")
	log.discovery(DiscoveryReport{
		Provider: "relay",
		Listing:  "https://relay.example.com/v1/models",
		Protocol: domain.ProtocolOpenAIChat,
		Err:      errors.New("上游 HTTP 状态码 503：上游维护中"),
		Degraded: true,
		endpoint: &config.Endpoint{Models: []config.Model{{Name: "a"}, {Name: "b"}}},
		Duration: 12 * time.Millisecond,
	})

	out := buf.String()
	for _, want := range []string{"ERROR", "发现失败", "503", "退回显式模型 2 个"} {
		if !strings.Contains(out, want) {
			t.Errorf("日志 = %q，期望含 %q", out, want)
		}
	}
}

// 级别过滤照常生效：log_level error 下，成功的发现记录不该输出。
func TestDiscoveryLogRespectsLevelFilter(t *testing.T) {
	log, buf := newTestLogger(t, "error", "text")
	log.discovery(DiscoveryReport{Provider: "relay", Found: 1, Kept: keptN(1)})
	if buf.Len() != 0 {
		t.Errorf("成功记录在 log_level error 下应被丢弃，实际：%q", buf.String())
	}
}

// 清单自述还有下一页时抬到 warn：缺失的模型在客户端看来就是不存在。
func TestDiscoveryLogPromotesHasMoreToWarn(t *testing.T) {
	log, buf := newTestLogger(t, "warn", "text")
	log.discovery(DiscoveryReport{Provider: "relay", Found: 100, Kept: keptN(100), HasMore: true})
	if out := buf.String(); !strings.Contains(out, "WARN") || !strings.Contains(out, "还有下一页") {
		t.Errorf("日志 = %q，期望 warn 级别且提示还有下一页", out)
	}

	// log_level error 下被丢弃：它仍然是一条成功的发现记录。
	silent, silentBuf := newTestLogger(t, "error", "text")
	silent.discovery(DiscoveryReport{Provider: "relay", Found: 100, Kept: keptN(100), HasMore: true})
	if silentBuf.Len() != 0 {
		t.Errorf("log_level error 下应被丢弃，实际：%q", silentBuf.String())
	}
}

func TestDiscoveryLogJSONCarriesFields(t *testing.T) {
	log, buf := newTestLogger(t, "info", "json")
	log.discovery(DiscoveryReport{
		Provider: "relay",
		Listing:  "https://relay.example.com/v1/models",
		Protocol: domain.ProtocolOpenAIChat,
		Shape:    catalog.ShapeOpenAI,
		Found:    10,
		Kept:     keptN(4),
		Declared: []config.Model{{Name: "configured"}},
		Filtered: 6,
		HasMore:  true,
		Duration: 1500 * time.Millisecond,
	})

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("解析失败：%v（原文 %q）", err, buf.String())
	}
	if got["msg"] != "model_discovery" {
		t.Errorf("msg = %v，期望 model_discovery", got["msg"])
	}
	if got["found"] != float64(10) || got["kept"] != float64(4) || got["filtered"] != float64(6) {
		t.Errorf("计数不对：%v / %v / %v", got["found"], got["kept"], got["filtered"])
	}
	if got["has_more"] != true {
		t.Errorf("has_more = %v，期望 true", got["has_more"])
	}
	if got["duration_ms"] != float64(1500) {
		t.Errorf("duration_ms = %v，期望 1500", got["duration_ms"])
	}
	// json 模式给全量名单：采集侧要能用它判断「模型集合变了」。
	models, ok := got["models"].([]any)
	if !ok || len(models) != 4 || models[0] != "m0" {
		t.Errorf("models = %v，期望 4 个对外名且首个是 m0", got["models"])
	}
	// 显式声明另占一个字段：它与清单保留项合起来才是客户端可用的集合。
	declared, ok := got["declared"].([]any)
	if !ok || len(declared) != 1 || declared[0] != "configured" {
		t.Errorf("declared = %v，期望一条显式声明的对外名", got["declared"])
	}
}

// 保留数不超上限时名单完整列出，不带省略提示。
func TestDiscoveryLogListsNamesWithoutTruncation(t *testing.T) {
	log, buf := newTestLogger(t, "info", "text")
	log.discovery(DiscoveryReport{Provider: "relay", Found: 2, Kept: keptN(2)})

	out := buf.String()
	if !strings.Contains(out, "保留：m0 m1") {
		t.Errorf("日志 = %q，期望列出两个对外名", out)
	}
	if strings.Contains(out, "共 ") {
		t.Errorf("日志 = %q，未超上限时不该有截断提示", out)
	}
}

// 名单用的是对外名：expose 改名后，客户端看到的才是要报出来的那个。
func TestDiscoveryLogListsExposedNames(t *testing.T) {
	log, buf := newTestLogger(t, "info", "text")
	log.discovery(DiscoveryReport{
		Provider: "cc",
		Found:    1,
		Kept:     []config.Model{{Name: "sensenova/kimi-k3", Upstream: "kimi-k3"}},
	})

	out := buf.String()
	if !strings.Contains(out, "保留：sensenova/kimi-k3") {
		t.Errorf("日志 = %q，期望名单里是对外名", out)
	}
	if strings.Contains(out, "保留：kimi-k3") {
		t.Errorf("日志 = %q，上游名不该单独出现在名单里", out)
	}
}

// 声明了发现的配置在横幅里要多出两行：清单给了多少、留下多少，以及有没有端点退回去。
func TestStartupRendersDiscoveryRows(t *testing.T) {
	cfg := &config.Config{
		Path: "/etc/nova/Novafile", Schema: 1, Listen: "127.0.0.1:8080",
		Admin: "localhost:2026", LogLevel: "info", LogFormat: "text",
	}
	out := renderStartup(cfg, catalogStats{
		DiscoveryEndpoints: 2,
		Discovered:         128,
		Kept:               42,
		Filtered:           86,
		Degraded:           1,
		Models:             44,
	}, false)

	for _, want := range []string{"模型发现", "2 条端点", "清单 128 条", "保留 42 条", "排除 86",
		"发现降级", "1 条端点发现失败", "对外模型 44 个"} {
		if !strings.Contains(out, want) {
			t.Errorf("横幅 = %q，期望含 %q", out, want)
		}
	}
}

// 不声明发现的配置与以前一字不差：那两行不出现。
func TestStartupOmitsDiscoveryRowsWithoutDiscovery(t *testing.T) {
	cfg := &config.Config{
		Path: "/etc/nova/Novafile", Schema: 1, Listen: "127.0.0.1:8080",
		Admin: "localhost:2026", LogLevel: "info", LogFormat: "text",
	}
	out := renderStartup(cfg, catalogStats{Models: 3}, false)
	if strings.Contains(out, "模型发现") || strings.Contains(out, "发现降级") {
		t.Errorf("没有发现型端点时不该写发现行：%q", out)
	}
	if !strings.Contains(out, "对外模型 3 个") {
		t.Errorf("横幅 = %q，期望报出目录大小", out)
	}
}
