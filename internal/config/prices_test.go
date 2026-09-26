package config

import (
	"strings"
	"testing"
)

// TestParsePricesBlock 守护 prices 块的两个取值：币种与本地价格表文件。
func TestParsePricesBlock(t *testing.T) {
	cfg := mustParse(t, "version 1\nprices {\n    currency CNY\n    file ./prices.yaml\n}\n")

	if cfg.Prices.Currency != "CNY" {
		t.Errorf("币种 = %q，期望 CNY", cfg.Prices.Currency)
	}
	if !cfg.Prices.PathDeclared {
		t.Error("写了 file 应标记 PathDeclared")
	}
	if cfg.Prices.Path != "prices.yaml" {
		t.Errorf("价格文件路径 = %q，期望相对配置解析为 prices.yaml", cfg.Prices.Path)
	}
}

// 块省略时等价于 currency USD 且无 file：价格只来自档案与内置表。
func TestPricesDefaultsWhenBlockOmitted(t *testing.T) {
	cfg := mustParse(t, "version 1\n")

	if cfg.Prices.Currency != defaultPricesCurrency {
		t.Errorf("缺省币种 = %q，期望 %q", cfg.Prices.Currency, defaultPricesCurrency)
	}
	if cfg.Prices.Path != "" || cfg.Prices.PathDeclared {
		t.Errorf("缺省价格文件 = %+v，期望无文件", cfg.Prices)
	}
}

// 只写 currency 时不带文件，配置仍然合法。
func TestPricesCurrencyOnly(t *testing.T) {
	cfg := mustParse(t, "version 1\nprices {\n    currency EUR\n}\n")
	if cfg.Prices.Currency != "EUR" {
		t.Errorf("币种 = %q，期望 EUR", cfg.Prices.Currency)
	}
	if cfg.Prices.PathDeclared {
		t.Error("没写 file 不该标记 PathDeclared")
	}
}

func TestPricesErrors(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		wantMsg []string
	}{
		{
			name:    "块头缺花括号",
			src:     "prices\n",
			wantMsg: []string{"prices 块写成"},
		},
		{
			name:    "块内未知指令",
			src:     "prices {\n    scheme https\n}\n",
			wantMsg: []string{"未知指令"},
		},
		{
			name:    "币种写两次",
			src:     "prices {\n    currency USD\n    currency EUR\n}\n",
			wantMsg: []string{"currency", "不能写两次"},
		},
		{
			name:    "file 写两次",
			src:     "prices {\n    file ./a.yaml\n    file ./b.yaml\n}\n",
			wantMsg: []string{"file", "不能写两次"},
		},
		{
			name:    "币种含空白",
			src:     "prices {\n    currency \"US D\"\n}\n",
			wantMsg: []string{"不能含空白"},
		},
		{
			name:    "file 用占位符",
			src:     "prices {\n    file {env.PRICES}\n}\n",
			wantMsg: []string{"占位符"},
		},
		{
			name:    "currency 缺取值",
			src:     "prices {\n    currency\n}\n",
			wantMsg: []string{"缺少取值"},
		},
		{
			name:    "两个 prices 块",
			src:     "prices {\n    currency USD\n}\nprices {\n    currency EUR\n}\n",
			wantMsg: []string{"prices", "不能写两次"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := parseErr(t, "version 1\n"+tt.src)
			for _, want := range tt.wantMsg {
				if !strings.Contains(err.Msg, want) {
					t.Errorf("消息 = %q，期望包含 %q", err.Msg, want)
				}
			}
		})
	}
}

// prices / currency / file 都在指令清单里：二进制自己回答「认哪些写法」。
func TestPricesListedAsInstructions(t *testing.T) {
	names := InstructionNames()
	for _, want := range []string{directivePrices, directiveCurrency, directiveFile} {
		found := false
		for _, name := range names {
			if name == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("指令清单里没有 %q：%v", want, names)
		}
	}
}
