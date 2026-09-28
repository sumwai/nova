package main

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"github.com/sumwai/nova/internal/config"
	"github.com/sumwai/nova/internal/keystore"
)

// wizardStep 是向导当前停在哪一步。
type wizardStep int

const (
	stepProvider wizardStep = iota
	stepAccount
	stepName
	stepKey
)

// inputHelp 是输入类步骤的按键说明。
const inputHelp = "(回车确认，Esc 取消)"

// loginWizard 是 nova login 的交互向导：渠道 → 账号（或新账号名）→ 密钥。
//
// 一次界面走完全部步骤，而不是每步各起一个界面：分步起界面时上一步的输出会留在
// 屏幕上，几次之后屏幕变成一摞互相矛盾的选择记录。
type loginWizard struct {
	providers []config.Provider
	store     *keystore.Store

	step    wizardStep
	cursor  int
	entries []keystore.Entry

	input    textinput.Model
	inputErr string

	namespace string
	account   string
	key       string

	aborted bool
}

func newLoginWizard(providers []config.Provider, store *keystore.Store) loginWizard {
	input := textinput.New()
	input.CharLimit = accountNameMaxLen
	input.Width = 40

	return loginWizard{
		providers: providers,
		store:     store,
		input:     input,
	}
}

// Init 让输入框里的光标闪起来。
func (m loginWizard) Init() tea.Cmd { return textinput.Blink }

func (m loginWizard) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.String() {
		case "ctrl+c", "esc":
			m.aborted = true
			return m, tea.Quit
		}
	}

	switch m.step {
	case stepProvider:
		return m.updateProvider(msg)
	case stepAccount:
		return m.updateAccount(msg)
	default:
		return m.updateInput(msg)
	}
}

func (m loginWizard) View() string {
	var b strings.Builder
	switch m.step {
	case stepProvider:
		labels := make([]string, 0, len(m.providers))
		for _, provider := range m.providers {
			labels = append(labels, providerLabel(provider))
		}
		fmt.Fprintf(&b, "请选择供应商：\n%s\n%s", renderChoices(labels, m.cursor), selectHelp)
	case stepAccount:
		fmt.Fprintf(&b, "渠道 %s 的账号：\n%s\n%s",
			m.namespace, renderChoices(m.accountLabels(), m.cursor), selectHelp)
	case stepName:
		fmt.Fprintf(&b, "渠道 %s 的新账号名：\n%s", m.namespace, m.input.View())
		b.WriteString(m.errorAndHelp())
	case stepKey:
		fmt.Fprintf(&b, "渠道 %s 的账号 %s 的密钥（不回显）：\n%s",
			m.namespace, m.account, m.input.View())
		b.WriteString(m.errorAndHelp())
	}
	return b.String()
}

// errorAndHelp 附上输入错误与按键说明。
func (m loginWizard) errorAndHelp() string {
	var b strings.Builder
	if m.inputErr != "" {
		fmt.Fprintf(&b, "\n%s", selectedStyle.Render(m.inputErr))
	}
	fmt.Fprintf(&b, "\n%s", inputHelp)
	return b.String()
}

// accountLabels 是账号列表的显示文本：已有账号带脱敏密钥，末尾一项是「添加新账号」。
func (m loginWizard) accountLabels() []string {
	labels := make([]string, 0, len(m.entries)+1)
	for _, entry := range m.entries {
		label := entry.Name
		if masked := keystore.Mask(entry.Key); masked != "" {
			label = fmt.Sprintf("%s  %s", entry.Name, masked)
		}
		labels = append(labels, label)
	}
	return append(labels, "添加新账号")
}

func (m loginWizard) updateProvider(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	if m.moveCursor(key, len(m.providers)) {
		return m, nil
	}
	if key.String() != "enter" || len(m.providers) == 0 {
		return m, nil
	}

	m.namespace = m.providers[m.cursor].Name
	m.entries = m.store.Entries(m.namespace)
	m.cursor = 0
	if len(m.entries) == 0 {
		return m.enterNameStep(), textinput.Blink
	}
	m.step = stepAccount
	return m, nil
}

func (m loginWizard) updateAccount(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	// 候选比账号数多一项：「添加新账号」。
	total := len(m.entries) + 1
	if m.moveCursor(key, total) {
		return m, nil
	}
	if key.String() != "enter" {
		return m, nil
	}

	if m.cursor == len(m.entries) {
		return m.enterNameStep(), textinput.Blink
	}
	m.account = m.entries[m.cursor].Name
	return m.enterKeyStep(), textinput.Blink
}

