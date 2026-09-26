package price

import (
	"os"
	"path/filepath"
	"testing"
)

// declaredOf 造一条带单价的声明，省略 From。
func declaredOf(key string, input, output float64) Declared {
	return Declared{
		Key: key,
		Unit: &Unit{
			Currency:   "USD",
			InputMTok:  input,
			OutputMTok: output,
		},
	}
}

// TestThreeStatesAreDistinct 守护三态分明：显式免费、明确单价、查不到。
//
// 尤其是「查表失败不是 Free」：把没查到价格当成免费，会让未知成本的候选排到所有
// 已知价之前，这正是价格层要避免的错误。
func TestThreeStatesAreDistinct(t *testing.T) {
	table := Build([]Declared{
		{Key: "p/free", Free: true},
		declaredOf("p/known", 1, 2),
	}, nil, Options{})

	tests := []struct {
		key       string
		want      Kind
		wantAssum bool
	}{
		{"p/free", Free, false},
		{"p/known", Known, false},
		{"p/missing", Unknown, true},
	}
	for _, tt := range tests {
		entry := table.Lookup(tt.key)
		if entry.Kind != tt.want {
			t.Errorf("Lookup(%q).Kind = %v，期望 %v", tt.key, entry.Kind, tt.want)
		}
		if entry.Assumed != tt.wantAssum {
			t.Errorf("Lookup(%q).Assumed = %v，期望 %v", tt.key, entry.Assumed, tt.wantAssum)
		}
	}
}

// TestUnknownIsNeverFree 单独守护「查不到价格不等于免费」。
func TestUnknownIsNeverFree(t *testing.T) {
	table := Build(nil, nil, Options{})
	entry := table.Lookup("nothing/here")
	if entry.Kind == Free {
		t.Fatal("查表失败被当成了免费")
	}
	if entry.Kind != Unknown {
		t.Fatalf("查表失败 = %v，期望 Unknown", entry.Kind)
	}
}

// TestEmptyUnitIsUnknown 守护「全零单价不是 Known」：零价必须显式写 free。
func TestEmptyUnitIsUnknown(t *testing.T) {
	table := Build([]Declared{{
		Key:  "p/zero",
		Unit: &Unit{Currency: "USD"},
	}}, nil, Options{})
	if got := table.Lookup("p/zero").Kind; got != Unknown {
		t.Errorf("全零单价 = %v，期望 Unknown", got)
	}
}

// TestFreeEstimateIsZero 守护显式免费的成本为 0。
func TestFreeEstimateIsZero(t *testing.T) {
	table := Build([]Declared{{Key: "p/free", Free: true}}, nil, Options{})
	if got := Estimate(table.Lookup("p/free"), Usage{Input: 1000, Output: 1000}); got != 0 {
		t.Errorf("免费条目成本 = %v，期望 0", got)
	}
}

// TestEstimateUsesInputAndOutput 守护估计只用输入与输出两项，单位是每百万 token。
func TestEstimateUsesInputAndOutput(t *testing.T) {
	table := Build([]Declared{declaredOf("p/m", 2, 6)}, nil, Options{})
	entry := table.Lookup("p/m")
	got := Estimate(entry, Usage{Input: 500_000, Output: 250_000})
	// 0.5 × 2 + 0.25 × 6 = 1 + 1.5
	if got != 2.5 {
		t.Errorf("估计成本 = %v，期望 2.5", got)
	}
}

// TestUnknownUsesNominalMedian 守护 Unknown 用已知按量档的中位数参与排序。
//
// 三条已知输入单价 1 / 3 / 10，中位数是 3：未知条目的估计等于 3（按百万输入计），
// 因此它落在已知价之间，而不是被无条件排到最后。
func TestUnknownUsesNominalMedian(t *testing.T) {
	table := Build([]Declared{
		declaredOf("p/a", 1, 0),
		declaredOf("p/b", 3, 0),
		declaredOf("p/c", 10, 0),
		{Key: "p/unknown"},
	}, nil, Options{})

	usage := Usage{Input: perMillion}
	unknown := table.Lookup("p/unknown")
	if unknown.Kind != Unknown || !unknown.Assumed {
		t.Fatalf("未知条目 = %+v，期望 Unknown 且标 assumed", unknown)
	}
	if got := Estimate(unknown, usage); got != 3 {
		t.Errorf("未知条目名义成本 = %v，期望中位数 3", got)
	}
	// 名义价必须落在已知价之间：既不是最低（不是免费），也不是最高（不是排到最后）。
	knownMin := Estimate(table.Lookup("p/a"), usage)
	knownMax := Estimate(table.Lookup("p/c"), usage)
	cost := Estimate(unknown, usage)
	if cost <= knownMin || cost >= knownMax {
		t.Errorf("名义成本 %v 落在已知价 [%v, %v] 之外", cost, knownMin, knownMax)
	}
}

// TestNominalIsPerCurrency 守护名义价按币种分别取中位数：跨币种不互相参照。
func TestNominalIsPerCurrency(t *testing.T) {
	table := Build([]Declared{
		{Key: "p/usd", Unit: &Unit{Currency: "USD", InputMTok: 4}},
		{Key: "p/eur", Unit: &Unit{Currency: "EUR", InputMTok: 100}},
		{Key: "p/unknown-usd", Currency: "USD"},
		{Key: "p/unknown-eur", Currency: "EUR"},
	}, nil, Options{})

	usage := Usage{Input: perMillion}
	if got := Estimate(table.Lookup("p/unknown-usd"), usage); got != 4 {
		t.Errorf("USD 未知名义成本 = %v，期望 4", got)
	}
	if got := Estimate(table.Lookup("p/unknown-eur"), usage); got != 100 {
		t.Errorf("EUR 未知名义成本 = %v，期望 100", got)
	}
}

