package config

import (
	"os"
	"testing"
)

// writeFile 是测试里建临时配置文件的唯一出口。
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写测试文件失败：%v", err)
	}
}

func TestErrorFormatting(t *testing.T) {
	tests := []struct {
		name string
		err  Error
		want string
	}{
		{"全有", Error{File: "Novafile", Line: 3, Col: 5, Msg: "坏了"}, "Novafile:3:5: 坏了"},
		{"无文件", Error{Line: 3, Col: 5, Msg: "坏了"}, "3:5: 坏了"},
		{"无行", Error{File: "Novafile", Msg: "坏了"}, "Novafile: 坏了"},
		{"只有消息", Error{Msg: "坏了"}, "坏了"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.Error(); got != tt.want {
				t.Errorf("Error() = %q，期望 %q", got, tt.want)
			}
		})
	}
}

func TestWarningFormatting(t *testing.T) {
	withLine := Warning{File: "Novafile", Line: 7, Msg: "注意"}
	if got, want := withLine.String(), "Novafile:7: 注意"; got != want {
		t.Errorf("String() = %q，期望 %q", got, want)
	}

	// 行号为 0 表示这条提醒不指向某一行，排版时不该出现一个 "0:" 前缀。
	withoutLine := Warning{File: "Novafile", Msg: "注意"}
	if got, want := withoutLine.String(), "Novafile: 注意"; got != want {
		t.Errorf("String() = %q，期望 %q", got, want)
	}
}

func TestEditDistance(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want int
	}{
		{"相同", "listen", "listen", 0},
		{"少一个字符", "lisen", "listen", 1},
		{"换位", "log_levle", "log_level", 2},
		{"完全不同", "abc", "xyz", 3},
		{"空串对空串", "", "", 0},
		{"空串对非空", "", "abc", 3},
		// 按 rune 而不是字节：一个汉字算一次编辑，而不是三次。
		{"中日文", "配置", "配置项", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := editDistance(tt.a, tt.b); got != tt.want {
				t.Errorf("editDistance(%q, %q) = %d，期望 %d", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestClosestDirective(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"拼写接近", "lisen", "listen"},
		{"换位错误", "log_levle", "log_level"},
		{"离得太远", "totally_unrelated", ""},
		{"空串", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := closestDirective(tt.input); got != tt.want {
				t.Errorf("closestDirective(%q) = %q，期望 %q", tt.input, got, tt.want)
			}
		})
	}
}

// 候选并列时结果只由名字决定，与指令表的排列顺序无关：
// 否则调整一次表顺序就会让同一个拼写错误报出不同的建议。
func TestClosestDirectiveIsOrderIndependent(t *testing.T) {
	original := globalDirectives
	t.Cleanup(func() { globalDirectives = original })

	first := closestDirective("lisen")

	reversed := make([]string, len(original))
	for i, name := range original {
		reversed[len(original)-1-i] = name
	}
	globalDirectives = reversed

	if second := closestDirective("lisen"); second != first {
		t.Errorf("调换指令表顺序后建议从 %q 变成 %q", first, second)
	}
}

func TestValueColumnPointsPastDirectiveName(t *testing.T) {
	head := token{kind: tokenWord, text: directiveListen, line: 1, col: 1}

	// listen 占 6 列，取值该从第 8 列开始（中间一个空格）。
	if got, want := valueColumn(head), 8; got != want {
		t.Errorf("valueColumn() = %d，期望 %d", got, want)
	}
}
