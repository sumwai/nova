package profile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

// parseDocument 读入档案字节并返回其顶层映射节点。
//
// 解析约束按设计文档第四节：禁用锚点、别名、多文档与重复键，任一命中即整份拒绝。
// 这些约束必须在转换为通用值之前检查：yaml.v3 解码到 Node 时不做重复键检测，
// 而锚点与别名一旦展开，报错就再也指不回书写位置。
func parseDocument(file string, data []byte) (*yaml.Node, *Error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))

	var doc yaml.Node
	if err := decoder.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, &Error{File: file, Msg: "档案为空"}
		}
		return nil, &Error{File: file, Msg: fmt.Sprintf("档案不是合法 YAML：%v", err)}
	}

	// 再取一次：拿到第二个文档即说明是多文档流。第二次返回 EOF 才是单文档。
	var extra yaml.Node
	switch err := decoder.Decode(&extra); {
	case err == nil:
		return nil, errorAt(file, firstContent(&extra), "档案只能包含一个 YAML 文档")
	case !errors.Is(err, io.EOF):
		return nil, &Error{File: file, Msg: fmt.Sprintf("档案不是合法 YAML：%v", err)}
	}

	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, &Error{File: file, Msg: "档案缺少内容"}
	}
	root := doc.Content[0]
	if err := checkYAMLFeatures(file, root); err != nil {
		return nil, err
	}
	return root, nil
}

// firstContent 返回文档节点的内容节点，用于给「多文档」报一个具体行号。
func firstContent(node *yaml.Node) *yaml.Node {
	if node != nil && len(node.Content) > 0 {
		return node.Content[0]
	}
	return node
}

// checkYAMLFeatures 递归检查节点树，拒绝锚点、别名与重复键。
func checkYAMLFeatures(file string, node *yaml.Node) *Error {
	if node.Kind == yaml.AliasNode {
		// 防御性分支：YAML 要求锚点先于别名出现，而遍历按文档顺序，因此上方的锚点检查
		// 通常先命中；此处仍保证一旦解析出别名就拒绝，不依赖叙述顺序。
		return errorAt(file, node, "档案不得使用 YAML 别名")
	}
	if node.Anchor != "" {
		return errorAt(file, node, "档案不得使用 YAML 锚点 %q", node.Anchor)
	}
	if node.Kind == yaml.MappingNode {
		seen := make(map[string]int, len(node.Content)/2)
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i]
			if first, ok := seen[key.Value]; ok {
				return errorAt(file, key, "字段 %q 重复（首次出现在第 %d 行）", key.Value, first)
			}
			seen[key.Value] = key.Line
		}
	}
	for _, child := range node.Content {
		if err := checkYAMLFeatures(file, child); err != nil {
			return err
		}
	}
	return nil
}

// instanceFromNode 把档案节点转成 JSON schema 校验用的通用值。
//
// 先解码再经 JSON 往返，是为了让取值类型与 jsonschema 的输入约定一致（数字统一为
// json.Number），避免 int 与 float64 的差异在校验与取值两处给出不同结论。
func instanceFromNode(root *yaml.Node) (any, error) {
	var value any
	if err := root.Decode(&value); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return jsonschema.UnmarshalJSON(bytes.NewReader(raw))
}

// mappingValue 返回映射节点中某个键的值节点；键不存在或节点不是映射时返回 nil。
func mappingValue(node *yaml.Node, key string) *yaml.Node {
	node = mappingRoot(node)
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

// mappingKeyNode 返回映射节点中某个键的键节点，用于把错误指向字段名本身。
func mappingKeyNode(node *yaml.Node, key string) *yaml.Node {
	node = mappingRoot(node)
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i]
		}
	}
	return nil
}

// mappingRoot 剥掉文档层，返回可当作映射处理的节点。
func mappingRoot(node *yaml.Node) *yaml.Node {
	if node != nil && node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		return node.Content[0]
	}
	return node
}

// nodeAt 按 JSON 指针风格的路径段（对象键与数组下标）定位节点；定位不到时返回 nil。
func nodeAt(root *yaml.Node, path []string) *yaml.Node {
	node := root
	for _, segment := range path {
		node = mappingRoot(node)
		switch {
		case node == nil:
			return nil
		case node.Kind == yaml.MappingNode:
			node = mappingValue(node, segment)
		case node.Kind == yaml.SequenceNode:
			index, err := strconv.Atoi(segment)
			if err != nil || index < 0 || index >= len(node.Content) {
				return nil
			}
			node = node.Content[index]
		default:
			return nil
		}
	}
	return node
}

// pathAtNode 按路径定位并构造错误；定位不到时只保留文件路径。
func pathAtNode(file string, root *yaml.Node, path []string, format string, args ...any) *Error {
	return errorAt(file, nodeAt(root, path), format, args...)
}
