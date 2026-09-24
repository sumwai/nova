package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sumwai/nova/internal/config"
)

func TestPrintVersionHumanReadable(t *testing.T) {
	var buf bytes.Buffer
	if err := printVersion(&buf, false); err != nil {
		t.Fatalf("printVersion 报错：%v", err)
	}
	out := buf.String()

	for _, want := range []string{"nova ", "go ", "配置语法代数", "配置指令"} {
		if !strings.Contains(out, want) {
			t.Errorf("输出 = %q，期望包含 %q", out, want)
		}
	}

	// 指令清单必须与解析器同源：它若与 config 包报出的清单不一致，
	// 就等于这个二进制在对外声称自己认识一些它其实不认识（或不认识一些它其实认识）的写法。
	for _, name := range config.InstructionNames() {
		if !strings.Contains(out, name) {
			t.Errorf("输出缺少指令 %q：%q", name, out)
		}
	}
}

func TestPrintVersionJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := printVersion(&buf, true); err != nil {
		t.Fatalf("printVersion 报错：%v", err)
	}

	var report versionReport
	if err := json.Unmarshal(buf.Bytes(), &report); err != nil {
		t.Fatalf("输出不是合法 JSON：%v\n%s", err, buf.String())
	}

	if report.Version == "" {
		t.Error("JSON 里的版本号为空；它至少要退化到 0.0.0，不能是一个空字符串")
	}
	if report.GoVersion == "" {
		t.Error("JSON 里的 Go 版本为空")
	}
	if got, want := report.ConfigSchema, config.SupportedSchemas(); len(got) != len(want) {
		t.Errorf("config_schemas = %v，期望 %v", got, want)
	}
	if got, want := report.Instructions, config.InstructionNames(); len(got) != len(want) {
		t.Errorf("instructions = %v，期望 %v", got, want)
	}
}

func TestDirtyMark(t *testing.T) {
	if got := dirtyMark(false); got != "" {
		t.Errorf("干净工作区的后缀 = %q，期望空串", got)
	}
	if got := dirtyMark(true); !strings.Contains(got, "改动") {
		t.Errorf("脏工作区的后缀 = %q，期望说明工作区已改动", got)
	}
}

func TestJoinInts(t *testing.T) {
	tests := []struct {
		name string
		in   []int
		want string
	}{
		{"单个", []int{1}, "1"},
		{"多个用顿号", []int{1, 2, 3}, "1、2、3"},
		{"空列表", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := joinInts(tt.in); got != tt.want {
				t.Errorf("joinInts(%v) = %q，期望 %q", tt.in, got, tt.want)
			}
		})
	}
}
