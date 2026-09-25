package catalog

import "github.com/sumwai/nova/internal/pattern"

// Match 报告模型 id 是否匹配一条过滤模式。
//
// 实现已移到 internal/pattern：同一套通配语义要供模型准入、选路规则与统计过滤共用，
// 留在本包会让观测层为了一个字符串匹配去依赖模型目录。这里保留同名入口，
// 使 allow / deny / expose 的调用点与既有测试不必改动。
//
// 语义见 pattern.Match：匹配整个 id、两端隐式锚定、`*` 可跨 `/`、`?` 匹配一个字符、
// 不区分大小写。
func Match(p, id string) bool {
	return pattern.Match(p, id)
}
