package limits

import (
	"sort"

	"github.com/sumwai/nova/internal/profile"
)

// Declared 是一条不依赖 profile 的中性额度声明。
//
// 字段与 profile.Limit 的账号级条目一一对应，但本类型不引用 profile：internal/config
// 因此可以携带它，而依赖方向保持 config → limits → profile，不成环。档案展开层用
// FromDoc 把档案里的计划限制转成本类型写入 config.Account，装配层再用 DeclaredDoc
// 还原成额度文档交给 Table.MergeDeclared。
type Declared struct {
	Kind      string
	Metric    string
	Window    string
	TZ        string
	Anchor    string
	Limit     *float64
	Used      *float64
	Remaining *float64
	ResetsAt  string
	Unbounded bool
	Assumed   bool
	Pool      string
	// ExpiresAt 是这份声明所属额度文档的到期时刻（RFC3339，带时区）；空表示不过期。
	//
	// 它是文档级字段，这里随每条声明携带一份：中性声明的载体是 config.Account.Limits，
	// 而账号与文档一一对应，逐条携带与另开一个字段承载在装配层是同一件事，
	// 后者的代价是 config.Account 多一个与 limits 平行的字段。
	ExpiresAt string
	// Model 非空时这条声明只影响该模型；空表示账号级。
	//
	// 它对应额度文档里「account 段」与「models 段」的区别；还原时由 DeclaredDoc 按它分流。
	Model string
}

// FromDoc 把一份额度文档的账号级与模型级条目转换成中性声明列表。
//
// 账号级在前、模型级按模型名排序在后，顺序确定；数值字段复制一份，避免调用方之间
// 共享同一批指针。文档级的 expires_at 随每条声明复制一份，还原时再由 DeclaredDoc 取回。
func FromDoc(doc *profile.LimitsDoc) []Declared {
	if doc == nil || (len(doc.Account) == 0 && len(doc.Models) == 0) {
		return nil
	}
	out := make([]Declared, 0, len(doc.Account))
	for _, limit := range doc.Account {
		out = append(out, declaredFrom(limit, "", doc.ExpiresAt))
	}
	for _, model := range sortedModelNames(doc.Models) {
		for _, limit := range doc.Models[model] {
			out = append(out, declaredFrom(limit, model, doc.ExpiresAt))
		}
	}
	return out
}

// declaredFrom 把一条档案条目转成中性声明；model 为空表示账号级。
func declaredFrom(limit profile.Limit, model, expiresAt string) Declared {
	return Declared{
		Kind:      limit.Kind,
		Metric:    limit.Metric,
		Window:    limit.Window,
		TZ:        limit.TZ,
		Anchor:    limit.Anchor,
		Limit:     copyFloat(limit.Limit),
		Used:      copyFloat(limit.Used),
		Remaining: copyFloat(limit.Remaining),
		ResetsAt:  limit.ResetsAt,
		Unbounded: limit.Unbounded,
		Assumed:   limit.Assumed,
		Pool:      limit.Pool,
		ExpiresAt: expiresAt,
		Model:     model,
	}
}

// sortedModelNames 按字典序返回模型名，让声明顺序确定。
func sortedModelNames(models map[string][]profile.Limit) []string {
	names := make([]string, 0, len(models))
	for name := range models {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// DeclaredDoc 把中性声明列表还原成一份账号级额度文档，供 Table.MergeDeclared 使用。
//
// 声明为空时返回 nil：装配层因此不必先判空，MergeDeclared 对 nil 的处理与「没有声明」
// 一致。数值字段同样复制一份。
func DeclaredDoc(declared []Declared) *profile.LimitsDoc {
	if len(declared) == 0 {
		return nil
	}
	doc := &profile.LimitsDoc{Schema: profile.CurrentSchema, Source: "profile"}
	for _, item := range declared {
		if doc.ExpiresAt == "" {
			doc.ExpiresAt = item.ExpiresAt
		}
		limit := profile.Limit{
			Kind:      item.Kind,
			Metric:    item.Metric,
			Window:    item.Window,
			TZ:        item.TZ,
			Anchor:    item.Anchor,
			Limit:     copyFloat(item.Limit),
			Used:      copyFloat(item.Used),
			Remaining: copyFloat(item.Remaining),
			ResetsAt:  item.ResetsAt,
			Unbounded: item.Unbounded,
			Assumed:   item.Assumed,
			Pool:      item.Pool,
		}
		if item.Model == "" {
			doc.Account = append(doc.Account, limit)
			continue
		}
		if doc.Models == nil {
			doc.Models = map[string][]profile.Limit{}
		}
		doc.Models[item.Model] = append(doc.Models[item.Model], limit)
	}
	return doc
}

// copyFloat 复制一个浮点指针，nil 原样返回。
func copyFloat(value *float64) *float64 {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}
