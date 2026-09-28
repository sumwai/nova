package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func testChoices(labels ...string) []choice {
	choices := make([]choice, 0, len(labels))
	for _, label := range labels {
		choices = append(choices, choice{label: label, value: label})
	}
	return choices
}

// 上下键移动光标，并在两端夹住。
func TestSelectModelNavigation(t *testing.T) {
	m := newSelectModel("请选择", testChoices("a", "b", "c"))

	up := func(model selectModel) selectModel {
		updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyUp})
		return updated.(selectModel)
	}
	down := func(model selectModel) selectModel {
		updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyDown})
		return updated.(selectModel)
	}

	if got := up(m).cursor; got != 0 {
		t.Errorf("顶部再上移 cursor = %d，期望 0", got)
	}
	if got := down(down(m)).cursor; got != 2 {
		t.Errorf("下移两次 cursor = %d，期望 2", got)
	}
	if got := down(down(down(m))).cursor; got != 2 {
		t.Errorf("底部再下移 cursor = %d，期望 2", got)
	}
}

// j / k 与 Home / End 也能移动。
func TestSelectModelVimKeysAndBounds(t *testing.T) {
	m := newSelectModel("请选择", testChoices("a", "b", "c"))

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	if got := updated.(selectModel).cursor; got != 1 {
		t.Errorf("j 之后 cursor = %d，期望 1", got)
	}
	updated, _ = updated.(selectModel).Update(tea.KeyMsg{Type: tea.KeyEnd})
	if got := updated.(selectModel).cursor; got != 2 {
		t.Errorf("End 之后 cursor = %d，期望 2", got)
	}
	updated, _ = updated.(selectModel).Update(tea.KeyMsg{Type: tea.KeyHome})
	if got := updated.(selectModel).cursor; got != 0 {
		t.Errorf("Home 之后 cursor = %d，期望 0", got)
	}
}

// 回车确认、Esc 取消各自终止界面。
func TestSelectModelConfirmAndCancel(t *testing.T) {
	m := newSelectModel("请选择", testChoices("a", "b"))

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	result := updated.(selectModel)
	if !result.chosen || result.aborted {
		t.Errorf("回车后 = %+v，期望 chosen", result)
	}
	if cmd == nil {
		t.Error("回车应返回 tea.Quit")
	}

	updated, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	result = updated.(selectModel)
	if !result.aborted || result.chosen {
		t.Errorf("Esc 后 = %+v，期望 aborted", result)
	}
	if cmd == nil {
		t.Error("Esc 应返回 tea.Quit")
	}
}

// View 用 `>` 标出当前项，并带上标题。
func TestSelectModelViewMarksCursor(t *testing.T) {
	m := newSelectModel("请选择供应商", testChoices("a", "b"))
	view := m.View()
	if !strings.Contains(view, "> a") {
		t.Errorf("View 未标出当前项：\n%s", view)
	}
	if !strings.Contains(view, "请选择供应商") {
		t.Errorf("View 未包含标题：\n%s", view)
	}
}

// 列表超出窗口时围绕光标滚动，并在两端夹住。
func TestSelectRange(t *testing.T) {
	tests := []struct {
		name                string
		cursor, total, size int
		wantStart, wantEnd  int
	}{
		{"不超出窗口时全部显示", 0, 3, 10, 0, 3},
		{"在顶部时不向前滚动", 0, 20, 10, 0, 10},
		{"在底部时不留空行", 19, 20, 10, 10, 20},
		{"在中间时围绕光标", 10, 20, 10, 5, 15},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, end := selectRange(tt.cursor, tt.total, tt.size)
			if start != tt.wantStart || end != tt.wantEnd {
				t.Errorf("selectRange(%d, %d, %d) = (%d, %d)，期望 (%d, %d)",
					tt.cursor, tt.total, tt.size, start, end, tt.wantStart, tt.wantEnd)
			}
		})
	}
}

// 空候选列表时不 panic，View 仍可渲染。
func TestSelectModelEmptyChoices(t *testing.T) {
	m := newSelectModel("请选择", nil)
	if view := m.View(); !strings.Contains(view, "请选择") {
		t.Errorf("View = %q，期望仍输出标题", view)
	}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if got := updated.(selectModel).cursor; got != 0 {
		t.Errorf("空列表下移 cursor = %d，期望 0", got)
	}
}
