package config

import "strings"

// defaultPricesCurrency 是 prices 块省略 currency 时的缺省币种。
const defaultPricesCurrency = "USD"

// Prices 是顶层 prices 块的解析结果。
//
// 它只描述价格层的外部事实：本地价格表在哪、用哪个币种。价格数字本身不进 Novafile，
// 要么由档案携带，要么由 Path 指向的文件提供。
type Prices struct {
	// Currency 是价格层缺省币种：档案与文件条目未写币种时用它，缺省 USD。
	// 币种只接受单一取值；跨币种在本版不换算、不排序。
	Currency string

	// Path 是价格表文件路径，已经相对于写下这一行的文件解析。
	// 空表示没有声明 file，价格只来自档案与内置表。
	Path string

	// PathDeclared 表示配置里确实写了一条 file。
	PathDeclared bool

	// File / Line / Col 指向 prices 块头，用于装配期读取文件失败时定位。
	File string
	Line int
	Col  int
}

// DefaultPrices 返回未写 prices 块时的缺省值：币种 USD，无本地文件。
func DefaultPrices() Prices {
	return Prices{Currency: defaultPricesCurrency}
}

// parsePrices 解析一个顶层 prices 块；返回时 p.pos 指向块后的第一行。
func (p *parser) parsePrices(head line) error {
	if len(head.tokens) != 2 || head.tokens[1].kind != tokenBlockOpen {
		return errorf(head.file, head.tokens[0].line, head.tokens[0].col,
			"prices 块写成 `prices {`；现在这一行是 %q", describeLine(head))
	}
	// 顶层作用域的重复检测覆盖 prices 本身：两个块各自声明币种，取值无从确定。
	if err := p.rejectRepeat(head.tokens[0]); err != nil {
		return err
	}
	p.pos++

	prices := Prices{
		Currency: defaultPricesCurrency,
		File:     head.file,
		Line:     head.no,
		Col:      head.tokens[0].col,
	}

	// 块体内的重复检测与外层分开：currency 与 file 各只写一次。
	outerSeen := p.seen
	p.seen = map[string]token{}
	defer func() { p.seen = outerSeen }()

	for {
		if p.pos >= len(p.lines) {
			return errorf(head.file, head.tokens[0].line, head.tokens[0].col,
				"prices 块没有收尾的 }")
		}
		ln := p.lines[p.pos]
		if isBlockClose(ln) {
			p.pos++
			break
		}
		if _, ok := blockOpener(ln); ok {
			return errorf(ln.file, ln.tokens[0].line, ln.tokens[0].col,
				"prices 块里不能再开块")
		}
		if err := p.applyPricesLine(ln, &prices); err != nil {
			return err
		}
		p.pos++
	}

	p.cfg.Prices = prices
	return nil
}

// applyPricesLine 处理 prices 块体里的一条指令。
func (p *parser) applyPricesLine(ln line, prices *Prices) error {
	head := ln.tokens[0]
	if head.kind != tokenWord {
		return errorf(ln.file, head.line, head.col,
			"prices 块里只能写指令；这一行以 %q 开头", head.text)
	}
	if !contains(pricesDirectives, head.text) {
		return p.unknownDirective(head, pricesDirectives)
	}
	if err := p.rejectRepeat(head); err != nil {
		return err
	}

	switch head.text {
	case directiveCurrency:
		return p.setValue(ln, func(v token) error {
			value, err := p.expand(v, ln)
			if err != nil {
				return err
			}
			if value == "" || strings.ContainsAny(value, " \t") {
				return errorf(ln.file, v.line, v.col,
					"币种 %q 不能为空、也不能含空白；它只接受单一取值", value)
			}
			prices.Currency = value
			return nil
		})

	case directiveFile:
		return p.setValue(ln, func(v token) error {
			if v.kind == tokenPlaceholder {
				return errorf(ln.file, v.line, v.col,
					"%s 的取值不支持占位符：路径在解析期就要定型", directiveFile)
			}
			value, err := p.expand(v, ln)
			if err != nil {
				return err
			}
			if value == "" {
				return errorf(ln.file, v.line, v.col, "%s 的取值不能为空", directiveFile)
			}
			prices.Path = resolveSourcePath(ln.file, value)
			prices.PathDeclared = true
			return nil
		})

	default:
		return p.unknownDirective(head, pricesDirectives)
	}
}
