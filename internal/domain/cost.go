package domain

// Cost 是一次尝试的估算成本。
//
// 它只用于统计口径（算「省了多少」），不参与任何计费与额度判定：额度层按 token 与
// 价格折算成本上界，走的是同一条价格查询但不是这个类型。
//
// 币种随成本一起携带：不同币种不可相加，统计侧按币种分组，不做换算。
type Cost struct {
	Currency string
	Amount   float64
	// Estimated 为真表示这是按价格表折算的估算值，而不是上游给出的权威金额。
	// 当前所有成本都是估算：价格来自档案或价格表，不是上游账单。
	Estimated bool
}
