package config

import (
	"crypto/ed25519"
	"encoding/base64"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/sumwai/nova/internal/sourceurl"
)

// SourceKind 是订阅源的种类。
type SourceKind string

const (
	// SourceBuiltin 是随二进制发布的档案，不联网。
	// 写在 profiles 块里时按声明顺序参与覆盖；块整体省略时它是唯一缺省源。
	// 重复写 builtin 是错误：它只有一份，写两次没有可区分的含义。
	SourceBuiltin SourceKind = "builtin"

	// SourceRemote 是 https 远端源，必须带公钥验签。
	SourceRemote SourceKind = "remote"

	// SourceLocal 是本地路径源，免签。
	SourceLocal SourceKind = "local"
)

// defaultProfilesRefresh 是 profiles 块省略 refresh 时的刷新周期。
const defaultProfilesRefresh = 24 * time.Hour

// ProfileKey 是一条具名 ed25519 公钥。
//
// 名字用于日志与验签来源标识，只在所属 source 内唯一即可。
type ProfileKey struct {
	// Name 是这条公钥的名字。
	Name string

	// PublicKey 是解码后的公钥，长度固定为 ed25519.PublicKeySize。
	PublicKey ed25519.PublicKey

	// File / Line / Col 指向这条公钥的声明处，用于报错定位。
	File string
	Line int
	Col  int
}

// ProfileSource 是一个订阅源。
//
// 三种形态由 Kind 区分：builtin 不携带地址；remote 用 URL；local 用 Path
// （已经相对于写下这一行的文件解析）。
//
// 没有 revoked 列表：撤销公钥就是把对应条目从配置里删掉，而公钥只能在配置里增删，
// 索引不得自授权；另立一张撤销表不会多出任何可承载的事实，只会让「哪些公钥仍有效」
// 出现两个真值来源。
type ProfileSource struct {
	Kind SourceKind

	// URL 是远端源地址，仅 Kind 为 SourceRemote 时非空。
	URL string

	// Path 是本地源路径，仅 Kind 为 SourceLocal 时非空。
	// 相对路径已经相对于写下 source 这一行的文件解析，不相对进程工作目录。
	Path string

	// Keys 是这个源的验签公钥，按声明顺序。
	Keys []ProfileKey

	// File / Line / Col 指向这条 source 指令，用于报错定位。
	File string
	Line int
	Col  int
}

// Profiles 是顶层 profiles 块的解析结果。
//
// Sources 按书写顺序生效：同名档案后者覆盖前者，因此本地源总是赢过远端与内置源。
// 这个顺序就是覆盖顺序，下游不应重排。
type Profiles struct {
	// Sources 是全部订阅源，按声明顺序。
	Sources []ProfileSource

	// Refresh 是远端源的刷新周期，缺省 24h，只接受正时长。
	//
	// 当前只做到解析与校验：nova profiles update 是显式拉取，list 也不看它，
	// 因此这个取值暂时没有消费者。
	Refresh time.Duration
}

// DefaultProfiles 返回未写 profiles 块时的缺省值：只有随二进制发布的 builtin 源，
// 刷新周期 24h。
//
// 返回新切片：默认值会被挂到每一份未声明该块的配置上，共享底层数组会让一处修改
// 波及所有配置。
func DefaultProfiles() Profiles {
	return Profiles{
		Sources: []ProfileSource{{Kind: SourceBuiltin}},
		Refresh: defaultProfilesRefresh,
	}
}

