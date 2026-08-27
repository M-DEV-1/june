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

func (d commandDelegate) Height() int                               { return 1 }
func (d commandDelegate) Spacing() int                              { return 0 }
func (d commandDelegate) Update(msg tea.Msg, m *list.Model) tea.Cmd { return nil }
// commandDelegateTitleWidth and the "  " prefix commandDelegate.Render hardcodes are the two fixed-width columns eating into a row's available width before the description gets whatever's left.
const commandDelegateTitleWidth = 18

func (d commandDelegate) Render(w io.Writer, m list.Model, index int, listItem list.Item) {
	i, ok := listItem.(commandItem)
	if !ok {
		return
	}

	titleStr := i.Title()
	descStr := i.Description()
	// Clamped to fit one line, not wrapped: Height() declares 1 row per item, so a description long enough to wrap here would silently make the list taller than SetHeight(len(items)) callers assume (see WP7).
	descWidth := max(0, m.Width()-2-commandDelegateTitleWidth)

	var fn func(string) string
	if index == m.Index() {
		// Highlighted item. Every inner span carries its own explicit Background matching the row's — a nested lipgloss Render always emits its own trailing ANSI reset, which otherwise cuts the outer row's background short wherever it lands, showing the terminal's own default (black in most terminals) for the rest of the row (see WP9).
		bg, fg := lipgloss.Color("#6d28d9"), lipgloss.Color("#ffffff")
		fn = func(s string) string {
			title := lipgloss.NewStyle().Background(bg).Foreground(fg).Width(commandDelegateTitleWidth).Render(titleStr)
			desc := lipgloss.NewStyle().Background(bg).Foreground(fg).MaxWidth(descWidth).Render(descStr)
			row := "  " + title + desc
			return lipgloss.NewStyle().Background(bg).Foreground(fg).Width(m.Width()).Render(row)
		}
	} else {
		// Normal item — same reasoning as the highlighted branch above.
		fn = func(s string) string {
			title := lipgloss.NewStyle().Foreground(d.styles.Gray).Background(d.styles.BgInput).Width(commandDelegateTitleWidth).Render(titleStr)
			desc := lipgloss.NewStyle().Foreground(d.styles.Muted).Background(d.styles.BgInput).MaxWidth(descWidth).Render(descStr)
			row := "  " + title + desc
			return lipgloss.NewStyle().Background(d.styles.BgInput).Width(m.Width()).Render(row)
		}
	}

	fmt.Fprint(w, fn(""))
}

// commandItems is the single source of truth for the "/" command list, shared by newCommandList (unfiltered) and FilterCommands (prefix-filtered).
var commandItems = []list.Item{
	commandItem{title: "voice", desc: "Switch to Voice-Only mode"},
	commandItem{title: "voice list", desc: "List TTS voices / show current"},
	commandItem{title: "voice preview ", desc: "Hear a sample of a voice: /voice preview <name>"},
	commandItem{title: "text", desc: "Switch to Text-Only mode"},
	commandItem{title: "both", desc: "Switch to Voice + Text mode"},
	commandItem{title: "mute", desc: "Toggle microphone mute state globally"},
	commandItem{title: "context", desc: "View recent semantic memory"},
	commandItem{title: "note", desc: "Save a stable fact: /note <text>"},
	commandItem{title: "notes", desc: "List saved notes"},
	commandItem{title: "help", desc: "Show all available commands"},
	commandItem{title: "clear", desc: "Clear the screen"},
	commandItem{title: "quit", desc: "Quit immediately (no confirmation)"},
}

func newCommandList(s Styles) list.Model {
	l := list.New(commandItems, commandDelegate{styles: s}, DefaultListWidth, DefaultListHeight)
	l.SetShowTitle(false)
	l.SetShowStatusBar(false)
	l.SetFilteringEnabled(false) // We handle filtering via the textarea
	l.SetShowHelp(false)

	// Styles for pagination
	l.Styles.PaginationStyle = lipgloss.NewStyle().PaddingLeft(2).Foreground(s.Muted).Background(s.BgInput)

	return l
}

func newHitlList(s Styles) list.Model {
	items := []list.Item{
		commandItem{title: "Allow once", desc: "Execute this command and return the result"},
		commandItem{title: "Allow for session", desc: "Always execute this command without asking"},
		commandItem{title: "Reject", desc: "Cancel execution and notify the agent"},
		commandItem{title: "Suggest changes", desc: "Edit the command before running"},
	}

	l := list.New(items, commandDelegate{styles: s}, DefaultListWidth, DefaultListHeight)
	l.SetShowTitle(false)
	l.SetShowStatusBar(false)
	l.SetFilteringEnabled(false)
	l.SetShowHelp(false)

	l.Styles.PaginationStyle = lipgloss.NewStyle().PaddingLeft(2).Foreground(s.Muted).Background(s.BgInput)

	return l
}
func FilterCommands(l *list.Model, query string) {
	var filtered []list.Item
	for _, item := range commandItems {
		ci := item.(commandItem)
		if strings.HasPrefix(ci.title, query) {
			filtered = append(filtered, item)
		}
	}
	l.SetItems(filtered)
}
