package profile

import (
	"errors"
	"fmt"

	"gopkg.in/yaml.v3"
)

// Error 是一条带位置的档案错误。
//
// Line 与 Col 都从 1 起算（与 yaml.v3 的 Node 一致），Col 按字符计数：含中文的行也能
// 报出人眼可数的列号。Line 为 0 表示这条错误不指向某一行（例如文件读不出来），
// 不是「第 0 行」。
//
// 档案错误的可定位性是底线：这类文件由人书写、由订阅源下发，一处字段写错若只报
// 「档案不合法」，使用者只能在大文件里逐行找。
type Error struct {
	File string
	Line int
	Col  int
	Msg  string

	// upToDate 表示这次「失败」其实是「远端索引不比已装的新」。
	//
	// 它默认不对外表现：报告与退出码仍按错误处理（使用者要知道本次更新被拒）；
	// 只有自动刷新这类以「要不要重载」为目的的调用方会用它区分「无需重载」与「真失败」。
	upToDate bool
}

// UpToDate 报告错误是否表示「源已是最新、无需重载」。
func UpToDate(err error) bool {
	var located *Error
	if errors.As(err, &located) {
		return located.upToDate
	}
	return false
}

// Error 按「有多少位置信息就说多少」排版，位置在前、消息在后，用冒号分隔，
// 因此编辑器与终端能直接把它当成「文件:行:列」的跳转目标。
func (e *Error) Error() string {
	switch {
	case e.File == "" && e.Line == 0:
		return e.Msg
	case e.File == "":
		return fmt.Sprintf("%d:%d: %s", e.Line, e.Col, e.Msg)
	case e.Line == 0:
		return fmt.Sprintf("%s: %s", e.File, e.Msg)
	default:
		return fmt.Sprintf("%s:%d:%d: %s", e.File, e.Line, e.Col, e.Msg)
	}
}

// errorAt 构造指向某个 YAML 节点的错误。node 为 nil 时只保留文件路径。
func errorAt(file string, node *yaml.Node, format string, args ...any) *Error {
	err := &Error{File: file, Msg: fmt.Sprintf(format, args...)}
	if node != nil {
		err.Line = node.Line
		err.Col = node.Column
	}
	return err
}
