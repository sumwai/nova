package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sumwai/nova/internal/config"
)

// TestValidatePriceFile 守护不联网的校验路径也读 prices 块声明的价格文件。
//
// 装配期本就要求这份文件可读，但不联网的 config check 若不读它，路径写错只能等到
// nova run 才发现——而那正是「校验通过」最容易被误读的场景。
func TestValidatePriceFile(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "prices.yaml")
	if err := os.WriteFile(good, []byte("vendor/model:\n  input_mtok: 1\n"), 0o600); err != nil {
		t.Fatalf("写入价格文件失败：%v", err)
	}

	// 没写 file 时不读磁盘：一份只用档案价格声明的配置不该被这个检查影响。
	if err := validatePriceFile(&config.Config{Prices: config.DefaultPrices()}); err != nil {
		t.Errorf("未声明 file 时报错：%v", err)
	}

	if err := validatePriceFile(&config.Config{Prices: config.Prices{
		Currency: "USD", Path: good, PathDeclared: true, File: "Novafile", Line: 3, Col: 1,
	}}); err != nil {
		t.Errorf("可读的价格文件不应报错：%v", err)
	}

	err := validatePriceFile(&config.Config{Prices: config.Prices{
		Currency: "USD", Path: filepath.Join(dir, "absent.yaml"),
		PathDeclared: true, File: "Novafile", Line: 3, Col: 1,
	}})
	if err == nil {
		t.Fatal("价格文件不存在时应报错")
	}
	var cerr *config.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("错误类型 = %T，期望 *config.Error", err)
	}
	if cerr.Line != 3 || cerr.Col != 1 {
		t.Errorf("定位 = %s:%d:%d，期望指回 prices 块行", cerr.File, cerr.Line, cerr.Col)
	}
}