// parseProfiles 解析一个顶层 profiles 块；返回时 p.pos 指向块后的第一行。
func (p *parser) parseProfiles(head line) error {
	if len(head.tokens) != 2 || head.tokens[1].kind != tokenBlockOpen {
		return errorf(head.file, head.tokens[0].line, head.tokens[0].col,
			"profiles 块写成 `profiles {`；现在这一行是 %q", describeLine(head))
	}
	// 顶层作用域的重复检测覆盖 profiles 本身：两个块各自声明源，合并规则无从确定。
	if err := p.rejectRepeat(head.tokens[0]); err != nil {
		return err
	}
	p.pos++

	profiles := Profiles{Refresh: defaultProfilesRefresh}
	seenSources := map[string]token{}

	// 块体内的重复检测与外层分开：source 可以写多条，refresh 只写一次。
	outerSeen := p.seen
	p.seen = map[string]token{}
	defer func() { p.seen = outerSeen }()

	for {
		if p.pos >= len(p.lines) {
			return errorf(head.file, head.tokens[0].line, head.tokens[0].col,
				"profiles 块没有收尾的 }")
		}
		ln := p.lines[p.pos]
		if isBlockClose(ln) {
			p.pos++
			break
		}
		if _, ok := blockOpener(ln); ok {
			return errorf(ln.file, ln.tokens[0].line, ln.tokens[0].col,
				"profiles 块里不能再开块")
		}
		if err := p.applyProfilesLine(ln, &profiles, seenSources); err != nil {
			return err
		}
		p.pos++
	}

	p.cfg.Profiles = profiles
	return nil
}

// applyProfilesLine 处理 profiles 块体里的一条指令。
func (p *parser) applyProfilesLine(ln line, profiles *Profiles, seenSources map[string]token) error {
	head := ln.tokens[0]
	if head.kind != tokenWord {
		return errorf(ln.file, head.line, head.col,
			"profiles 块里只能写指令；这一行以 %q 开头", head.text)
	}
	if !contains(profilesDirectives, head.text) {
		return p.unknownDirective(head, profilesDirectives)
	}

	switch head.text {
	case directiveRefresh:
		if err := p.rejectRepeat(head); err != nil {
			return err
		}
		return p.setValue(ln, func(v token) error {
			d, err := time.ParseDuration(v.text)
			if err != nil || d <= 0 {
				return errorf(ln.file, v.line, v.col,
					"刷新周期 %q 不是正的时长（形如 24h、30m）", v.text)
			}
			profiles.Refresh = d
			return nil
		})

	case directiveSource:
		return p.appendProfileSource(ln, profiles, seenSources)

	default:
		return p.unknownDirective(head, profilesDirectives)
	}
}

// appendProfileSource 解析一条 source 指令。
//
// 形态是 `source <取值> [key <名> <base64公钥>]…`：取值之后每三个记号构成一条公钥。
// 取值是 builtin、https 地址或本地路径三者之一，由取值本身判定，不另加指令。
func (p *parser) appendProfileSource(ln line, profiles *Profiles, seenSources map[string]token) error {
	head := ln.tokens[0]
	if len(ln.tokens) < 2 {
		return errorf(ln.file, head.line, valueColumn(head),
			"%s 缺少取值（形如 %s builtin、%s https://example.com/nova/profiles、%s ./profiles.local）",
			directiveSource, directiveSource, directiveSource, directiveSource)
	}
	if ln.tokens[1].kind == tokenPlaceholder {
		return errorf(ln.file, ln.tokens[1].line, ln.tokens[1].col,
			"%s 的取值不支持占位符：地址与路径在解析期就要定型", directiveSource)
	}

	value := ln.tokens[1].text
	source := ProfileSource{File: ln.file, Line: ln.no, Col: head.col}
	parsed, parseErr := url.Parse(value)
	scheme := ""
	if parseErr == nil {
		scheme = strings.ToLower(parsed.Scheme)
	}
	switch {
	case value == string(SourceBuiltin):
		source.Kind = SourceBuiltin
	case scheme == "https":
		if parsed.Host == "" {
			return errorf(ln.file, ln.tokens[1].line, ln.tokens[1].col,
				"远端源 %q 不是带主机名的 https 地址", value)
		}
		source.Kind = SourceRemote
		source.URL = value
	case scheme == "http":
		// 没有 insecure 开关：公钥校验是远端源唯一的来源保证，明文传输会让它失去意义。
		// 需要免签的来源应当放成本地路径。
		return errorf(ln.file, ln.tokens[1].line, ln.tokens[1].col,
			"远端源 %q 是明文 http；远端源只支持 https，需要免签请改用本地路径", value)
	default:
		source.Kind = SourceLocal
		source.Path = resolveSourcePath(ln.file, value)
	}

	key := profileSourceKey(source)
	if first, ok := seenSources[key]; ok {
		return errorf(ln.file, ln.tokens[1].line, ln.tokens[1].col,
			"source %s 已在 %s 第 %d 行声明过；同一个源写两次只会重复拉取，"+
				"同名档案的覆盖顺序也失去意义", value, first.file, first.line)
	}
	seenSources[key] = ln.tokens[1]

	if err := p.appendProfileKeys(ln, &source); err != nil {
		return err
	}
	profiles.Sources = append(profiles.Sources, source)
	return nil
}

