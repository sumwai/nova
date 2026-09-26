package price

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"
)

// fileEntry 是价格文件里一条条目的原始形状。
//
// 币种允许省略，由 Table 的缺省币种补齐；价格用指针接收，以便区分「写了 0」与
// 「没写」——前者是一条真实价格，后者会让这条条目退化成查不到价格。
type fileEntry struct {
	Currency       string   `yaml:"currency"`
	InputMTok      *float64 `yaml:"input_mtok"`
	OutputMTok     *float64 `yaml:"output_mtok"`
	CacheReadMTok  *float64 `yaml:"cache_read_mtok"`
	CacheWriteMTok *float64 `yaml:"cache_write_mtok"`
	ReasoningMTok  *float64 `yaml:"reasoning_mtok"`
}

// LoadFile 读取一份价格文件，形如：
//
//	deepseek/deepseek-v4-flash:
//	  currency: USD
//	  input_mtok: 0.28
//	  output_mtok: 0.42
//
// 顶层是「条目键 → 单价」的映射，键与档案的 price_from 同命名空间。没有写任何价格
// 分量的条目会被跳过：它不是一条可用的价格，留在表里只会让一个全零单价看起来像免费。
func LoadFile(path string) (map[string]Unit, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取价格文件失败：%v", err)
	}
	return ParseFile(data)
}

// ParseFile 解析价格文件的字节内容；LoadFile 只负责读取。
//
// 解析是严格的：未知字段即报错。价格文件全篇都是单价数字，字段名拼错时既没有默认值
// 可退、也没有别处能补上这个信息，若静默跳过，一个少了 output_mtok 的条目会变成
// 「只有输入价」的 Known，而使用者以为自己写了两项。校验器与消费者必须用同一份解析
// 结果，因此这里不接受「先宽松解析、再人工核对字段」的做法。
//
// 同时拒绝第二份 YAML 文档：价格表是一张映射，多文档没有承载它的形状，
// 允许它只会让「只读了第一份」这件事不被察觉。
func ParseFile(data []byte) (map[string]Unit, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)

	var raw map[string]fileEntry
	if err := decoder.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			// 空文件等价于「没有任何条目」，不是解析失败：注释掉全部条目的迭代过程
			// 不该让装配在启动时才炸掉。
			return map[string]Unit{}, nil
		}
		return nil, fmt.Errorf("解析价格文件失败：%v", err)
	}
	var extra any
	switch err := decoder.Decode(&extra); {
	case err == nil:
		return nil, fmt.Errorf("解析价格文件失败：只接受一份文档（顶层是「条目键 → 单价」的映射），发现有第二份")
	case errors.Is(err, io.EOF):
	default:
		return nil, fmt.Errorf("解析价格文件失败：%v", err)
	}

	out := make(map[string]Unit, len(raw))
	for key, entry := range raw {
		unit := Unit{
			Currency: entry.Currency,
		}
		if entry.InputMTok != nil {
			unit.InputMTok = *entry.InputMTok
		}
		if entry.OutputMTok != nil {
			unit.OutputMTok = *entry.OutputMTok
		}
		if entry.CacheReadMTok != nil {
			unit.CacheReadMTok = *entry.CacheReadMTok
		}
		if entry.CacheWriteMTok != nil {
			unit.CacheWriteMTok = *entry.CacheWriteMTok
		}
		if entry.ReasoningMTok != nil {
			unit.ReasoningMTok = *entry.ReasoningMTok
		}
		if unit.empty() {
			continue
		}
		out[key] = unit
	}
	return out, nil
}
