package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/sumwai/nova/internal/config"
	"github.com/sumwai/nova/internal/keystore"
)

func newLoginCmd() *cobra.Command {
	var (
		configPath string
		keyStdin   bool
		keyEnv     string
	)
	cmd := &cobra.Command{
		Use:   "login [<provider>] [<账号名>]",
		Short: "把一份 API Key 存进凭据库",
		Long: `把一份 API Key 按「供应商名 + 账号名」存进凭据库，供配置里的 account 指令取用。

省略 provider 时进入一个向导：先选渠道，再选账号或「添加新账号」，最后输入密钥。
渠道的显示形如 ` + "`commandcode (api.commandcode.ai)`" + `，主机取它第一条端点。
该渠道已有账号时会列出来（带脱敏后的密钥）：选已有账号即更新它的密钥，选
「添加新账号」才问名字；没有账号时直接问名字，缺省 ` + keystore.DefaultAccount + `。
账号名只收字母、数字、下划线、连字符，且不得与已有账号重名。

输入不是终端时退回逐段提问（编号列表加账号名输入），便于脚本；也可以直接用位置参数
与取值标志跳过全部交互。

密钥有三种取法：终端上无回显读入（缺省）、--key-stdin 从标准输入读、--key-env VAR 读环境变量；
标准输入不是终端且没有给来源标志时报错，而不是静默读到一个空值。

登录流程不联网，因此不校验密钥是否有效：一次被拒绝的 401 会推迟到第一次请求。
凭据库缺省落在 ${XDG_CONFIG_HOME:-$HOME/.config}/nova/` + keystoreFileName + `。`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) > 2 {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"nova: login 至多接受两个位置参数（provider 名与账号名），多出来的是 %q\n\n", args[2])
				fmt.Fprint(cmd.ErrOrStderr(), cmd.UsageString())
				return errUsage
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if keyStdin && keyEnv != "" {
				return fmt.Errorf("--key-stdin 与 --key-env 只能选一个")
			}
			store, err := loadKeystore()
			if err != nil {
				return err
			}
			input, err := collectLogin(cmd, store, args, configPath, keyStdin, keyEnv)
			if err != nil {
				return err
			}
			return input.save(cmd, store)
		},
	}
	registerConfigFlag(cmd, &configPath)
	cmd.Flags().BoolVar(&keyStdin, "key-stdin", false, "从标准输入读入密钥")
	cmd.Flags().StringVar(&keyEnv, "key-env", "", "从指定环境变量读入密钥")
	return cmd
}

// loginInput 是一次登录要写进凭据库的三件事。
type loginInput struct {
	namespace string
	account   string
	key       string
}

// save 把这次登录写进凭据库，并把「新增」与「覆盖」分开报出。
//
// 覆盖是有反馈的：凭据被换掉却没有提示时，「改成新的却看起来没生效」与「旧密钥被
// 静默替换」都会变成没有线索的现象。
func (in loginInput) save(cmd *cobra.Command, store *keystore.Store) error {
	if err := validateAccountName(in.account); err != nil {
		return err
	}
	replacing := accountExists(store, in.namespace, in.account)
	if err := store.Set(in.namespace, in.account, in.key); err != nil {
		return err
	}
	action := "已保存"
	if replacing {
		action = "已更新"
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s %s 的账号 %s 到 %s\n",
		action, in.namespace, in.account, store.Path())
	return nil
}

// collectLogin 收集这次登录的命名空间、账号名与密钥。
//
// 交互式、输入是终端、且没给密钥来源标志时走一次向导，三步在一个界面里走完；
// 其余情形逐段收集，那条路径同时是可脚本化的。
func collectLogin(
	cmd *cobra.Command,
	store *keystore.Store,
	args []string,
	configPath string,
	keyStdin bool,
	keyEnv string,
) (loginInput, error) {
	// 向导自己收密钥，因此只在没有外部密钥来源时用它。
	if len(args) == 0 && canUseTUI(cmd) && !keyStdin && keyEnv == "" {
		providers, err := providersForLogin(configPath)
		if err != nil {
			return loginInput{}, err
		}
		return runLoginWizard(cmd, store, providers)
	}

	namespace := ""
	if len(args) >= 1 {
		namespace = args[0]
	} else {
		name, err := chooseProvider(cmd, configPath)
		if err != nil {
			return loginInput{}, err
		}
		namespace = name
	}

	account := ""
	switch {
	case len(args) == 2:
		account = args[1]
	case len(args) == 0:
		// 终端上先在已有账号与「添加新账号」之间选；不是终端时退回单纯的账号名
		// 输入，脚本与测试因此不必多喂一步选择。
		var err error
		if canUseTUI(cmd) {
			account, err = promptAccountInteractive(cmd, store, namespace)
		} else {
			account, err = promptAccountName(cmd, store, namespace)
		}
		if err != nil {
			return loginInput{}, err
		}
	default:
		var err error
		account, err = defaultAccountOrError(store, namespace)
		if err != nil {
			return loginInput{}, err
		}
	}

	key, err := readLoginKey(cmd, keyStdin, keyEnv)
	if err != nil {
		return loginInput{}, err
	}
	return loginInput{namespace: namespace, account: account, key: key}, nil
}

