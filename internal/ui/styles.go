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
	Viewport    lipgloss.Style
	SignalField lipgloss.Style
	InputWrap   lipgloss.Style

	// 4. Component Identities
	OraLogo    lipgloss.Style
	HeaderPath lipgloss.Style

	PrefixSystem  lipgloss.Style
	PrefixYou     lipgloss.Style
	PrefixOra     lipgloss.Style
	PrefixThought lipgloss.Style
	TextSystem    lipgloss.Style
	TextYou       lipgloss.Style
	TextOra       lipgloss.Style
	TextThought   lipgloss.Style
	GutterWidth   int

	ToolDot  lipgloss.Style
	ToolText lipgloss.Style

	ToolLogDot        lipgloss.Style
	ToolLogText       lipgloss.Style
	ToolLogFailedDot  lipgloss.Style
	ToolLogFailedText lipgloss.Style
	StatusLine        lipgloss.Style

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

	// centralized gutter width so we can change it in one place if we want more room
	gutterWidth := GutterWidth

	// structural definitions
	s.AppFrame = lipgloss.NewStyle().Foreground(s.White)

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

	// branding and status bits
	s.OraLogo = lipgloss.NewStyle().Foreground(s.Purple).Bold(true)
	s.HeaderPath = lipgloss.NewStyle().Foreground(s.Muted)
	s.HeaderSep = lipgloss.NewStyle().Foreground(s.BorderColor).Margin(0, 1)

	s.LivePill = lipgloss.NewStyle().
		Foreground(s.Green).
		Background(lipgloss.Color("#061611")).
		Padding(0, 1)

	// chat block prefixes and colors
	s.PrefixSystem = lipgloss.NewStyle().Foreground(s.Muted).Background(s.BgViewport).Width(gutterWidth).Align(lipgloss.Right).PaddingRight(2)
	s.TextSystem = lipgloss.NewStyle().Foreground(s.Muted).Background(s.BgViewport)

	s.PrefixThought = lipgloss.NewStyle().Foreground(lipgloss.Color("#a78bfa")).Background(s.BgViewport).Width(gutterWidth).Align(lipgloss.Right).PaddingRight(2)
	s.TextThought = lipgloss.NewStyle().
		Foreground(lipgloss.Color("#c4b5fd")).
		Background(s.BgThought).
		Italic(true).
		Border(lipgloss.NormalBorder(), false, false, false, true).
		BorderForeground(lipgloss.Color("#363650")).
		PaddingLeft(1)
	s.BgThought = lipgloss.Color("#13131a") // slightly lighter Zinc/Navy for thoughts

	s.PrefixYou = lipgloss.NewStyle().Foreground(s.Green).Background(s.BgViewport).Bold(true).Width(gutterWidth).Align(lipgloss.Right).PaddingRight(2)
	s.TextYou = lipgloss.NewStyle().Foreground(lipgloss.Color("#a3e635")).Background(s.BgViewport)

	s.PrefixOra = lipgloss.NewStyle().Foreground(s.Purple).Background(s.BgViewport).Bold(true).Width(gutterWidth).Align(lipgloss.Right).PaddingRight(2)
	s.TextOra = lipgloss.NewStyle().Foreground(s.White).Background(s.BgViewport)

	s.ToolDot = lipgloss.NewStyle().Foreground(s.Purple).Background(s.BgViewport)
	s.ToolText = lipgloss.NewStyle().Foreground(s.Gray).Background(s.BgViewport).Italic(true)

	// Collapsed "tool ran" transcript entries — a step dimmer than the HITL prompt above, since it's a passive record, not a request.
	s.ToolLogDot = lipgloss.NewStyle().Foreground(s.Purple).Background(s.BgViewport)
	s.ToolLogText = lipgloss.NewStyle().Foreground(s.Gray).Background(s.BgViewport)

	// Failed tool calls get their own color (red-500, same family/lightness as the emerald/amber pills elsewhere) so they don't read as a normal completed call.
	s.ToolLogFailedDot = lipgloss.NewStyle().Foreground(lipgloss.Color("#ef4444")).Background(s.BgViewport)
	s.ToolLogFailedText = lipgloss.NewStyle().Foreground(lipgloss.Color("#ef4444")).Background(s.BgViewport)

	// Live "agent is doing something" status line, rendered above the input — dim like the kbd hints it visually replaces while active.
	s.StatusLine = lipgloss.NewStyle().Foreground(s.Gray).Background(s.BgInput)

	// keyboard hints and input styling - locking to BgInput
	s.InputPrefix = lipgloss.NewStyle().
		Foreground(s.Purple).
		Background(s.BgInput).
		PaddingRight(2).
		Bold(true)

	// kbd keys - light lavender / button look
	s.KbdKey = lipgloss.NewStyle().
		Background(lipgloss.Color("#dcd7ff")).
		Foreground(lipgloss.Color("#2e2a4a")).
		Padding(0, 1).
		Bold(true)

	s.KbdLabel = lipgloss.NewStyle().
		Foreground(s.Gray).
		Background(s.BgInput).
		PaddingLeft(1).
		PaddingRight(1)

	s.KbdSep = lipgloss.NewStyle().
		Foreground(s.BorderColor).
		Background(s.BgInput).
		PaddingLeft(1).
		PaddingRight(1)

	// waveforms - anchored to the input deck's background
	s.WaveUser = lipgloss.NewStyle().Foreground(s.Green).Background(s.BgInput)
	s.WaveOra = lipgloss.NewStyle().Foreground(s.Purple).Background(s.BgInput)

	return s
}
