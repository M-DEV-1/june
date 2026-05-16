package ui

import (
	"ora/internal/config"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

func (m *model) renderHeader() string {
	headerLeft := lipgloss.JoinHorizontal(lipgloss.Center,
		m.styles.OraLogo.Render("ORA"),
		m.styles.HeaderSep.Render("·"),
		m.styles.HeaderPath.Render("~/dev/ora"),
	)

	headerRight := m.styles.LivePill.Render("● live")

	// spacer math is headache, we force it to fill the gap
	spacer := strings.Repeat(" ", max(0, m.width-lipgloss.Width(headerLeft)-lipgloss.Width(headerRight)-8))

	header := lipgloss.JoinHorizontal(lipgloss.Center, headerLeft, spacer, headerRight)
	return m.styles.Header.Width(m.width).Render(header)
}

// this field handles the microphone and speaker waveforms
func (m *model) renderSignalField() string {
	if m.mode == ModeText || m.agent.IsMuted() {
		return ""
	}

	micView := m.micWave.Render(m.styles.WaveUser, "MICROPHONE")
	speakerView := m.speakerWave.Render(m.styles.WaveOra, "ORA VOICE")

	// spacer needs to be as tall as the waveforms (3 lines) to avoid holes below it
	spacer := lipgloss.NewStyle().
		Background(m.styles.BgInput).
		Width(10).
		Height(3).
		Render("")
	waves := lipgloss.JoinHorizontal(lipgloss.Top, micView, spacer, speakerView)

	return m.styles.SignalField.Width(m.width).Align(lipgloss.Center).Render(waves)
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

	hints := lipgloss.JoinHorizontal(lipgloss.Center,
		m.styles.KbdKey.Render("↵"), m.styles.KbdLabel.Render("send"), sep,
		m.styles.KbdKey.Render("/context"), m.styles.KbdLabel.Render("workspace"), sep,
		m.styles.KbdKey.Render("/voice"), m.styles.KbdLabel.Render("toggle"), sep,
		m.styles.KbdKey.Render("mode"), modeHint,
	)

	// 1. Render the command menu if active
	var inputDeck string
	if m.showCmdList {
		menu := lipgloss.NewStyle().
			Background(m.styles.BgInput).
			Render(m.cmdList.View())

		// Show menu below the input
		inputDeck = lipgloss.JoinVertical(lipgloss.Left, inpRow, menu)
	} else {
		inputDeck = lipgloss.JoinVertical(lipgloss.Left, inpRow, lipgloss.NewStyle().Background(m.styles.BgInput).MarginTop(1).Render(hints))
	}

	return m.styles.InputWrap.Width(m.width).Render(inputDeck)
}