// providersForLogin 读配置并返回渠道列表，供向导与选择器使用。
//
// 读的是配置里已声明的渠道，而不是一份内置的供应商清单：凭据库的命名空间默认取
// 渠道名，因此「可选什么」由这份配置决定，选出来的名字一定与 account 指令对得上。
func providersForLogin(configPath string) ([]config.Provider, error) {
	path, err := resolveConfigPath(configPath, os.Getenv)
	if err != nil {
		return nil, err
	}
	providers, err := loadProviders(path)
	if err != nil {
		return nil, err
	}
	if len(providers) == 0 {
		return nil, fmt.Errorf(
			"%s 里没有任何 provider；先在配置里声明一条渠道，或写 `nova login <provider>`", path)
	}
	return providers, nil
}

// accountNameMaxLen 是账号名的字节长度上限。
//
// 名字只用于在配置里指认一份凭据，没有理由很长；给个上限是为了不让一个粘贴事故
// 把整段文字写进配置与凭据库的键。
const accountNameMaxLen = 64

// validateAccountName 校验账号名能安全地进配置语法。
//
// 账号名会出现在 account 指令里（形如 `account <命名空间>.<账号名>`），所以只收
// 字母、数字、下划线、连字符：空白与点会破坏那条指令的解析，点还会让「命名空间.账号名」
// 的拆分出现歧义，其它符号则会让同一个名字在日志、JSON 与命令行之间长出多种写法。
func validateAccountName(name string) error {
	if name == "" {
		return fmt.Errorf("账号名不能为空")
	}
	if len(name) > accountNameMaxLen {
		return fmt.Errorf("账号名过长（上限 %d 字节，现在 %d）", accountNameMaxLen, len(name))
	}
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '_' || r == '-' {
			continue
		}
		return fmt.Errorf(
			"账号名含不允许的字符 %q；只能用字母、数字、下划线、连字符", string(r))
	}
	return nil
}

// newAccountMarker 是账号选择列表里「添加新账号」那一项的取值。
//
// 它不会与真实账号名冲突：账号名来自命令行或输入，不以这个形状出现；
// 选择器同时用它作为文本回退时的匹配名。
const newAccountMarker = "+ 添加新账号"

// accountChoices 把已有账号与「添加新账号」拼成候选列表。
//
// 已有账号带脱敏后的密钥：同一渠道下可能有多个账号，只给名字时无法分辨哪个还在用。
func accountChoices(entries []keystore.Entry) []choice {
	choices := make([]choice, 0, len(entries)+1)
	for _, entry := range entries {
		label := entry.Name
		if masked := keystore.Mask(entry.Key); masked != "" {
			label = fmt.Sprintf("%s  %s", entry.Name, masked)
		}
		choices = append(choices, choice{label: label, value: entry.Name})
	}
	choices = append(choices, choice{label: "添加新账号", value: newAccountMarker})
	return choices
}

// promptAccountInteractive 让使用者在已有账号与「添加新账号」之间选一个，
// 返回这次要写入的账号名。
//
// 选已有账号就是明确要更新它的密钥，因此不再额外问一次「确定覆盖吗」；
// 正是这条路径让「换一份密钥」不必先记住账号名。
func promptAccountInteractive(
	cmd *cobra.Command,
	store *keystore.Store,
	namespace string,
) (string, error) {
	entries := store.Entries(namespace)
	if len(entries) == 0 {
		return promptNewAccountName(cmd, store, namespace)
	}

	choices := accountChoices(entries)
	index, err := promptChoice(cmd, fmt.Sprintf("渠道 %s 的账号", namespace), choices)
	if err != nil {
		return "", err
	}
	if choices[index].value == newAccountMarker {
		return promptNewAccountName(cmd, store, namespace)
	}
	return choices[index].value, nil
}

