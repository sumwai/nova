package gateway

import (
	"fmt"

	"github.com/sumwai/nova/internal/config"
	"github.com/sumwai/nova/internal/price"
)

// buildPriceTable 把配置里全部渠道声明的价格与 prices 文件合成一张价格表。
//
// 声明的来源是各渠道端点上的模型：展开阶段已把档案的 price / free / price_from 写进
// config.Model.Price。文件只能由配置显式声明，读取失败即装配失败——一份写错路径的价格
// 文件若被静默跳过，prefer price 会退化成按声明顺序，而这种退化从输出上看不出来。
func buildPriceTable(cfg *config.Config) (*price.Table, error) {
	var declared []price.Declared
	for i := range cfg.Providers {
		provider := &cfg.Providers[i]
		for j := range provider.Endpoints {
			endpoint := &provider.Endpoints[j]
			for _, model := range endpoint.Models {
				if model.Price.Key == "" {
					continue
				}
				declared = append(declared, model.Price)
			}
		}
	}

	var external map[string]price.Unit
	if cfg.Prices.Path != "" {
		loaded, err := price.LoadFile(cfg.Prices.Path)
		if err != nil {
			return nil, config.PriceFileError(cfg.Prices, err)
		}
		external = loaded
	}

	return price.Build(declared, external, price.Options{Currency: cfg.Prices.Currency}), nil
}

// priceReferenceWarnings 报告 price_from 指向了不存在条目的模型。
//
// 悬空引用与「本来就没价」在查表结果上都是 Unknown，因此它不会自己浮到日志里。
// 提醒按条目去重：多份渠道引用同一个不存在的键时，把键说一遍就够，
// 但每条引用仍然指向它自己的端点，方便定位是哪个模型写错了。
func priceReferenceWarnings(cfg *config.Config, table *price.Table) []config.Warning {
	missing := map[string]bool{}
	for _, key := range table.UnresolvedReferences() {
		missing[key] = true
	}
	if len(missing) == 0 {
		return nil
	}

	seen := map[string]bool{}
	var warnings []config.Warning
	for i := range cfg.Providers {
		provider := &cfg.Providers[i]
		for j := range provider.Endpoints {
			endpoint := &provider.Endpoints[j]
			for _, model := range endpoint.Models {
				if model.Price.From == "" || !missing[model.Price.From] {
					continue
				}
				key := provider.Name + "\x00" + model.Name + "\x00" + model.Price.From
				if seen[key] {
					continue
				}
				seen[key] = true
				warnings = append(warnings, config.Warning{
					File: endpoint.File,
					Line: endpoint.Line,
					Msg: fmt.Sprintf("模型 %s 的 price_from 指向不存在的价格条目 %s；该模型按查不到价格处理（不是免费）",
						model.Name, model.Price.From),
				})
			}
		}
	}
	return warnings
}
