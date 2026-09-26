package profile

import (
	"embed"
	"fmt"
	"io/fs"
	"strings"
)

// builtin 目录随代码进版本历史：内置档案是 nova 的基底事实，必须与二进制同版本发布，
// 因此由 go:embed 嵌入，不依赖磁盘上的文件，也不需要单独的发版流程。
//
//go:embed builtin
var builtinFS embed.FS

// Builtin 加载随二进制发布的内置档案，按文件名排序。
//
// 每份都走与使用者提供的档案同一条加载路径：parseDocument 施加解析约束，嵌入的 schema
// 校验形状，checkSemantics 做跨字段校验。内置档案写错因此会在首次调用立即暴露，
// 而不是等到某个平台出问题时才被读出来。
//
// 任一份失败即整体失败：基底没有档案可用时，后续的覆盖与按 id 引用都无从谈起。
func Builtin() ([]*Profile, error) {
	entries, err := fs.ReadDir(builtinFS, "builtin")
	if err != nil {
		return nil, fmt.Errorf("读取内置档案目录失败：%v", err)
	}

	// fs.ReadDir 已按文件名排序，加载顺序因此与文件系统枚举顺序无关。
	profiles := make([]*Profile, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		name := "builtin/" + entry.Name()
		data, readErr := builtinFS.ReadFile(name)
		if readErr != nil {
			return nil, fmt.Errorf("读取内置档案 %s 失败：%v", name, readErr)
		}
		// 加载失败的错误已带文件名与行号，直接上抛，不再套一层。
		loaded, loadErr := loadProfile(name, data)
		if loadErr != nil {
			return nil, loadErr
		}
		profiles = append(profiles, loaded)
	}
	if len(profiles) == 0 {
		return nil, fmt.Errorf("内置档案目录里没有 *.yaml")
	}
	return profiles, nil
}
