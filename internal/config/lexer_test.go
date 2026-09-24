package config

import (
	"strings"
	"testing"
)

// textOf 把记号序列排成便于断言的形态，位置信息另由专门的测试覆盖。
func textOf(tokens []token) []string {
	out := make([]string, len(tokens))
	for i, tk := range tokens {
		out[i] = tk.text
	}
	return out
}

func lexOK(t *testing.T, src string) []token {
	t.Helper()
	tokens, err := lex([]byte(src), "Novafile")
	if err != nil {
		t.Fatalf("lex 意外失败：%v", err)
	}
	return tokens
}

func lexErr(t *testing.T, src string) *Error {
	t.Helper()
	_, err := lex([]byte(src), "Novafile")
	if err == nil {
		t.Fatal("期望 lex 报错，但它成功了")
	}
	cerr, ok := err.(*Error)
	if !ok {
		t.Fatalf("错误类型是 %T，期望 *Error", err)
	}
	return cerr
}

func TestLexSplitsWordsPerLine(t *testing.T) {
	tokens := lexOK(t, "version 1\nlog_level debug\n")

	if got, want := textOf(tokens), []string{"version", "1", "log_level", "debug"}; !equalStrings(got, want) {
		t.Fatalf("记号 = %v，期望 %v", got, want)
	}
	if tokens[2].line != 2 || tokens[3].line != 2 {
		t.Errorf("第二行的记号行号 = %d/%d，期望都是 2", tokens[2].line, tokens[3].line)
	}
}

// 空行与纯注释行不产出任何记号：下游因此只面对「有内容的行」，
// 不必到处判断一行是不是空的。
func TestLexDropsBlankAndCommentOnlyLines(t *testing.T) {
	tokens := lexOK(t, "\n   \n# 只有注释\nlisten :8080\n")

	if got, want := textOf(tokens), []string{"listen", ":8080"}; !equalStrings(got, want) {
		t.Fatalf("记号 = %v，期望 %v", got, want)
	}
	if tokens[0].line != 4 {
		t.Errorf("行号 = %d，期望 4（前三行都不产出记号）", tokens[0].line)
	}
}

func TestLexTreatsHashAsCommentStart(t *testing.T) {
	tokens := lexOK(t, "listen :8080 # 这是注释 { }\n")

	if got, want := textOf(tokens), []string{"listen", ":8080"}; !equalStrings(got, want) {
		t.Fatalf("记号 = %v，期望 %v（注释里的花括号不该被切出来）", got, want)
	}
}

func TestLexRecognizesBracesAsOwnTokens(t *testing.T) {
	tokens := lexOK(t, "provider openai {\n}\n")

	if got, want := textOf(tokens), []string{"provider", "openai", "{", "}"}; !equalStrings(got, want) {
		t.Fatalf("记号 = %v，期望 %v", got, want)
	}
	if tokens[2].kind != tokenBlockOpen || tokens[3].kind != tokenBlockClose {
		t.Errorf("花括号的种类 = %v/%v，期望 blockOpen/blockClose", tokens[2].kind, tokens[3].kind)
	}
}

// 花括号即使紧贴取值也要切出来：否则 `foo{` 会变成一条名叫 "foo{" 的指令，
// 报错时指向一个使用者根本没写过的词。
func TestLexSplitsBraceAttachedToWord(t *testing.T) {
	tokens := lexOK(t, "endpoint chat{url /v1}\n")

	if got, want := textOf(tokens), []string{"endpoint", "chat", "{", "url", "/v1", "}"}; !equalStrings(got, want) {
		t.Fatalf("记号 = %v，期望 %v", got, want)
	}
}

func TestLexUnquotesAndUnescapes(t *testing.T) {
	tokens := lexOK(t, `admin "localhost:2026"`+"\n"+`listen "a\"b\\c"`)

	if tokens[1].text != "localhost:2026" {
		t.Errorf("去引号后的取值 = %q，期望 %q", tokens[1].text, "localhost:2026")
	}
	if want := `a"b\c`; tokens[3].text != want {
		t.Errorf("解转义后的取值 = %q，期望 %q", tokens[3].text, want)
	}
}

// 列号按 rune 计：含中文的行里，第 11 列就是人眼数到的那一列。
func TestLexCountsColumnsInRunes(t *testing.T) {
	tokens := lexOK(t, "log_level 中文取值\n")

	if tokens[1].col != 11 {
		t.Errorf("取值列号 = %d，期望 11（log_level 占 9 列，加一个空格）", tokens[1].col)
	}
}

func TestLexNormalizesCRLF(t *testing.T) {
	tokens := lexOK(t, "version 1\r\nlisten :8080\r\n")

	if got, want := textOf(tokens), []string{"version", "1", "listen", ":8080"}; !equalStrings(got, want) {
		t.Fatalf("记号 = %v，期望 %v（行尾 CR 不该混进取值）", got, want)
	}
	if tokens[2].col != 1 {
		t.Errorf("第二行首列 = %d，期望 1（CRLF 不该把列号推后）", tokens[2].col)
	}
}

func TestLexRejectsBadQuoting(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		wantMsg string
		wantCol int
	}{
		{"未闭合", `listen "abc`, "引号没有闭合", 8},
		{"不认识的转义", `listen "a\nb"`, `转义 \n 不认识`, 10},
		{"以反斜杠结尾", `listen "abc\`, "转义不完整", 8},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := lexErr(t, tt.src)
			if !strings.Contains(err.Msg, tt.wantMsg) {
				t.Errorf("消息 = %q，期望包含 %q", err.Msg, tt.wantMsg)
			}
			if err.Col != tt.wantCol {
				t.Errorf("列号 = %d，期望 %d", err.Col, tt.wantCol)
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