// promptNewAccountName 询问一个新账号的名字。
//
// default 还空着时直接回车即可用它；已经有 default 时不再提供这个默认值，并要求
// 名字不与已有账号重名：重名的下一步是「从列表里选它去更新」，不该在这里变成覆盖。
func promptNewAccountName(
	cmd *cobra.Command,
	store *keystore.Store,
	namespace string,
) (string, error) {
	hasDefault := accountExists(store, namespace, keystore.DefaultAccount)
	if hasDefault {
		fmt.Fprint(cmd.OutOrStdout(), "新账号名（不能与已有账号重名）：")
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "账号名 [%s]：", keystore.DefaultAccount)
	}

	line, err := readLine(cmd)
	if err != nil {
		return "", err
	}
	if line == "" {
		if hasDefault {
			return "", fmt.Errorf(
				"渠道 %s 已有 %s；请给一个不重名的账号名，或从上面的列表里选它来更新",
				namespace, keystore.DefaultAccount)
		}
		return keystore.DefaultAccount, nil
	}
	if err := validateAccountName(line); err != nil {
		return "", err
	}
	if accountExists(store, namespace, line) {
		return "", fmt.Errorf(
			"渠道 %s 已有账号 %q；要更新它请从上面的列表里选它，或换一个名字",
			namespace, line)
	}
	return line, nil
}

// promptAccountName 让使用者给这次登录起一个账号名（非终端路径）。
//
// 缺省是 default。该渠道已经有 default 时不再包含它：直接回车会被拒绝，需要显式
// 输入 default 才覆盖。静默覆盖会让上一份密钥在没有任何提示的情况下消失，而
// 「凭据换了一份却没人记得」是最难查的一类现象。
func promptAccountName(cmd *cobra.Command, store *keystore.Store, namespace string) (string, error) {
	exists := accountExists(store, namespace, keystore.DefaultAccount)
	if exists {
		fmt.Fprintf(cmd.OutOrStdout(),
			"账号名（渠道 %s 已有 %s，回车不接受；输入 %s 覆盖）：",
			namespace, keystore.DefaultAccount, keystore.DefaultAccount)
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "账号名 [%s]：", keystore.DefaultAccount)
	}

	line, err := readLine(cmd)
	if err != nil {
		return "", err
	}
	if line != "" {
		return line, nil
	}
	if exists {
		return "", fmt.Errorf(
			"渠道 %s 已有 %s 账号；要覆盖它请显式输入 %s，或换一个账号名",
			namespace, keystore.DefaultAccount, keystore.DefaultAccount)
	}
	return keystore.DefaultAccount, nil
}

// defaultAccountOrError 在没给账号名、又没法询问时决定用哪个账号。
//
// default 已经存在时报错而不是覆盖：这个分支不给使用者「看到提示再决定」的机会，
// 因此不能替他把已有的密钥换掉。
func defaultAccountOrError(store *keystore.Store, namespace string) (string, error) {
	if accountExists(store, namespace, keystore.DefaultAccount) {
		return "", fmt.Errorf(
			"渠道 %s 已有 %s 账号；要覆盖它请写 `nova login %s %s`，或换一个账号名",
			namespace, keystore.DefaultAccount, namespace, keystore.DefaultAccount)
	}
	return keystore.DefaultAccount, nil
}

// accountExists 报告某个命名空间下是否已有这个账号。
func accountExists(store *keystore.Store, namespace, name string) bool {
	_, ok := store.Lookup(namespace, name)
	return ok
}

// chooseProvider 读配置并让使用者从中选一个渠道名（非终端路径）。
func chooseProvider(cmd *cobra.Command, configPath string) (string, error) {
	providers, err := providersForLogin(configPath)
	if err != nil {
		return "", err
	}
	choices := make([]choice, 0, len(providers))
	for _, provider := range providers {
		choices = append(choices, choice{label: providerLabel(provider), value: provider.Name})
	}
	index, err := promptChoice(cmd, "请选择供应商", choices)
	if err != nil {
		return "", err
	}
	return providers[index].Name, nil
}
