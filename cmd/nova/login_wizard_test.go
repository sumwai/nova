package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sumwai/nova/internal/config"
)

func testProviders() []config.Provider {
	return []config.Provider{{Name: "commandcode"}, {Name: "relay"}}
}

func wizKey(kind tea.KeyType) tea.KeyMsg {
	return tea.KeyMsg{Type: kind}
}

func wizRunes(text string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(text)}
}

// stepThrough 依次喂入按键，返回最终模型。
func stepThrough(t *testing.T, m loginWizard, msgs ...tea.Msg) loginWizard {
	t.Helper()
	var model tea.Model = m
	for _, msg := range msgs {
		model, _ = model.Update(msg)
	}
	result, ok := model.(loginWizard)
	if !ok {
		t.Fatalf("模型类型 = %T，期望 loginWizard", model)
	}
	return result
}

func TestWizardInitReturnsBlink(t *testing.T) {
	if cmd := newLoginWizard(testProviders(), storeWith(t, nil)).Init(); cmd == nil {
		t.Error("Init 应返回输入框的闪烁命令")
	}
}

// 该渠道没有账号时跳过账号列表，直接进账号名输入。
func TestWizardWithoutAccountsGoesToNameStep(t *testing.T) {
	got := stepThrough(t, newLoginWizard(testProviders(), storeWith(t, nil)), wizKey(tea.KeyEnter))
	if got.step != stepName {
		t.Fatalf("step = %v，期望 stepName", got.step)
	}
	if got.namespace != "commandcode" {
		t.Errorf("namespace = %q，期望 commandcode", got.namespace)
	}
}

// 已有账号时先列账号（带脱敏密钥），选它即进入密钥输入。
func TestWizardListsAccountsAndPicksExisting(t *testing.T) {
	store := storeWith(t, map[string]map[string]string{
		"commandcode": {"default": "sk-1234567890abcdefghijklmnopqrstuvwxyz"},
	})
	got := stepThrough(t, newLoginWizard(testProviders(), store), wizKey(tea.KeyEnter))
	if got.step != stepAccount {
		t.Fatalf("step = %v，期望 stepAccount", got.step)
	}
	if view := got.View(); !strings.Contains(view, "sk-123456789****stuvwxyz") {
		t.Errorf("账号列表未显示脱敏密钥：\n%s", view)
	}

	got = stepThrough(t, got, wizKey(tea.KeyEnter))
	if got.step != stepKey || got.account != "default" {
		t.Fatalf("step = %v, account = %q，期望 stepKey/default", got.step, got.account)
	}
}

// 账号列表末尾是「添加新账号」，选它才问名字，然后收密钥。
func TestWizardAddsNewAccount(t *testing.T) {
	store := storeWith(t, map[string]map[string]string{
		"commandcode": {"default": "sk-old"},
	})
	got := stepThrough(t, newLoginWizard(testProviders(), store),
		wizKey(tea.KeyEnter), // 选 commandcode
		wizKey(tea.KeyDown),  // 移到「添加新账号」
		wizKey(tea.KeyEnter),
	)
	if got.step != stepName {
		t.Fatalf("step = %v，期望 stepName", got.step)
	}

	got = stepThrough(t, got, wizRunes("work"), wizKey(tea.KeyEnter))
	if got.step != stepKey || got.account != "work" {
		t.Fatalf("step = %v, account = %q，期望 stepKey/work", got.step, got.account)
	}

	got = stepThrough(t, got, wizRunes("sk-new"), wizKey(tea.KeyEnter))
	if got.key != "sk-new" {
		t.Errorf("key = %q，期望 sk-new", got.key)
	}
}

// 新账号名重名或非法时停在原步并给出原因。
func TestWizardRejectsBadNewName(t *testing.T) {
	store := storeWith(t, map[string]map[string]string{
		"commandcode": {"default": "sk-old"},
	})

	t.Run("重名", func(t *testing.T) {
		got := stepThrough(t, newLoginWizard(testProviders(), store),
			wizKey(tea.KeyEnter), wizKey(tea.KeyDown), wizKey(tea.KeyEnter),
			wizRunes("default"), wizKey(tea.KeyEnter),
		)
		if got.step != stepName {
			t.Fatalf("step = %v，期望停在 stepName", got.step)
		}
		if !strings.Contains(got.inputErr, "已有账号") {
			t.Errorf("inputErr = %q，期望说明重名", got.inputErr)
		}
	})

	t.Run("非法字符", func(t *testing.T) {
		got := stepThrough(t, newLoginWizard(testProviders(), store),
			wizKey(tea.KeyEnter), wizKey(tea.KeyDown), wizKey(tea.KeyEnter),
			wizRunes("bad name"), wizKey(tea.KeyEnter),
		)
		if got.step != stepName || !strings.Contains(got.inputErr, "不允许的字符") {
			t.Errorf("step = %v, inputErr = %q", got.step, got.inputErr)
		}
	})
}

// 没有 default 时账号名空回车取 default。
func TestWizardEmptyNameFallsBackToDefault(t *testing.T) {
	got := stepThrough(t, newLoginWizard(testProviders(), storeWith(t, nil)),
		wizKey(tea.KeyEnter), // 选 commandcode
		wizKey(tea.KeyEnter), // 账号名空回车
	)
	if got.step != stepKey || got.account != "default" {
		t.Fatalf("step = %v, account = %q，期望 stepKey/default", got.step, got.account)
	}
}

// 密钥为空时不停在原地并提示。
func TestWizardRejectsEmptyKey(t *testing.T) {
	got := stepThrough(t, newLoginWizard(testProviders(), storeWith(t, nil)),
		wizKey(tea.KeyEnter), wizKey(tea.KeyEnter), wizKey(tea.KeyEnter),
	)
	if got.step != stepKey {
		t.Fatalf("step = %v，期望停在 stepKey", got.step)
	}
	if got.inputErr == "" {
		t.Error("空密钥应给出提示")
	}
}

// Esc 与 Ctrl-C 都能取消。
func TestWizardCancel(t *testing.T) {
	for _, kind := range []tea.KeyType{tea.KeyEsc, tea.KeyCtrlC} {
		got := stepThrough(t, newLoginWizard(testProviders(), storeWith(t, nil)), wizKey(kind))
		if !got.aborted {
			t.Errorf("%v 应取消", kind)
		}
	}
}
