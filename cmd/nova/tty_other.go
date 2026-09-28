//go:build !linux

package main

import (
	"fmt"
	"os"
)

// readSecretNoEcho 在非 Linux 平台没有实现。
//
// 终端属性的取值随平台而异，本仓库只在 Linux 上发布，因此不在这里维护一份
// 无法在 CI 里验证的实现；给出明确的替代路径好过一份读得到明文的假实现。
func readSecretNoEcho(*os.File) (string, error) {
	return "", fmt.Errorf(
		"本版只在 Linux 上支持无回显读入密钥；请用 --key-stdin 或 --key-env VAR")
}
