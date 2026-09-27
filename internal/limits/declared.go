package limits

import "github.com/sumwai/nova/internal/profile"

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
}

// FromDoc 把一份额度文档的账号级条目转换成中性声明列表。
//
// 只取 Account 段：档案计划里写下的静态限制是账号级事实，模型级条目由探测或观测带入，
// 不在档案声明这一层表达。数值字段复制一份，避免调用方之间共享同一批指针。
// 文档级的 expires_at 随每条声明复制一份，还原时再由 DeclaredDoc 取回。
func FromDoc(doc *profile.LimitsDoc) []Declared {
	if doc == nil || len(doc.Account) == 0 {
		return nil
	}
	out := make([]Declared, 0, len(doc.Account))
	for _, limit := range doc.Account {
		out = append(out, Declared{
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
			ExpiresAt: doc.ExpiresAt,
		})
	}
	return out
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
		doc.Account = append(doc.Account, profile.Limit{
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
		})
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
