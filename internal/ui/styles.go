package ui

import "github.com/charmbracelet/lipgloss"

type Styles struct {
	// 1. Tonal Palette
	BgBase     lipgloss.Color
	BgHeader   lipgloss.Color
	BgViewport lipgloss.Color
	BgInput    lipgloss.Color
	BgStatus   lipgloss.Color

	// 2. Ink Palette
	White       lipgloss.Color
	Gray        lipgloss.Color
	Muted       lipgloss.Color
	Purple      lipgloss.Color
	Green       lipgloss.Color
	BorderColor lipgloss.Color

	// 3. Structural Layouts
	AppFrame    lipgloss.Style
	Header      lipgloss.Style
	Viewport    lipgloss.Style
	SignalField lipgloss.Style
	InputWrap   lipgloss.Style

	// 4. Component Identities
	OraLogo    lipgloss.Style
	HeaderPath lipgloss.Style
	HeaderSep  lipgloss.Style
	LivePill   lipgloss.Style

	PrefixSystem lipgloss.Style
	PrefixYou    lipgloss.Style
	PrefixOra    lipgloss.Style
	TextSystem   lipgloss.Style
	TextYou      lipgloss.Style
	TextOra      lipgloss.Style

	ToolDot  lipgloss.Style
	ToolText lipgloss.Style

	WaveUser lipgloss.Style
	WaveOra  lipgloss.Style

	InputPrefix lipgloss.Style
	KbdKey      lipgloss.Style
	KbdLabel    lipgloss.Style
	KbdSep      lipgloss.Style
}

func DefaultStyles() Styles {
	s := Styles{
		// elevations
		BgBase:     lipgloss.Color("#0d0d0f"),
		BgHeader:   lipgloss.Color("#111114"),
		BgViewport: lipgloss.Color("#0d0d0f"),
		BgInput:    lipgloss.Color("#09090b"),
		BgStatus:   lipgloss.Color("#070709"),

		// inks
		White:       lipgloss.Color("#e4e4e7"), // zinc 200
		Gray:        lipgloss.Color("#71717a"), // zinc 400
		Muted:       lipgloss.Color("#3f3f46"), // zinc 700
		Purple:      lipgloss.Color("#7c6cf8"), // electric violet
		Green:       lipgloss.Color("#10b981"), // emerald
		BorderColor: lipgloss.Color("#1a1a24"),
	}

	// Structural Definitions
	s.AppFrame = lipgloss.NewStyle().Background(s.BgBase).Foreground(s.White)

	s.Header = lipgloss.NewStyle().
		Background(s.BgHeader).
		Border(lipgloss.NormalBorder(), false, false, true, false).
		BorderForeground(s.BorderColor).
		Padding(0, 4)

	s.Viewport = lipgloss.NewStyle().
		Background(s.BgViewport).
		Padding(2, 4)

	s.SignalField = lipgloss.NewStyle().
		Background(s.BgInput).
		Border(lipgloss.NormalBorder(), true, false, true, false).
		BorderForeground(s.BorderColor).
		Padding(1, 0)

	s.InputWrap = lipgloss.NewStyle().
		Background(s.BgInput).
		Padding(1, 4)

	// Component Identities
	s.OraLogo = lipgloss.NewStyle().Foreground(s.Purple).Bold(true)
	s.HeaderPath = lipgloss.NewStyle().Foreground(s.Muted)
	s.HeaderSep = lipgloss.NewStyle().Foreground(s.BorderColor).Margin(0, 1)

	s.LivePill = lipgloss.NewStyle().
		Foreground(s.Green).
		Background(lipgloss.Color("#061611")).
		Padding(0, 1)

	// Text Block Identities
	s.PrefixSystem = lipgloss.NewStyle().Background(lipgloss.Color("#13131a")).Foreground(s.Muted).Width(10)
	s.TextSystem = lipgloss.NewStyle().Background(lipgloss.Color("#13131a")).Foreground(s.Muted)

	s.PrefixYou = lipgloss.NewStyle().Foreground(s.Green).Bold(true).Width(10)
	s.TextYou = lipgloss.NewStyle().Foreground(lipgloss.Color("#a3e635")) // bright lime-green for pop

	s.PrefixOra = lipgloss.NewStyle().Foreground(s.Purple).Bold(true).Width(10)
	s.TextOra = lipgloss.NewStyle().Foreground(s.White)

	s.ToolDot = lipgloss.NewStyle().Foreground(s.Purple)
	s.ToolText = lipgloss.NewStyle().Foreground(s.Gray).Italic(true)

	// Interaction Elements
	s.InputPrefix = lipgloss.NewStyle().Foreground(s.Purple).MarginRight(2).Bold(true)
	s.KbdKey = lipgloss.NewStyle().
		Background(s.BgStatus).
		Foreground(s.Muted).
		Padding(0, 1)
	s.KbdLabel = lipgloss.NewStyle().Foreground(s.Muted).MarginLeft(1)
	s.KbdSep = lipgloss.NewStyle().Foreground(s.BorderColor).Margin(0, 1)

	// Waves
	s.WaveUser = lipgloss.NewStyle().Foreground(s.Green)
	s.WaveOra = lipgloss.NewStyle().Foreground(s.Purple)

	return s
}
