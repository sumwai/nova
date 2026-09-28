package main

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sumwai/nova/internal/config"
	"github.com/sumwai/nova/internal/keystore"
)

// keystoreFileName 只是给日志与提示用的展示名；真正的位置由 keystore 包决定。
const keystoreFileName = "credentials.json"

// defaultConfigBaseDir 返回 XDG 约定下的用户配置目录（${XDG_CONFIG_HOME:-~/.config}）。
//
// 它与 defaultConfigPath 同口径：nova 自己的目录是它下面的 nova/，配置文件（Novafile）
// 与凭据库（credentials.json）都放在那里。定位不到时报错，不退回当前目录。
func defaultConfigBaseDir() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("无法确定用户配置目录（%w）", err)
	}
	return dir, nil
}

// loadKeystore 按 XDG 配置目录读凭据库。
func loadKeystore() (*keystore.Store, error) {
	dir, err := defaultConfigBaseDir()
	if err != nil {
		return nil, err
	}
	return keystore.Load(dir)
}

// credentialLookup 构造「命名空间与账号名取明文密钥」的取值函数。
//
// 供配置加载与装配注入：它把「凭据存在哪」这件事挡在配置与网关之外，
// 两者只拿到一个纯函数。
//
// 每次取值都重新读凭据库：这个函数被 nova run 的首次装配与之后每一次 nova reload
// 共用，绑定启动时的内存快照会让「先 nova login 再 nova reload」仍报账号不存在，
// 而那条报错给出的补救步骤恰恰是这次 login；同理，nova logout 之后的重载也必须
// 真的丢掉已删的密钥。凭据库是小文件，重读的开销远小于一次装配。
func credentialLookup() (func(namespace, name string) (string, bool), error) {
	dir, err := defaultConfigBaseDir()
	if err != nil {
		return nil, err
	}
	// 先读一次：凭据库损坏这类问题在启动时就该暴露，而不是推迟到第一次取值。
	if _, err := keystore.Load(dir); err != nil {
		return nil, err
	}
	return func(namespace, name string) (string, bool) {
		store, err := keystore.Load(dir)
		if err != nil {
			return "", false
		}
		return store.Lookup(namespace, name)
	}, nil
}

// loadConfigWithCredentials 读一份配置，并从本机凭据库补齐 account 指令的取值。
//
// 取值缺失时报错并指回那一行，供 nova run、nova reload 与其它需要真实凭据的调用方
// 使用。只校验写法的 nova config check、以及要在没有凭据的机器上给出报告的
// nova models 走 loadConfigForInspection（宽松 account、严格 env）；
// nova login 列渠道时两种都放宽。
func loadConfigWithCredentials(path string) (*config.Config, error) {
	lookup, err := credentialLookup()
	if err != nil {
		return nil, err
	}
	return config.LoadWith(path, config.Options{LookupAccount: lookup})
}

// loadConfigForInspection 读一份配置，只关心写法，不关心凭据是否取到值。
//
// account 引用的账号缺失时不报错：凭据库是使用者 HOME 下的状态，CI 里通常没有，
// 「语法对不对」与「登录没登录」是两件事。{env.NAME} 仍按严格处理，与既有行为一致。
func loadConfigForInspection(path string) (*config.Config, error) {
	return config.LoadWith(path, config.Options{IgnoreMissingAccount: true})
}

// loadProviders 读一份配置并返回它的渠道列表，用于登录时选择供应商。
//
// 两种缺值都放宽：登录时还没登录，配置里指向凭据库的 account 行与未导出的
// {env.NAME} 都取不到值，不该让「列出可选渠道」这一步先失败。
func loadProviders(path string) ([]config.Provider, error) {
	cfg, err := config.LoadWith(path, config.Options{
		IgnoreMissingEnv:     true,
		IgnoreMissingAccount: true,
	})
	if err != nil {
		return nil, err
	}
	return cfg.Providers, nil
}

// providerLabel 是渠道在选择列表里的显示文本：名字加它第一条端点的主机。
//
// 只给名字时，同一个平台的多条渠道会看起来完全一样；主机是使用者真正能对上号的信息。
func providerLabel(provider config.Provider) string {
	host := ""
	if len(provider.Endpoints) > 0 {
		if parsed, err := url.Parse(provider.Endpoints[0].URL); err == nil {
			host = parsed.Host
		}
	}
	if host == "" {
		return provider.Name
	}
	return fmt.Sprintf("%s (%s)", provider.Name, host)
}

// choice 是交互选择里的一个候选：显示文本与匹配用的取值。
type choice struct {
	label string
	value string
}

