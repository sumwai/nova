package price

import (
	"fmt"
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
	var raw map[string]fileEntry
	if err := yaml.Unmarshal(data, &raw); err != nil {
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
