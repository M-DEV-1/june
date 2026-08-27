package ui

import (
	"ora/internal/config"

	"github.com/charmbracelet/lipgloss"
)

// this field handles the microphone and speaker waveforms
func (m *model) renderSignalField() string {
	if m.mode == ModeText {
		return ""
	}

	micView := m.micWave.Render(m.styles.WaveUser, "MICROPHONE")
	speakerView := m.speakerWave.Render(m.styles.WaveOra, "ORA VOICE")

	// spacer needs to be as tall as the waveforms (label + 4 braille rows = 5) to avoid holes below it
	spacer := lipgloss.NewStyle().
		Background(m.styles.BgInput).
		Width(10).
		Height(5).
		Render("")
	waves := lipgloss.JoinHorizontal(lipgloss.Top, micView, spacer, speakerView)

	return m.styles.SignalField.Width(m.width).Align(lipgloss.Center).Render(waves)
}

// renderStatusLine renders the spinner + label for "agent is doing something" (an in-flight tool call, or "thinking" while waiting for the first response chunk). Returns "" when nothing is active, which renderInput treats as omit-this-row rather than a blank line.
func (m *model) renderStatusLine() string {
	if m.activity == nil {
		return ""
	}
	return m.styles.StatusLine.Render(m.spinner.View() + " " + m.activity.label)
}

func (m *model) renderInput() string {
	inpRow := lipgloss.JoinHorizontal(lipgloss.Center,
		m.styles.InputPrefix.Render("❯"),
		m.textarea.View(),
	)

	// keyboard hints
	sep := m.styles.KbdSep.Render("  ") // more space

	var modeHint string
	switch m.mode {
	case ModeVoice:
		modeHint = m.styles.KbdLabel.Render("voice: " + config.VoiceModel)
	case ModeText:
		modeHint = m.styles.KbdLabel.Render("text: " + config.TextModel)
	default:
		modeHint = m.styles.KbdLabel.Render("both: voice+text")
	}

	ramHint := m.styles.KbdLabel.Render(m.cachedRAM)

	hints := lipgloss.JoinHorizontal(lipgloss.Center,
		m.styles.KbdKey.Render("↵"), m.styles.KbdLabel.Render("send"), sep,
		m.styles.KbdKey.Render("/context"), m.styles.KbdLabel.Render("workspace"), sep,
		m.styles.KbdKey.Render("/voice"), m.styles.KbdLabel.Render("toggle"), sep,
		m.styles.KbdKey.Render("mode"), modeHint, sep,
		m.styles.KbdKey.Render("ram"), ramHint,
	)

	// Built as a slice so the status row only adds a line when present — an unconditional empty string would still add a blank row via JoinVertical's join, breaking recalcViewportHeight's count.
	rows := []string{}
	if line := m.renderStatusLine(); line != "" {
		rows = append(rows, line)
	}
	rows = append(rows, inpRow)

	// 1. Render the menu depending on state
	if m.mode == ModeToolConfirm {
		menu := lipgloss.NewStyle().
			Background(m.styles.BgInput).
			PaddingTop(1).
			Render(m.hitlList.View())
		rows = append(rows, menu)
	} else if m.showCmdList {
		menu := lipgloss.NewStyle().
			Background(m.styles.BgInput).
			PaddingTop(1).
			Render(m.cmdList.View())
		rows = append(rows, menu) // shown below the input
	} else {
		rows = append(rows, lipgloss.NewStyle().Background(m.styles.BgInput).PaddingTop(1).Render(hints))
	}

	inputDeck := lipgloss.JoinVertical(lipgloss.Left, rows...)
	return m.styles.InputWrap.Width(m.width).Render(inputDeck)
}
