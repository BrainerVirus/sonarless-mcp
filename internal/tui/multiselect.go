// Package tui has the interactive pickers used by `sonarless-mcp setup`.
package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// Option is one selectable row.
type Option struct {
	ID        string
	Label     string
	Tag       string // e.g. "detected", "already configured", "not installed"
	Available bool   // "select all available" picks only these
	Selected  bool
}

// Styles are plain values in lipgloss v2: building them does not touch the
// terminal, and the program downsamples colors when it renders.
var (
	titleStyle   = lipgloss.NewStyle().Bold(true)
	cursorStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("12")).Bold(true)
	checkedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	dimStyle     = lipgloss.NewStyle().Faint(true)
	tagOKStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	tagCfgStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("14"))
)

const (
	rowAll  = -2
	rowNone = -1
)

type model struct {
	heading string
	opts    []Option
	cursor  int // 0,1 = the all/none rows; 2.. = options
	done    bool
	aborted bool
}

func (m model) Init() tea.Cmd { return nil }

func (m model) rows() int { return len(m.opts) + 2 }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	k, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return m, nil
	}
	switch k.String() {
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j", "tab":
		if m.cursor < m.rows()-1 {
			m.cursor++
		}
	case "space", "x":
		m.toggle(m.cursor - 2)
	case "a":
		m.toggle(rowAll)
	case "n":
		m.toggle(rowNone)
	case "enter":
		if m.cursor < 2 { // Enter on a shortcut row applies it first
			m.toggle(m.cursor - 2)
		}
		m.done = true
		return m, tea.Quit
	case "esc", "q", "ctrl+c":
		m.aborted = true
		return m, tea.Quit
	}
	return m, nil
}

func (m *model) toggle(row int) {
	switch row {
	case rowAll:
		for i := range m.opts {
			m.opts[i].Selected = m.opts[i].Available
		}
	case rowNone:
		for i := range m.opts {
			m.opts[i].Selected = false
		}
	default:
		m.opts[row].Selected = !m.opts[row].Selected
	}
}

func (m model) View() tea.View {
	return tea.NewView(m.render())
}

func (m model) render() string {
	if m.done || m.aborted {
		return ""
	}
	var b strings.Builder
	b.WriteString(titleStyle.Render(m.heading) + "\n\n")
	line := func(i int, text string) {
		if i == m.cursor {
			b.WriteString(cursorStyle.Render("› ") + text + "\n")
		} else {
			b.WriteString("  " + text + "\n")
		}
	}
	line(0, "Select all available")
	line(1, "Clear all")
	b.WriteString("\n")
	for i, o := range m.opts {
		box := "[ ]"
		if o.Selected {
			box = checkedStyle.Render("[x]")
		}
		tag := dimStyle.Render(" · " + o.Tag)
		switch o.Tag {
		case "detected":
			tag = tagOKStyle.Render(" · detected")
		case "already configured":
			tag = tagCfgStyle.Render(" · already configured")
		}
		line(i+2, fmt.Sprintf("%s %s%s", box, o.Label, tag))
	}
	b.WriteString("\n" + dimStyle.Render("↑/↓ move · space toggle · a all · n none · enter confirm · esc cancel") + "\n")
	return b.String()
}

// MultiSelect shows the picker and returns the chosen IDs; ok is false if
// the user cancelled.
func MultiSelect(heading string, opts []Option) (ids []string, ok bool, err error) {
	final, err := tea.NewProgram(model{heading: heading, opts: opts, cursor: 2}).Run()
	if err != nil {
		return nil, false, err
	}
	m := final.(model)
	if m.aborted {
		return nil, false, nil
	}
	for _, o := range m.opts {
		if o.Selected {
			ids = append(ids, o.ID)
		}
	}
	return ids, true, nil
}
