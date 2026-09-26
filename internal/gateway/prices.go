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
			return nil, priceFileError(cfg, err)
		}
		external = loaded
	}

	return price.Build(declared, external, price.Options{Currency: cfg.Prices.Currency}), nil
}

// priceFileError 把价格文件读取失败包成一条指回 prices 块的配置错误。
//
// 定位指向块头而不是路径那一行：块头是「这份价格从哪来」的声明处，而路径只是它的一个取值；
// 读取失败时使用者要改的往往是整块声明或文件本身，指回声明处更省一轮换算。
func priceFileError(cfg *config.Config, err error) error {
	return &config.Error{
		File: cfg.Prices.File,
		Line: cfg.Prices.Line,
		Col:  cfg.Prices.Col,
		Msg:  fmt.Sprintf("prices 块声明的价格文件 %s 无法使用：%v", cfg.Prices.Path, err),
	}
}
