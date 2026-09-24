package catalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// listingResponse 是清单响应里本包用到的顶层字段。
//
// Data 用指针：区分「没有 data 字段」与「data 是空数组」。前者说明响应根本不是清单
// （或者被中间层改写成了别的 JSON），后者是一份合法的空清单，两者不该得到同一种处置。
type listingResponse struct {
	Data    *[]listingItem `json:"data"`
	HasMore bool           `json:"has_more"`
}

// listingItem 是清单里的一条模型。
//
// 两种上游形状的字段都收在这里：OpenAI 系给 object / created / owned_by，
// Anthropic 给 type / display_name / created_at。同一个 JSON 对象里能解出哪个就有哪个，
// 形状判定只用于统计与分页，字段提取一律宽容——判错不会丢字段，最多让日志里的风格标注不准。
type listingItem struct {
	ID          string `json:"id"`
	Object      string `json:"object"`
	Type        string `json:"type"`
	DisplayName string `json:"display_name"`
	OwnedBy     string `json:"owned_by"`
	Created     int64  `json:"created"`
	CreatedAt   string `json:"created_at"`
}

// parseListing 解析一份清单响应，返回归一化后的条目、判出的形状与分页标志。
func parseListing(body []byte) ([]Model, Shape, bool, error) {
	var resp listingResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, ShapeUnknown, false, fmt.Errorf("不是 JSON：%w", err)
	}
	if resp.Data == nil {
		return nil, ShapeUnknown, false, errors.New("没有 data 数组")
	}

	items := *resp.Data
	models := make([]Model, 0, len(items))
	for _, item := range items {
		id := strings.TrimSpace(item.ID)
		if id == "" {
			// 没有 id 的条目无法成为对外名，也进不了选路表。
			// 跳过而不是报错：上游多回一条残缺记录不该让整条端点的模型全部不可用。
			continue
		}
		models = append(models, Model{
			ID: id,
			// 缺省对外名即上游 id；被 expose 命中时由 Discover 改写成捕获后的名字。
			Name:        id,
			DisplayName: item.DisplayName,
			OwnedBy:     item.OwnedBy,
			CreatedAt:   itemTime(item),
		})
	}
	return models, detectShape(items), resp.HasMore, nil
}

// detectShape 判断一份清单响应的字段风格。
//
// 先看两种形状各自独有的字段（display_name / created_at 对 owned_by / created），
// 再看两者都会出现的弱特征（type / object）。一批条目里两种风格的特征同时出现时记为 unknown：
// 那种清单多半既不是 OpenAI 也不是 Anthropic，硬判一个只会让日志里的标注与实际不符。
func detectShape(items []listingItem) Shape {
	openai, anthropic := false, false
	for _, item := range items {
		switch {
		case item.DisplayName != "" || item.CreatedAt != "":
			anthropic = true
		case item.OwnedBy != "" || item.Created != 0:
			openai = true
		case item.Type == "model":
			anthropic = true
		case item.Object == "model":
			openai = true
		}
	}
	switch {
	case anthropic && !openai:
		return ShapeAnthropic
	case openai && !anthropic:
		return ShapeOpenAI
	default:
		return ShapeUnknown
	}
}

// itemTime 取条目的创建时间，两种形状都看。
//
// 解析不出时返回零值而不是 Unix 纪元：零值在内部一路表示「上游没给」，
// 只有面向客户端的渲染层才需要把它换成一个具名取值（那里的理由是可反序列化，不是事实）。
func itemTime(item listingItem) time.Time {
	if item.CreatedAt != "" {
		if parsed, err := time.Parse(time.RFC3339, item.CreatedAt); err == nil {
			return parsed
		}
	}
	if item.Created > 0 {
		return time.Unix(item.Created, 0).UTC()
	}
	return time.Time{}
}