// TestPriceFromResolvesToAnotherEntry 守护 price_from 指向另一条声明的键。
func TestPriceFromResolvesToAnotherEntry(t *testing.T) {
	table := Build([]Declared{
		declaredOf("other/deepseek", 1, 2),
		{Key: "p/alias", From: "other/deepseek"},
	}, nil, Options{})

	entry := table.Lookup("p/alias")
	if entry.Kind != Known {
		t.Fatalf("price_from 未解析成 Known：%+v", entry)
	}
	if entry.Unit.InputMTok != 1 || entry.Unit.OutputMTok != 2 {
		t.Errorf("解析后的单价 = %+v，期望引用目标的 1/2", entry.Unit)
	}
}

// TestPriceFromResolvesIntoFile 守护 price_from 能指向本地价格表里的条目。
func TestPriceFromResolvesIntoFile(t *testing.T) {
	table := Build([]Declared{{Key: "p/alias", From: "vendor/model"}}, map[string]Unit{
		"vendor/model": {Currency: "USD", InputMTok: 7},
	}, Options{})

	entry := table.Lookup("p/alias")
	if entry.Kind != Known || entry.Unit.InputMTok != 7 {
		t.Errorf("price_from 引用文件条目 = %+v，期望 Known 且输入 7", entry)
	}
}

// TestNominalOptionOverridesMedian 守护名义价可配置：设置 Nominal 时覆盖中位数。
func TestNominalOptionOverridesMedian(t *testing.T) {
	table := Build([]Declared{
		declaredOf("p/known", 1, 0),
		{Key: "p/unknown", Currency: "USD"},
	}, nil, Options{Nominal: &Unit{Currency: "USD", InputMTok: 42}})

	if got := Estimate(table.Lookup("p/unknown"), Usage{Input: perMillion}); got != 42 {
		t.Errorf("可配置名义成本 = %v，期望 42", got)
	}
}

// TestPriceFromCycleIsUnknown 守护循环引用按查表失败处理，不会死循环。
func TestPriceFromCycleIsUnknown(t *testing.T) {
	table := Build([]Declared{
		{Key: "p/a", From: "p/b"},
		{Key: "p/b", From: "p/a"},
	}, nil, Options{})
	if got := table.Lookup("p/a").Kind; got != Unknown {
		t.Errorf("循环引用的条目 = %v，期望 Unknown", got)
	}
}

// TestDeclaredOverridesFile 守护优先级：档案声明高于 prices 文件。
func TestDeclaredOverridesFile(t *testing.T) {
	table := Build([]Declared{declaredOf("p/m", 9, 9)}, map[string]Unit{
		"p/m": {Currency: "USD", InputMTok: 1},
	}, Options{})
	entry := table.Lookup("p/m")
	if entry.Source != "profile" || entry.Unit.InputMTok != 9 {
		t.Errorf("条目 = %+v，期望档案声明胜出", entry)
	}
}

// TestFileEntryIsKnown 守护本地文件条目在没有档案声明时被采用。
func TestFileEntryIsKnown(t *testing.T) {
	table := Build(nil, map[string]Unit{
		"vendor/model": {Currency: "USD", InputMTok: 3},
	}, Options{})
	entry := table.Lookup("vendor/model")
	if entry.Kind != Known || entry.Source != "file" {
		t.Errorf("文件条目 = %+v，期望 Known 且来源为 file", entry)
	}
}

// TestLoadFile 守护价格文件的读取与「全零条目不进表」。
func TestLoadFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prices.yaml")
	content := `
vendor/model:
  currency: USD
  input_mtok: 0.28
  output_mtok: 0.42
vendor/empty:
  currency: USD
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写入价格文件失败：%v", err)
	}
	loaded, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile 报错：%v", err)
	}
	if got := loaded["vendor/model"]; got.InputMTok != 0.28 || got.OutputMTok != 0.42 {
		t.Errorf("条目 = %+v，期望 0.28/0.42", got)
	}
	if _, ok := loaded["vendor/empty"]; ok {
		t.Error("全零条目不进表：它不是一条可用的价格")
	}
}

// TestLoadFileMissingReportsError 守护读取失败必须报错，由调用方决定是否阻断装配。
func TestLoadFileMissingReportsError(t *testing.T) {
	if _, err := LoadFile(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("读取不存在的价格文件应报错")
	}
}

// TestDefaultUsage 守护排序用的固定输出估计。
func TestDefaultUsage(t *testing.T) {
	table := Build(nil, nil, Options{})
	if got := table.DefaultUsage(); got.Input != 0 || got.Output != DefaultOutputTokens {
		t.Errorf("缺省用量 = %+v，期望输入 0、输出 %d", got, DefaultOutputTokens)
	}
	configured := Build(nil, nil, Options{OutputTokens: 123})
	if got := configured.DefaultUsage().Output; got != 123 {
		t.Errorf("可配置输出估计 = %d，期望 123", got)
	}
}
