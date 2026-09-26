package gateway

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sumwai/nova/internal/config"
	"github.com/sumwai/nova/internal/domain"
	"github.com/sumwai/nova/internal/price"
)

// TestDiscoveredModelGetsPriceKey 守护发现来的模型也能被 prices.file 命中。
//
// 发现模型没有档案里的价格声明，运行时按「档案 id/模型 id」补一个键；
// 缺了这个键，prices.file 对任何走 discover 的渠道都不生效。
func TestDiscoveredModelGetsPriceKey(t *testing.T) {
	provider := &config.Provider{
		Name:    "agg",
		Profile: "vendor",
		Endpoints: []config.Endpoint{{
			URL:      "https://agg.example.com/v1/chat/completions",
			Protocol: domain.ProtocolOpenAIChat,
		}},
	}
	endpoints := []effectiveEndpoint{{
		provider: provider,
		endpoint: &provider.Endpoints[0],
		models:   []config.Model{{Name: "m", Upstream: "model-1"}},
	}}

	routes := routesByModel(endpoints)
	if len(routes["m"]) != 1 {
		t.Fatalf("候选数 = %d，期望 1", len(routes["m"]))
	}
	if got := routes["m"][0].PriceKey; got != "vendor/model-1" {
		t.Fatalf("发现模型的 priceKey = %q，期望 %q", got, "vendor/model-1")
	}
}

// TestBuildPriceTableLoadsFile 守护 prices 文件被接入价格表：文件条目与档案声明
// 合到同一张表里，查不到的键仍是 Unknown。
func TestBuildPriceTableLoadsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prices.yaml")
	content := "vendor/model:\n  currency: USD\n  input_mtok: 3\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写入价格文件失败：%v", err)
	}
	cfg := &config.Config{
		Providers: []config.Provider{pricedProvider("a", "m", "USD", 1)},
		Prices:    config.Prices{Currency: "USD", Path: path, PathDeclared: true, File: "Novafile", Line: 2, Col: 1},
	}

	table, err := buildPriceTable(cfg)
	if err != nil {
		t.Fatalf("构造价格表失败：%v", err)
	}
	if got := table.Lookup("vendor/model"); got.Kind != price.Known || got.Unit.InputMTok != 3 {
		t.Errorf("文件条目 = %+v，期望 Known 且输入 3", got)
	}
	if got := table.Lookup("p/a"); got.Kind != price.Known {
		t.Errorf("档案声明条目 = %+v，期望 Known", got)
	}
	if got := table.Lookup("nothing/here"); got.Kind != price.Unknown {
		t.Errorf("查不到的键 = %+v，期望 Unknown", got)
	}
}

// TestBuildPriceTableReportsBadFile 守护价格文件读取失败是装配失败，并指回 prices 块。
func TestBuildPriceTableReportsBadFile(t *testing.T) {
	cfg := &config.Config{
		Prices: config.Prices{
			Currency: "USD",
			Path:     filepath.Join(t.TempDir(), "absent.yaml"),
			File:     "Novafile",
			Line:     7,
			Col:      5,
		},
	}

	_, err := buildPriceTable(cfg)
	if err == nil {
		t.Fatal("价格文件不存在时应装配失败")
	}
	var cerr *config.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("错误类型 = %T，期望 *config.Error", err)
	}
	if cerr.Line != 7 || cerr.Col != 5 {
		t.Errorf("定位 = %s:%d:%d，期望指回 prices 块行", cerr.File, cerr.Line, cerr.Col)
	}
}

// TestPriceReferenceWarnings 守护悬空 price_from 在装配期被提一句并指回端点。
func TestPriceReferenceWarnings(t *testing.T) {
	cfg := &config.Config{Providers: []config.Provider{{
		Name: "vendor",
		Endpoints: []config.Endpoint{{
			File: "Novafile",
			Line: 12,
			Models: []config.Model{{
				Name:  "m",
				Price: price.Declared{Key: "vendor/m", From: "absent/key"},
			}},
		}},
	}}}
	table := price.Build([]price.Declared{{Key: "vendor/m", From: "absent/key"}}, nil, price.Options{})

	warnings := priceReferenceWarnings(cfg, table)
	if len(warnings) != 1 {
		t.Fatalf("提醒数 = %d，期望 1", len(warnings))
	}
	if warnings[0].Line != 12 || warnings[0].File != "Novafile" {
		t.Errorf("定位 = %s:%d，期望指回端点行", warnings[0].File, warnings[0].Line)
	}
	if !strings.Contains(warnings[0].Msg, "absent/key") {
		t.Errorf("提醒 = %q，期望含悬空的键", warnings[0].Msg)
	}
}

// TestPriceReferenceWarningsSilentWhenResolved 守护能解析的 price_from 不产生提醒。
func TestPriceReferenceWarningsSilentWhenResolved(t *testing.T) {
	cfg := &config.Config{Providers: []config.Provider{{
		Name: "vendor",
		Endpoints: []config.Endpoint{{
			File: "Novafile",
			Line: 12,
			Models: []config.Model{{
				Name:  "m",
				Price: price.Declared{Key: "vendor/m", From: "vendor/target"},
			}},
		}},
	}}}
	table := price.Build([]price.Declared{
		{Key: "vendor/m", From: "vendor/target"},
		{Key: "vendor/target", Unit: &price.Unit{Currency: "USD", InputMTok: 1, OutputMTok: 2}},
	}, nil, price.Options{})

	if got := priceReferenceWarnings(cfg, table); len(got) != 0 {
		t.Errorf("提醒 = %v，期望没有提醒", got)
	}
}
