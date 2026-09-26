package profile

import (
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// currentIndexSchema 是本二进制实现的最高索引格式代数。
//
// 索引形状与档案形状各自演进，因此用单独的常量：把索引代数绑到档案代数上，
// 会让任一侧的代数单调性都失去意义。
const currentIndexSchema = 1

// Index 是远端档案源的索引：源标识、单调序号、有效期与档案清单。
//
// 索引是远端源唯一的入口：档案路径与校验值都只能由它声明，公钥不得由它携带
// （公钥只在配置里增删，索引不得自授权）。
type Index struct {
	Schema   int          `yaml:"schema"`
	Name     string       `yaml:"name"`
	Serial   int64        `yaml:"serial"`
	NotAfter string       `yaml:"not_after"`
	Profiles []IndexEntry `yaml:"profiles"`
}

// IndexEntry 是索引里的一条档案声明：id、相对路径与内容校验值。
type IndexEntry struct {
	ID     string `yaml:"id"`
	Path   string `yaml:"path"`
	SHA256 string `yaml:"sha256"`
}

// LoadIndex 解析并校验一份索引。
//
// 走与档案加载同一套机制：parseDocument 施加禁锚点、别名、多文档与重复键的约束，
// 再用嵌入的 index schema 校验形状，最后做 schema 表达不了的语义校验。报错因此与
// 档案保持一致：带文件路径与行号，可直接当跳转目标。
func LoadIndex(file string, data []byte) (*Index, *Error) {
	root, parseErr := parseDocument(file, data)
	if parseErr != nil {
		return nil, parseErr
	}
	if err := validateIndexSchema(file, root); err != nil {
		return nil, err
	}

	var loaded Index
	if err := root.Decode(&loaded); err != nil {
		return nil, &Error{File: file, Msg: fmt.Sprintf("索引解码失败：%v", err)}
	}
	if err := loaded.checkSemantics(file, root); err != nil {
		return nil, err
	}
	return &loaded, nil
}

// checkSemantics 做 schema 表达不了的索引校验。
//
// 三项都是「索引能把文件指向哪里」与「同一份索引内部是否自洽」的约束：
// 路径必须落在缓存目录以内，id 与 path 各自唯一，清单不得为空。
func (ix *Index) checkSemantics(file string, root *yaml.Node) *Error {
	if ix.Schema != currentIndexSchema {
		return pathAtNode(file, root, []string{"schema"},
			"索引格式代数 %d 不被本二进制支持（本二进制支持 %d）；索引太新时 nova 也可能忽略新代数新增的约束",
			ix.Schema, currentIndexSchema)
	}
	if len(ix.Profiles) == 0 {
		return pathAtNode(file, root, []string{"profiles"}, "索引必须至少列出一份档案")
	}

	seenID := make(map[string]int, len(ix.Profiles))
	seenPath := make(map[string]int, len(ix.Profiles))
	for i, entry := range ix.Profiles {
		where := []string{"profiles", strconv.Itoa(i)}
		pathWhere := append(append([]string{}, where...), "path")
		if err := checkIndexPath(file, root, pathWhere, entry.Path); err != nil {
			return err
		}
		// 已装快照用 *.yaml 收集档案；声明成 .yml/.json 的档案能装上却在 list 里消失，
		// 因此两处筛选口径在这里统一。
		if !strings.HasSuffix(entry.Path, ".yaml") {
			return pathAtNode(file, root, pathWhere,
				"索引里的档案路径 %q 必须以 .yaml 结尾（已装快照按 *.yaml 收集档案）", entry.Path)
		}
		if first, ok := seenID[entry.ID]; ok {
			return pathAtNode(file, root, append(append([]string{}, where...), "id"),
				"档案 id %q 重复（首次出现在第 %d 条）", entry.ID, first+1)
		}
		seenID[entry.ID] = i
		if first, ok := seenPath[entry.Path]; ok {
			return pathAtNode(file, root, pathWhere,
				"档案路径 %q 重复（首次出现在第 %d 条）", entry.Path, first+1)
		}
		seenPath[entry.Path] = i
	}
	return nil
}

// checkIndexPath 校验索引里的档案路径只能指向缓存目录以内的相对路径。
//
// 绝对路径、含 .. 的路径、带协议或主机名的 URL 都能把请求指向索引声明之外的地方：
// url.ResolveReference 对绝对引用直接返回它，因此请求会绕开 CheckRedirect 的同源
// 约束，也会绕开「远端源只支持 https」。这些都要在下载发生之前挡掉。
func checkIndexPath(file string, root *yaml.Node, where []string, value string) *Error {
	if path.IsAbs(value) || filepath.IsAbs(value) || strings.HasPrefix(value, "/") {
		return pathAtNode(file, root, where, "索引里的档案路径 %q 必须是相对路径", value)
	}
	if parsed, err := url.Parse(value); err == nil && (parsed.IsAbs() || parsed.Host != "") {
		return pathAtNode(file, root, where,
			"索引里的档案路径 %q 必须是相对路径，不能是带协议或主机名的 URL", value)
	}
	// 按 / 与 \ 两种分隔符切分：Windows 上 ..\..\x.yaml 在 path 视角只是一个普通段，
	// 但 filepath.Join 会把它当成向上两级。
	for _, segment := range strings.FieldsFunc(value, func(r rune) bool { return r == '/' || r == '\\' }) {
		if segment == ".." {
			return pathAtNode(file, root, where,
				"索引里的档案路径 %q 不得包含 ..（索引只能指向缓存目录以内的文件）", value)
		}
	}
	return nil
}
