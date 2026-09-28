package main

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
)

// selectedStyle 标出候选项里的当前行。
//
// 颜色在非终端输出上会被 lipgloss 降级成纯文本，因此同一套渲染既能上屏也能进日志。
var selectedStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))

// selectWindow 是选择列表一次显示的行数上限。
//
// 超出时列表围绕光标滚动，而不是把整屏撑满：凭据库里的账号可能很多，全量打印会把
// 光标挤出可见区域，方向键看上去就没反应了。
const selectWindow = 10

// selectModel 是一个上下键选择列表。
type selectModel struct {
	title   string
	choices []choice
	cursor  int
	chosen  bool
	aborted bool
}

func newSelectModel(title string, choices []choice) selectModel {
	return selectModel{title: title, choices: choices}
}

// Init 不产生命令：列表没有需要异步拉取的内容。
func (m selectModel) Init() tea.Cmd { return nil }

// Update 处理按键：上下移动、回车确认、Esc/Ctrl-C 取消。
func (m selectModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	switch key.String() {
	case "ctrl+c", "esc", "q":
		m.aborted = true
		return m, tea.Quit
	case "enter":
		m.chosen = true
		return m, tea.Quit
	case "up", "k", "shift+tab":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j", "tab":
		if m.cursor < len(m.choices)-1 {
			m.cursor++
		}
	case "home", "g":
		m.cursor = 0
	case "end", "G":
		if len(m.choices) > 0 {
			m.cursor = len(m.choices) - 1
		}
	}
	return m, nil
}

// View 渲染列表：光标行用 `>` 标出，其余行缩进对齐。
func (m selectModel) View() string {
	labels := make([]string, 0, len(m.choices))
	for _, item := range m.choices {
		labels = append(labels, item.label)
	}
	return fmt.Sprintf("%s：\n%s\n%s", m.title, renderChoices(labels, m.cursor), selectHelp)
}

// selectHelp 是选择类界面的按键说明。
const selectHelp = "(↑/↓ 选择，回车确认，Esc 取消)"

// renderChoices 渲染一列候选项：当前行用样式标出，超出窗口时围绕光标滚动。
func renderChoices(labels []string, cursor int) string {
	var b strings.Builder
	start, end := selectRange(cursor, len(labels), selectWindow)
	if start > 0 {
		fmt.Fprintf(&b, "  … 上面还有 %d 项\n", start)
	}
	for i := start; i < end; i++ {
		if i == cursor {
			fmt.Fprintf(&b, "%s %s\n", selectedStyle.Render(">"), selectedStyle.Render(labels[i]))
			continue
		}
		fmt.Fprintf(&b, "  %s\n", labels[i])
	}
	if end < len(labels) {
		fmt.Fprintf(&b, "  … 下面还有 %d 项\n", len(labels)-end)
	}
	return b.String()
}

// selectRange 返回应当显示的下标区间 [start, end)。
//
// 项数不超过窗口时全部显示；超出时让光标尽量居中，并在两端夹住，
// 因此列表底部与顶部都不会出现空行。
func selectRange(cursor, total, size int) (int, int) {
	if total <= size {
		return 0, total
	}
	start := cursor - size/2
	if start < 0 {
		start = 0
	}
	if start+size > total {
		start = total - size
	}
	return start, start + size
}

// promptChoiceByTUI 用上下键列表选一个候选，返回下标。
//
// 输入输出都从命令上取，而不是直接用 os.Stdin/os.Stdout：这样调用方（以及测试）
// 仍然掌握这两条流的去向。
func promptChoiceByTUI(cmd *cobra.Command, prompt string, choices []choice) (int, error) {
	program := tea.NewProgram(
		newSelectModel(prompt, choices),
		tea.WithInput(cmd.InOrStdin()),
		tea.WithOutput(cmd.OutOrStdout()),
	)
	final, err := program.Run()
	if err != nil {
		return 0, fmt.Errorf("选择失败：%v", err)
	}
	result, ok := final.(selectModel)
	if !ok {
		return 0, fmt.Errorf("选择失败：界面返回了意外的结果")
	}
	if result.aborted {
		return 0, fmt.Errorf("已取消")
	}
	return result.cursor, nil
}