func (m loginWizard) updateInput(msg tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok && key.String() == "enter" {
		if next, cmd, handled := m.submitInput(); handled {
			return next, cmd
		}
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// submitInput 处理输入步骤上的回车；返回是否消费了这次按键。
//
// 校验失败时停在原步骤并显示原因，而不是退出：账号名与密钥都是当场能改的东西，
// 为一次输错重跑整个向导并不划算。
func (m loginWizard) submitInput() (tea.Model, tea.Cmd, bool) {
	value := strings.TrimSpace(m.input.Value())
	switch m.step {
	case stepName:
		name, err := m.checkNewName(value)
		if err != nil {
			m.inputErr = err.Error()
			return m, nil, true
		}
		m.account = name
		return m.enterKeyStep(), textinput.Blink, true
	case stepKey:
		if value == "" {
			m.inputErr = "密钥不能为空"
			return m, nil, true
		}
		m.key = value
		return m, tea.Quit, true
	}
	return m, nil, false
}

// checkNewName 校验新账号名：空值在不冲突时取 default，其余按命名规则与重名检查。
func (m loginWizard) checkNewName(value string) (string, error) {
	if value == "" {
		if accountExists(m.store, m.namespace, keystore.DefaultAccount) {
			return "", fmt.Errorf("已有 %s 账号；请给一个不重名的名字", keystore.DefaultAccount)
		}
		return keystore.DefaultAccount, nil
	}
	if err := validateAccountName(value); err != nil {
		return "", err
	}
	if accountExists(m.store, m.namespace, value) {
		return "", fmt.Errorf("已有账号 %q；请从列表里选它来更新，或换一个名字", value)
	}
	return value, nil
}

// enterNameStep 切到账号名输入：default 空着时提示它，被占用时提示不能重名。
func (m loginWizard) enterNameStep() loginWizard {
	m.step = stepName
	m.inputErr = ""
	m.input.Reset()
	m.input.EchoMode = textinput.EchoNormal
	if accountExists(m.store, m.namespace, keystore.DefaultAccount) {
		m.input.Placeholder = "不能与已有账号重名"
	} else {
		m.input.Placeholder = keystore.DefaultAccount
	}
	m.input.Focus()
	return m
}

// enterKeyStep 切到密钥输入：换成不回显，并清掉上一步的内容。
func (m loginWizard) enterKeyStep() loginWizard {
	m.step = stepKey
	m.inputErr = ""
	m.input.Reset()
	m.input.EchoMode = textinput.EchoPassword
	m.input.Placeholder = ""
	m.input.Focus()
	return m
}

// moveCursor 处理上下移动，返回是否消费了这次按键。
func (m *loginWizard) moveCursor(key tea.KeyMsg, total int) bool {
	if total == 0 {
		return false
	}
	switch key.String() {
	case "up", "k", "shift+tab":
		if m.cursor > 0 {
			m.cursor--
		}
		return true
	case "down", "j", "tab":
		if m.cursor < total-1 {
			m.cursor++
		}
		return true
	case "home", "g":
		m.cursor = 0
		return true
	case "end", "G":
		m.cursor = total - 1
		return true
	}
	return false
}

// runLoginWizard 起一次向导，返回这次要写入的命名空间、账号名与密钥。
//
// 输入输出都从命令上取，而不是直接用 os.Stdin/os.Stdout，让调用方仍然掌握这两条流。
func runLoginWizard(
	cmd *cobra.Command,
	store *keystore.Store,
	providers []config.Provider,
) (loginInput, error) {
	program := tea.NewProgram(
		newLoginWizard(providers, store),
		tea.WithInput(cmd.InOrStdin()),
		tea.WithOutput(cmd.OutOrStdout()),
	)
	final, err := program.Run()
	if err != nil {
		return loginInput{}, fmt.Errorf("交互失败：%v", err)
	}
	result, ok := final.(loginWizard)
	if !ok {
		return loginInput{}, fmt.Errorf("交互失败：界面返回了意外的结果")
	}
	if result.aborted {
		return loginInput{}, fmt.Errorf("已取消")
	}
	return loginInput{
		namespace: result.namespace,
		account:   result.account,
		key:       result.key,
	}, nil
}