// appendProfileKeys 解析 source 取值之后的公钥三元组。
//
// 公钥在加载期就解码：把非法 base64 留到验签时才发现，会让配置看起来完全正常，
// 直到第一次拉取才失败，而那时错误已经离书写处很远。
func (p *parser) appendProfileKeys(ln line, source *ProfileSource) error {
	seen := map[string]token{}
	for i := 2; i < len(ln.tokens); {
		keyword := ln.tokens[i]
		if keyword.kind != tokenWord || keyword.text != "key" {
			return errorf(ln.file, keyword.line, keyword.col,
				"source 的取值之后只接受 `key <名> <base64公钥>` 三元组，多出来的是 %q", keyword.text)
		}
		if i+2 >= len(ln.tokens) {
			return errorf(ln.file, keyword.line, valueColumn(keyword),
				"key 缺少名字或公钥（形如 key main <base64公钥>）")
		}
		nameTok := ln.tokens[i+1]
		keyTok := ln.tokens[i+2]

		if nameTok.kind != tokenWord || nameTok.text == "" {
			return errorf(ln.file, nameTok.line, nameTok.col, "key 的名字不能为空")
		}
		if first, ok := seen[nameTok.text]; ok {
			return errorf(ln.file, nameTok.line, nameTok.col,
				"key 名 %q 在这个 source 里已经在 %s 第 %d 行用过；同源内 key 名用于标识验签来源，不能重复",
				nameTok.text, first.file, first.line)
		}
		seen[nameTok.text] = nameTok

		decoded, err := base64.StdEncoding.DecodeString(keyTok.text)
		if err != nil {
			return errorf(ln.file, keyTok.line, keyTok.col,
				"key %s 的公钥 %q 不是合法的 base64：%v", nameTok.text, keyTok.text, err)
		}
		if len(decoded) != ed25519.PublicKeySize {
			return errorf(ln.file, keyTok.line, keyTok.col,
				"key %s 的公钥解码后是 %d 字节，ed25519 公钥必须是 %d 字节",
				nameTok.text, len(decoded), ed25519.PublicKeySize)
		}
		source.Keys = append(source.Keys, ProfileKey{
			Name:      nameTok.text,
			PublicKey: ed25519.PublicKey(decoded),
			File:      keyTok.file,
			Line:      keyTok.line,
			Col:       keyTok.col,
		})
		i += 3
	}
	return nil
}

// resolveSourcePath 把本地源路径解析成相对于写下这一行的文件。
//
// 与 import 同一口径：配置从别处加载时，相对路径才不会跟着进程的工作目录跑偏。
func resolveSourcePath(file, value string) string {
	if filepath.IsAbs(value) {
		return value
	}
	return filepath.Join(filepath.Dir(file), value)
}

// profileSourceKey 返回一个源的去重键。
//
// 本地路径用解析后的结果而不是原文：写法不同但指向同一个文件的路径，是同一个源。
// 远端地址用与同步层同一套归一（internal/sourceurl）：两处口径不一致会放行
// 「写两种地址的同一个源」，而它在运行时每次都会撞上 serial 回滚检查。
func profileSourceKey(source ProfileSource) string {
	switch source.Kind {
	case SourceBuiltin:
		return string(SourceBuiltin)
	case SourceRemote:
		return string(SourceRemote) + ":" + sourceurl.Normalize(source.URL)
	default:
		return string(SourceLocal) + ":" + source.Path
	}
}
