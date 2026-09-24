// Package version 回答「这个二进制是谁」：它的版本号、它构建自哪个提交、
// 以及它编译时用的是哪一版 Go。
//
// 它不认识配置，也不认识命令行：把版本事实的采集与呈现分开，是为了让
// 「采集」这件事能被单元测试注入，而不必为了验证某条分支去改构建参数。
package version

import (
	"runtime"
	"runtime/debug"
)

// Version 是构建期由 -ldflags -X 注入的版本号。
//
// 它必须是包级可寻址的字符串变量：-X 对常量和拼错的符号名都是静默失效，
// 构建照样成功、退出码为 0，「以为注入了、其实没有」因此只能靠读输出发现。
//
// 缺省留空串而不是 "0.0.0"：空串就是「没有注入」这一事实本身，兜底取值交给
// Current 按构建信息推导；两处各给一个缺省会让「谁说了算」变得含糊。
var Version = ""

// shortRevisionLength 是短提交哈希的字符数，取 7 与 git 自身 abbreviate 的
// 缺省长度一致，便于把它和 `git log --oneline` 里那一列直接对照，不必换算。
const shortRevisionLength = 7

// fallbackVersion 是既拿不到注入值、也拿不到提交哈希时报出的版本号。
const fallbackVersion = "0.0.0"

// 构建信息里 vcs 相关条目的键名。它们是工具链写入的外部契约，集中列出以免散落。
const (
	vcsRevisionKey = "vcs.revision"
	vcsTimeKey     = "vcs.time"
	vcsModifiedKey = "vcs.modified"
)

// Info 是一次版本查询的完整结果。
//
// 它把「这个二进制是谁」拆成几条可各自缺失的事实，而不是预先拼成一个字符串：
// 取不到的字段一律留零值，怎么呈现交给排版层决定，采集与排版因此互不牵制。
type Info struct {
	Version    string // 版本号：注入值 > 提交短哈希 > "0.0.0"
	Revision   string // 完整提交哈希；未知为空串
	CommitTime string // 提交时间，RFC3339；未知为空串
	Modified   bool   // 构建时工作区是否脏；未知为 false
	GoVersion  string // 编译期 Go 版本
}

// Current 返回本二进制的版本事实。
func Current() Info {
	return fromBuildInfo(debug.ReadBuildInfo)
}

// fromBuildInfo 是 Current 的可测内核。
//
// readBuildInfo 作为入参注入而不是就地调用 debug.ReadBuildInfo：「注入生效」
// 「退到提交哈希」「读不到构建信息」三条分支因此都能在不改构建参数的前提下被检查，
// 否则这些分支的覆盖情况会随构建环境（是否在 git 仓库内、镜像里有没有 git）漂移。
//
// 入参为 nil 时按「读不到构建信息」处理而不 panic：调用方把「没有构建信息可读」
// 误写成传 nil 是编码失误，版本查询不该让进程崩在启动路径上。
func fromBuildInfo(readBuildInfo func() (*debug.BuildInfo, bool)) Info {
	var info *debug.BuildInfo
	if readBuildInfo != nil {
		// 第二个返回值表示构建信息是否可用；不可用时 info 为 nil，两种情况无需分别处理。
		info, _ = readBuildInfo()
	}

	result := Info{GoVersion: runtime.Version()}
	if info != nil {
		result.GoVersion = info.GoVersion
		for _, setting := range info.Settings {
			switch setting.Key {
			case vcsRevisionKey:
				result.Revision = setting.Value
			case vcsTimeKey:
				result.CommitTime = setting.Value
			case vcsModifiedKey:
				// 判据只有「取值恰好是 true」这一条：工具链写出的形态只有 true 与 false，
				// 按布尔解析或大小写不敏感匹配会认下 "TRUE" 这类没有依据的写法。
				result.Modified = setting.Value == "true"
			}
		}
	}
	// 构建信息在而 Go 版本为空时退回运行时版本：首行里 "(go )" 这个空槽没有信息量，
	// 退回运行时至少报出了当前工具链，也保证 GoVersion 永远不是一个空串。
	if result.GoVersion == "" {
		result.GoVersion = runtime.Version()
	}

	// 版本号的取值优先级：注入值 > 提交短哈希 > 字面量 0.0.0。
	// 注入值优先是因为正式构建要报的是 tag 版本，而提交哈希任何构建都有，
	// 拿它当首选会让 tag 版本永远显示不出来。
	switch {
	case Version != "":
		result.Version = Version
	case result.Revision != "":
		result.Version = shortRevision(result.Revision)
	default:
		result.Version = fallbackVersion
	}
	return result
}

// ShortRevision 把完整提交哈希截成便于阅读的短哈希。
//
// 它是「版本号兜底」与「提交行」两处共同的截断依据：各截各的会让同一份构建信息
// 报出两个不同的提交号。不足 shortRevisionLength 位时原样返回而不补零——
// 补零会造出一个仓库里根本不存在的提交号，原样返回至少还是那条被记录的事实。
func ShortRevision(revision string) string {
	if len(revision) <= shortRevisionLength {
		return revision
	}
	return revision[:shortRevisionLength]
}

func shortRevision(revision string) string { return ShortRevision(revision) }
