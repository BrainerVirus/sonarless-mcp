// Package tui has the interactive pickers used by `sonarless-mcp setup`.
package tui

import (
	"fmt"
	"strings"
	"sync"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Option is one selectable row.
type Option struct {
	ID        string
	Label     string
	Tag       string // e.g. "detected", "already configured", "not installed"
	Available bool   // "select all available" picks only these
	Selected  bool
}

// styles are built on first use, not at package init: creating lipgloss
// styles makes it query the terminal (background color, cursor position),
// which every sonarless-mcp command would otherwise pay for at startup.
type styles struct{ title, cursor, checked, dim, tagOK, tagCfg lipgloss.Style }

var (
	stylesOnce sync.Once
	st         styles
)

func getStyles() styles {
	stylesOnce.Do(func() {
		st = styles{
			title:   lipgloss.NewStyle().Bold(true),
			cursor:  lipgloss.NewStyle().Foreground(lipgloss.Color("12")).Bold(true),
			checked: lipgloss.NewStyle().Foreground(lipgloss.Color("10")),
			dim:     lipgloss.NewStyle().Faint(true),
			tagOK:   lipgloss.NewStyle().Foreground(lipgloss.Color("10")),
			tagCfg:  lipgloss.NewStyle().Foreground(lipgloss.Color("14")),
		}
	})
	return st
}

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
	k, ok := msg.(tea.KeyMsg)
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
	case " ", "x":
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

func (m model) View() string {
	if m.done || m.aborted {
		return ""
	}
	st := getStyles()
	var b strings.Builder
	b.WriteString(st.title.Render(m.heading) + "\n\n")
	line := func(i int, text string) {
		if i == m.cursor {
			b.WriteString(st.cursor.Render("› ") + text + "\n")
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
			box = st.checked.Render("[x]")
		}
		tag := st.dim.Render(" · " + o.Tag)
		switch o.Tag {
		case "detected":
			tag = st.tagOK.Render(" · detected")
		case "already configured":
			tag = st.tagCfg.Render(" · already configured")
		}
		line(i+2, fmt.Sprintf("%s %s%s", box, o.Label, tag))
	}
	b.WriteString("\n" + st.dim.Render("↑/↓ move · space toggle · a all · n none · enter confirm · esc cancel") + "\n")
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
