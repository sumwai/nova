package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// importer 展开 import 指令。
type importer struct {
	// stack 是当前正在展开的文件链，用于检出循环导入。
	// 它只在一次展开过程中有意义：同一个文件在不同位置被导入两次仍会真的拼接两次，
	// 那是使用者的选择，不是错误。
	stack []string

	// warnings 收集展开期那些不阻止加载的提醒，例如 glob 没命中任何文件。
	warnings []Warning
}

// expand 把行集里的 import 行就地替换成被导入文件的内容。
//
// 替换发生在语法解析之前：被导入的内容与主配置参与的是同一次语法分析，
// 因此它里面写错的一条指令报出的位置，仍然指向真正写下它的那个文件。
func (im *importer) expand(lines []line) ([]line, error) {
	out := make([]line, 0, len(lines))
	for _, ln := range lines {
		head := ln.tokens[0]
		if head.kind != tokenWord || head.text != directiveImport {
			out = append(out, ln)
			continue
		}
		imported, err := im.expandImport(ln)
		if err != nil {
			return nil, err
		}
		out = append(out, imported...)
	}
	return out, nil
}

// expandImport 展开一条 import 指令。
func (im *importer) expandImport(ln line) ([]line, error) {
	head := ln.tokens[0]
	switch {
	case len(ln.tokens) == 1:
		return nil, errorf(ln.file, head.line, valueColumn(head),
			"import 缺少模式（形如 import provider/*）")
	case len(ln.tokens) > 2:
		extra := ln.tokens[2]
		return nil, errorf(ln.file, extra.line, extra.col,
			"import 只接受一个模式，多出来的是 %q", extra.text)
	}

	pattern := ln.tokens[1].text
	if err := checkImportPattern(ln, pattern); err != nil {
		return nil, err
	}

	// 模式相对写下这一行的文件，而不是进程的工作目录：
	// 配置从别处加载时，相对路径才不会悄悄跑偏。
	full := pattern
	if !filepath.IsAbs(pattern) {
		full = filepath.Join(filepath.Dir(ln.file), pattern)
	}

	matches, err := filepath.Glob(full)
	if err != nil {
		return nil, errorf(ln.file, ln.tokens[1].line, ln.tokens[1].col,
			"import 模式 %q 不合法：%v", pattern, err)
	}
	// 字典序固定下来：同一个对外模型名在多个被导入文件里的回退顺序，
	// 因此由文件名决定，而不是由文件系统的返回顺序决定。
	sort.Strings(matches)

	if len(matches) == 0 {
		// 拆出来的目录可能只是暂时为空，为它让网关起不来太重；
		// 但模式里没有通配符时它指向的是某个具体文件，读不到就是配置写错了。
		if !hasGlobMeta(pattern) {
			return nil, errorf(ln.file, ln.tokens[1].line, ln.tokens[1].col,
				"import %s 没有命中文件；模式里没有通配符时它必须命中一个具体文件", pattern)
		}
		im.warnings = append(im.warnings, Warning{
			File: ln.file,
			Line: ln.no,
			Msg:  fmt.Sprintf("import %s 没有命中任何文件", pattern),
		})
		return nil, nil
	}

	var out []line
	for _, path := range matches {
		imported, err := im.expandFile(path, ln)
		if err != nil {
			return nil, err
		}
		out = append(out, imported...)
	}
	return out, nil
}

// expandFile 读入一个被导入的文件，并递归展开它自己的 import。
func (im *importer) expandFile(path string, from line) ([]line, error) {
	head := from.tokens[0]
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	if index := indexOf(im.stack, abs); index >= 0 {
		cycle := append(append([]string{}, im.stack[index:]...), abs)
		return nil, errorf(from.file, head.line, head.col,
			"import 成环：%s", strings.Join(cycle, " → "))
	}

	info, err := os.Stat(path)
	if err != nil {
		return nil, errorf(from.file, head.line, head.col, "import 的 %s 读不到：%v", path, err)
	}
	if info.IsDir() {
		return nil, errorf(from.file, head.line, head.col,
			"import 的 %s 是一个目录；要连它下面的文件一起读请写成 %s/*", path, path)
	}

	src, err := os.ReadFile(path)
	if err != nil {
		return nil, errorf(from.file, head.line, head.col, "import 的 %s 读不到：%v", path, err)
	}
	tokens, err := lex(src, path)
	if err != nil {
		return nil, err
	}

	lines := groupLines(tokens)
	if len(lines) == 0 {
		// 空文件几乎总是「路径写对了但内容没写」或「导出脚本没跑」，
		// 静默拼接一片空白会让上游缺失这件事在更晚的地方才暴露。
		im.warnings = append(im.warnings, Warning{
			File: path,
			Msg:  "被 import 的文件是空的，没有拼接任何内容",
		})
	}

	im.stack = append(im.stack, abs)
	defer func() { im.stack = im.stack[:len(im.stack)-1] }()

	return im.expand(lines)
}

// checkImportPattern 校验 import 模式的形态。
//
// 只允许 * 与 ?，不接受字符组：`import provider/[a-z]*` 这类写法在 shell 里很自然，
// 但它的含义随 locale 与 shell 配置而变，而配置文件的含义不该取决于运行环境。
func checkImportPattern(ln line, pattern string) error {
	tok := ln.tokens[1]
	if pattern == "" {
		return errorf(ln.file, tok.line, tok.col, "import 的模式不能为空")
	}
	if strings.ContainsAny(pattern, "[]") {
		return errorf(ln.file, tok.line, tok.col,
			"import 模式 %q 里的字符组不支持；要读多组文件请写多行 import", pattern)
	}
	if count := strings.Count(pattern, "*") + strings.Count(pattern, "?"); count > 1 {
		return errorf(ln.file, tok.line, tok.col,
			"import 模式 %q 含 %d 个通配符，一条 import 至多一个；要多个模式请写多行 import",
			pattern, count)
	}
	return nil
}

// hasGlobMeta 报告一个模式是否含有通配符。
func hasGlobMeta(pattern string) bool {
	return strings.ContainsAny(pattern, "*?[")
}

// indexOf 返回一个字符串在切片里的下标，找不到返回 -1。
func indexOf(list []string, target string) int {
	for i, item := range list {
		if item == target {
			return i
		}
	}
	return -1
}
