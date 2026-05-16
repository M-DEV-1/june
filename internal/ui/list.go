package ui

import (
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type commandItem struct {
	title, desc string
}

func (c commandItem) Title() string       { return c.title }
func (c commandItem) Description() string { return c.desc }
func (c commandItem) FilterValue() string { return c.title }

type commandDelegate struct {
	styles Styles
}

func (d commandDelegate) Height() int                             { return 1 }
func (d commandDelegate) Spacing() int                            { return 0 }
func (d commandDelegate) Update(msg tea.Msg, m *list.Model) tea.Cmd { return nil }
func (d commandDelegate) Render(w io.Writer, m list.Model, index int, listItem list.Item) {
	i, ok := listItem.(commandItem)
	if !ok {
		return
	}

	titleStr := i.Title()
	descStr := i.Description()

	var fn func(string) string
	if index == m.Index() {
		// Highlighted item
		fn = func(s string) string {
			title := lipgloss.NewStyle().Width(18).Render(titleStr)
			desc := lipgloss.NewStyle().Render(descStr)
			row := "  " + title + desc
			return lipgloss.NewStyle().Background(lipgloss.Color("#6d28d9")).Foreground(lipgloss.Color("#ffffff")).Width(m.Width()).Render(row)
		}
	} else {
		// Normal item
		fn = func(s string) string {
			title := lipgloss.NewStyle().Foreground(d.styles.Gray).Width(18).Render(titleStr)
			desc := lipgloss.NewStyle().Foreground(d.styles.Muted).Render(descStr)
			row := "  " + title + desc
			return lipgloss.NewStyle().Background(d.styles.BgInput).Width(m.Width()).Render(row)
		}
	}

	fmt.Fprint(w, fn(""))
}

func newCommandList(s Styles) list.Model {
	items := []list.Item{
		commandItem{title: "voice", desc: "Switch to Voice-Only mode"},
		commandItem{title: "text", desc: "Switch to Text-Only mode"},
		commandItem{title: "both", desc: "Switch to Voice + Text mode"},
		commandItem{title: "mute", desc: "Toggle microphone mute state globally"},
		commandItem{title: "context", desc: "View recent semantic memory"},
		commandItem{title: "help", desc: "Show all available commands"},
		commandItem{title: "clear", desc: "Clear the screen"},
	}

	l := list.New(items, commandDelegate{styles: s}, DefaultListWidth, DefaultListHeight)
	l.SetShowTitle(false)
	l.SetShowStatusBar(false)
	l.SetFilteringEnabled(false) // We handle filtering via the textarea
	l.SetShowHelp(false)

	// Styles for pagination
	l.Styles.PaginationStyle = lipgloss.NewStyle().PaddingLeft(2).Foreground(s.Muted)

	return l
}

func FilterCommands(l *list.Model, query string) {
	allCommands := []list.Item{
		commandItem{title: "voice", desc: "Switch to Voice-Only mode"},
		commandItem{title: "text", desc: "Switch to Text-Only mode"},
		commandItem{title: "both", desc: "Switch to Voice + Text mode"},
		commandItem{title: "mute", desc: "Toggle microphone mute state globally"},
		commandItem{title: "context", desc: "View recent semantic memory"},
		commandItem{title: "help", desc: "Show all available commands"},
		commandItem{title: "clear", desc: "Clear the screen"},
	}
	var filtered []list.Item
	for _, item := range allCommands {
		ci := item.(commandItem)
		if strings.HasPrefix(ci.title, query) {
			filtered = append(filtered, item)
		}
	}
	l.SetItems(filtered)
}