// promptChoice 让使用者从一组候选里选一个，返回候选的下标。
//
// 输入是终端时用上下键列表（bubbletea），不是终端时（管道、重定向、测试）退回
// 「打印编号列表再读一行」。两条路径都接受序号或候选项的取值，读不到输入、或输入
// 对不上任何候选时报错，不静默取第一个：选错供应商会把密钥存到一个用不上的命名空间下，
// 而现象要到第一次请求才会暴露。返回下标而不是取值，调用方才能同时拿到与候选并列的
// 其它信息（例如账号记录的加入时间）。
func promptChoice(cmd *cobra.Command, prompt string, choices []choice) (int, error) {
	if len(choices) == 0 {
		return 0, fmt.Errorf("%s：没有可选项", prompt)
	}
	if canUseTUI(cmd) {
		return promptChoiceByTUI(cmd, prompt, choices)
	}
	return promptChoiceByText(cmd, prompt, choices)
}

// canUseTUI 报告能否在当前输入上起一个读方向键的界面。
//
// 判据是输入为终端：bubbletea 要把它切到原始模式才收得到方向键，管道与测试里的
// strings.Reader 都不行。输出不做要求，非终端输出会被降级成普通文本。
func canUseTUI(cmd *cobra.Command) bool {
	file, ok := cmd.InOrStdin().(*os.File)
	return ok && isTerminal(file)
}

// promptChoiceByText 是选择器的非终端实现。
func promptChoiceByText(cmd *cobra.Command, prompt string, choices []choice) (int, error) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "%s：\n", prompt)
	for i, item := range choices {
		fmt.Fprintf(out, "  %d) %s\n", i+1, item.label)
	}
	fmt.Fprint(out, "> ")

	line, err := readLine(cmd)
	if err != nil {
		return 0, err
	}
	if line == "" {
		return 0, fmt.Errorf("没有选择")
	}
	if index, convErr := strconv.Atoi(line); convErr == nil {
		if index < 1 || index > len(choices) {
			return 0, fmt.Errorf("序号 %d 超出范围（1 到 %d）", index, len(choices))
		}
		return index - 1, nil
	}
	for i, item := range choices {
		if item.value == line {
			return i, nil
		}
	}
	return 0, fmt.Errorf("没有名为 %q 的可选项", line)
}

// readLine 从命令的标准输入读一行并去掉首尾空白。
//
// 逐字节读到换行为止，不用 bufio：登录会连着读「选择」与「密钥」两次输入，
// bufio 的预读会把后一行吞进它自己的缓冲区，后续再从标准输入读时拿到空。
func readLine(cmd *cobra.Command) (string, error) {
	reader := cmd.InOrStdin()
	line := make([]byte, 0, 64)
	buf := make([]byte, 1)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			if buf[0] == '\n' {
				return strings.TrimSpace(string(line)), nil
			}
			line = append(line, buf[0])
		}
		if err != nil {
			if len(line) > 0 {
				return strings.TrimSpace(string(line)), nil
			}
			return "", fmt.Errorf("没有读到输入：%v", err)
		}
	}
}

// readLoginKey 按标志选择密钥的取值来源。
//
// 三种来源互斥且都有明确的失败：两条来源标志同时给出时报错，环境变量为空时报错，
// 标准输入不是终端且没有来源标志时报错并说明可用的标志。不做任何静默兜底——
// 静默取到一个空密钥，会把问题推迟成上游的一次 401。
func readLoginKey(cmd *cobra.Command, keyStdin bool, keyEnv string) (string, error) {
	switch {
	case keyEnv != "":
		value := os.Getenv(keyEnv)
		if value == "" {
			return "", fmt.Errorf("环境变量 %s 未设置或为空", keyEnv)
		}
		return value, nil
	case keyStdin:
		data, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return "", fmt.Errorf("读取标准输入失败：%v", err)
		}
		key := strings.TrimSpace(string(data))
		if key == "" {
			return "", fmt.Errorf("标准输入里没有读到密钥")
		}
		return key, nil
	}

	input := cmd.InOrStdin()
	file, ok := input.(*os.File)
	if !ok || !isTerminal(file) {
		return "", fmt.Errorf(
			"标准输入不是终端，无法无回显读入密钥；请用 --key-stdin 或 --key-env VAR")
	}
	fmt.Fprint(cmd.ErrOrStderr(), "密钥（不回显）：")
	key, err := readSecretNoEcho(file)
	fmt.Fprintln(cmd.ErrOrStderr())
	if err != nil {
		return "", err
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "", fmt.Errorf("没有读到密钥")
	}
	return key, nil
}

// isTerminal 报告一个文件是不是终端字符设备。
//
// 只看设备类型，不看 TERM：TERM 为空只说明终端类型未知，不代表输入不是终端。
func isTerminal(file *os.File) bool {
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// formatAddedAt 把加入时间排成人读的 RFC3339 秒精度。
//
// 存储用 RFC3339Nano 保证同一秒内多次写入的顺序可判；展示只需要秒。解析不了时
// 原样返回，手工改过的凭据库不该让整份清单读不出来。
func formatAddedAt(text string) string {
	parsed, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return text
	}
	return parsed.Format(time.RFC3339)
}
