package tui

import (
	"reflect"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func press(m model, keys ...string) model {
	for _, k := range keys {
		var msg tea.KeyMsg
		switch k {
		case "up":
			msg = tea.KeyMsg{Type: tea.KeyUp}
		case "down":
			msg = tea.KeyMsg{Type: tea.KeyDown}
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		case "esc":
			msg = tea.KeyMsg{Type: tea.KeyEsc}
		case " ":
			msg = tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		}
		next, _ := m.Update(msg)
		m = next.(model)
	}
	return m
}

func selected(m model) []string {
	var ids []string
	for _, o := range m.opts {
		if o.Selected {
			ids = append(ids, o.ID)
		}
	}
	return ids
}

func newModel() model {
	return model{cursor: 2, opts: []Option{
		{ID: "claude", Available: true, Selected: true},
		{ID: "cursor", Available: true, Selected: true},
		{ID: "vscode", Available: false},
	}}
}

func TestToggleAndShortcuts(t *testing.T) {
	m := press(newModel(), " ") // untick claude
	if got := selected(m); !reflect.DeepEqual(got, []string{"cursor"}) {
		t.Errorf("toggle: %v", got)
	}
	m = press(m, "n")
	if got := selected(m); got != nil {
		t.Errorf("none: %v", got)
	}
	m = press(m, "a") // all AVAILABLE, not vscode
	if got := selected(m); !reflect.DeepEqual(got, []string{"claude", "cursor"}) {
		t.Errorf("all: %v", got)
	}
	m = press(m, "down", "down", " ") // tick unavailable vscode explicitly
	if got := selected(m); !reflect.DeepEqual(got, []string{"claude", "cursor", "vscode"}) {
		t.Errorf("explicit: %v", got)
	}
}

func TestEnterOnShortcutRow(t *testing.T) {
	m := press(newModel(), "up", "enter") // "Clear all" row, then confirm
	if !m.done || selected(m) != nil {
		t.Errorf("done=%v selected=%v", m.done, selected(m))
	}
}

func TestEscCancels(t *testing.T) {
	if m := press(newModel(), "esc"); !m.aborted {
		t.Error("esc did not cancel")
	}
}
