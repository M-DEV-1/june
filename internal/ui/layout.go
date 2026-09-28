package ui

import (
	"strconv"
	"time"

	"june/internal/config"

	"github.com/charmbracelet/lipgloss"
)

// this field handles the microphone and speaker waveforms
func (m *model) renderSignalField() string {
	if m.mode == ModeText {
		return ""
	}

	micView := m.micWave.Render(m.styles.WaveUser, "MICROPHONE")
	speakerView := m.speakerWave.Render(m.styles.WaveJune, "June VOICE")

	// spacer needs to be as tall as the waveforms (label + 4 braille rows = 5) to avoid holes below it
	spacer := lipgloss.NewStyle().
		Background(m.styles.BgInput).
		Width(10).
		Height(5).
		Render("")
	waves := lipgloss.JoinHorizontal(lipgloss.Top, micView, spacer, speakerView)

	return m.styles.SignalField.Width(m.width).Align(lipgloss.Center).Render(waves)
}

// renderStatusLine renders the spinner + label + elapsed time for "agent is doing something" (an in-flight tool call, or "thinking" while waiting for the first response chunk). Returns "" when nothing is active, which renderInput treats as omit-this-row rather than a blank line.
// The elapsed counter is what tells a slow call apart from a hung one: the spinner spins either way, the number only moves while the call is really open.
func (m *model) renderStatusLine() string {
	if m.activity == nil {
		return ""
	}
	return m.styles.StatusLine.Render(m.spinner.View() + " " + m.activity.label + " · " + formatElapsed(time.Since(m.activity.started)))
}

// formatElapsed renders a duration for the tool lines: tenths of a second under ten seconds, whole seconds above that.
// Input: a duration. Output: e.g. "0.8s" or "12s".
func formatElapsed(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < 10*time.Second {
		return strconv.FormatFloat(d.Seconds(), 'f', 1, 64) + "s"
	}
	return strconv.Itoa(int(d.Seconds())) + "s"
}

// hintsText returns the contextual shortcut text for the current state — the hint row reflects what's actually available right now instead of always showing the same static set. quitConfirmArmed takes priority over every other state since Ctrl+C's confirmation applies regardless of mode or menu.
func (m *model) hintsText() string {
	switch {
	case m.quitConfirmArmed:
		return "press ctrl+c again to quit"
	case m.mode == ModeToolConfirm:
		return "↑↓ choose · ↵ confirm · esc reject"
	case m.mode == ModeToolEdit:
		return "↵ run · esc cancel"
	case m.showCmdList:
		return "↑↓ navigate · ↵ select · esc close"
	default:
		return "↵ send · ctrl+j newline · / commands · esc clear · pgup/pgdn scroll"
	}
}

func (m *model) renderInput() string {
	// Top, not Center: a multi-row textarea (Ctrl+J newlines, or wrapped long input) would otherwise vertically center the ❯ prompt against the whole block, floating it away from the cursor line instead of hugging the first input row.
	inpRow := lipgloss.JoinHorizontal(lipgloss.Top,
		m.styles.InputPrefix.Render("❯"),
		m.textarea.View(),
	)

	hintLine := m.styles.KbdLabel.Render(m.hintsText())

	// Mode chip rides alongside the hint text only in the normal (no menu, not tool-confirm/edit) state — hintsText's own state check already covers exactly this "default" case, so the two conditions stay in lockstep.
	if !m.quitConfirmArmed && m.mode != ModeToolConfirm && m.mode != ModeToolEdit && !m.showCmdList {
		sep := m.styles.KbdSep.Render("  ") // more space

		var modeHint string
		switch m.mode {
		case ModeVoice:
			modeHint = m.styles.KbdLabel.Render("voice: " + config.VoiceModel())
		case ModeText:
			modeHint = m.styles.KbdLabel.Render("text: " + config.TextModel)
		default:
			modeHint = m.styles.KbdLabel.Render("both: voice+text")
		}

		chips := []string{hintLine, sep, m.styles.KbdKey.Render("mode"), modeHint}

		// Quiet when healthy — these replace the old header row's always-visible "⊙ tracking"/"● live" pills. A connection loss is already announced as a transcript system line; a stale/absent daemon has no other signal, so it gets a chip here instead.
		warnStyle := m.styles.KbdLabel.Foreground(lipgloss.Color("#f59e0b"))
		if !m.daemonOK {
			chips = append(chips, sep, warnStyle.Render("⚠ no tracker"))
		}
		if !m.isConnected {
			chips = append(chips, sep, warnStyle.Render("⚠ reconnecting"))
		}

		hintLine = lipgloss.JoinHorizontal(lipgloss.Center, chips...)
	}

	// Built as a slice so the status row only adds a line when present — an unconditional empty string would still add a blank row via JoinVertical's join, breaking recalcViewportHeight's count.
	rows := []string{}
	if line := m.renderStatusLine(); line != "" {
		rows = append(rows, line)
	}
	rows = append(rows, inpRow)
	rows = append(rows, lipgloss.NewStyle().Background(m.styles.BgInput).PaddingTop(1).Render(hintLine))

	// The selectable list, when one is open, renders below the (always-present) contextual hint line above.
	if m.mode == ModeToolConfirm {
		menu := lipgloss.NewStyle().
			Background(m.styles.BgInput).
			Render(m.hitlList.View())
		rows = append(rows, menu)
	} else if m.showCmdList {
		menu := lipgloss.NewStyle().
			Background(m.styles.BgInput).
			Render(m.cmdList.View())
		rows = append(rows, menu)
	}

	inputDeck := lipgloss.JoinVertical(lipgloss.Left, rows...)
	return m.styles.InputWrap.Width(m.width).Render(inputDeck)
}
