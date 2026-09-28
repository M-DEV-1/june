package ui

import "github.com/charmbracelet/lipgloss"

type Styles struct {
	// 1. Tonal Palette
	BgInput lipgloss.Color

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
	JuneLogo   lipgloss.Style
	HeaderPath lipgloss.Style

	PrefixSystem  lipgloss.Style
	PrefixYou     lipgloss.Style
	PrefixJune    lipgloss.Style
	PrefixThought lipgloss.Style
	TextSystem    lipgloss.Style
	TextYou       lipgloss.Style
	TextJune      lipgloss.Style
	TextThought   lipgloss.Style

	ToolDot  lipgloss.Style
	ToolText lipgloss.Style

	ToolLogDot        lipgloss.Style
	ToolLogText       lipgloss.Style
	ToolLogFailedDot  lipgloss.Style
	ToolLogFailedText lipgloss.Style
	StatusLine        lipgloss.Style

	WaveUser lipgloss.Style
	WaveJune lipgloss.Style

	InputPrefix lipgloss.Style
	KbdKey      lipgloss.Style
	KbdLabel    lipgloss.Style
	KbdSep      lipgloss.Style
}

func DefaultStyles() Styles {
	s := Styles{
		// elevations
		BgInput: lipgloss.Color("#09090b"),

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

	// No Background: the transcript is transparent (see message.go's renderMessage doc comment) — the terminal's own background shows through the padding too.
	s.Viewport = lipgloss.NewStyle().
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
	s.JuneLogo = lipgloss.NewStyle().Foreground(s.Purple).Bold(true)
	s.HeaderPath = lipgloss.NewStyle().Foreground(s.Muted)

	// chat block prefixes and colors — transparent (no Background): the terminal's own background shows through, like every native TUI, instead of the transcript painting its own (see message.go's renderMessage doc comment for why that approach was killed rather than patched further).
	s.PrefixSystem = lipgloss.NewStyle().Foreground(s.Muted).Width(gutterWidth).Align(lipgloss.Right).PaddingRight(2)
	s.TextSystem = lipgloss.NewStyle().Foreground(s.Muted)

	s.PrefixThought = lipgloss.NewStyle().Foreground(lipgloss.Color("#a78bfa")).Width(gutterWidth).Align(lipgloss.Right).PaddingRight(2)
	s.TextThought = lipgloss.NewStyle().
		Foreground(lipgloss.Color("#c4b5fd")).
		Italic(true).
		Border(lipgloss.NormalBorder(), false, false, false, true).
		BorderForeground(lipgloss.Color("#363650")).
		PaddingLeft(1)

	s.PrefixYou = lipgloss.NewStyle().Foreground(s.Green).Bold(true).Width(gutterWidth).Align(lipgloss.Right).PaddingRight(2)
	s.TextYou = lipgloss.NewStyle().Foreground(lipgloss.Color("#a3e635"))

	s.PrefixJune = lipgloss.NewStyle().Foreground(s.Purple).Bold(true).Width(gutterWidth).Align(lipgloss.Right).PaddingRight(2)
	s.TextJune = lipgloss.NewStyle().Foreground(s.White)

	s.ToolDot = lipgloss.NewStyle().Foreground(s.Purple)
	s.ToolText = lipgloss.NewStyle().Foreground(s.Gray).Italic(true)

	// Collapsed "tool ran" transcript entries — a step dimmer than the HITL prompt above, since it's a passive record, not a request.
	s.ToolLogDot = lipgloss.NewStyle().Foreground(s.Purple)
	s.ToolLogText = lipgloss.NewStyle().Foreground(s.Gray)

	// Failed tool calls get their own color (red-500, same family/lightness as the emerald/amber pills elsewhere) so they don't read as a normal completed call.
	s.ToolLogFailedDot = lipgloss.NewStyle().Foreground(lipgloss.Color("#ef4444"))
	s.ToolLogFailedText = lipgloss.NewStyle().Foreground(lipgloss.Color("#ef4444"))

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
	s.WaveJune = lipgloss.NewStyle().Foreground(s.Purple).Background(s.BgInput)

	return s
}
