package ui

import (
	"strings"
	"time"

	"ora/internal/agent"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type Message struct {
	Sender    string
	Content   string
	IsTool    bool
	IsThought bool
}

type model struct {
	agent       *agent.Agent
	viewport    viewport.Model
	textarea    textarea.Model
	styles      Styles
	messages    []Message
	lastSender  string
	lastUpdate  time.Time
	micWave     *Waveform
	speakerWave *Waveform
	width       int
	height      int
	isThinking  bool
}

type responseMsg string
type tickMsg time.Time

func NewModel(a *agent.Agent) model {
	s := DefaultStyles()

	ta := textarea.New()
	ta.Placeholder = "ask anything, or /command"
	ta.Focus()
	ta.Prompt = "" // Handled manually in rendering
	ta.CharLimit = 1000
	ta.SetHeight(1)
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.ShowLineNumbers = false

	vp := viewport.New(80, 20)

	banner := ` ██████╗ ██████╗  █████╗ 
██╔═══██╗██╔══██╗██╔══██╗
██║   ██║██████╔╝███████║
██║   ██║██╔══██╗██╔══██║
╚██████╔╝██║  ██║██║  ██║
 ╚═════╝ ╚═╝  ╚═╝╚═╝  ╚═╝`

	introMsg := Message{
		Sender:  "system",
		Content: "\n\n\n\n\n\n" + banner + "\n\nambient OS companion · v0.1-alpha · type /help for commands",
	}

	return model{
		agent:       a,
		styles:      s,
		textarea:    ta,
		viewport:    vp,
		messages:    []Message{introMsg},
		micWave:     NewWaveform(40),
		speakerWave: NewWaveform(40),
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(
		textarea.Blink,
		m.waitForResponse(),
		m.waitForError(),
		m.doTick(),
	)
}

func (m model) doTick() tea.Cmd {
	return tea.Tick(time.Millisecond*50, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

type errorMsg error

func (m model) waitForError() tea.Cmd {
	return func() tea.Msg {
		err, ok := <-m.agent.ErrorChan
		if !ok {
			return nil
		}
		return errorMsg(err)
	}
}

func (m model) waitForResponse() tea.Cmd {
	return func() tea.Msg {
		res, ok := <-m.agent.TextResponseChan
		if !ok {
			return nil
		}
		return responseMsg(res)
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var (
		tiCmd tea.Cmd
		vpCmd tea.Cmd
	)

	m.textarea, tiCmd = m.textarea.Update(msg)
	m.viewport, vpCmd = m.viewport.Update(msg)

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyCtrlC, tea.KeyEsc:
			return m, tea.Quit
		case tea.KeyEnter:
			input := strings.TrimSpace(m.textarea.Value())
			if input != "" {
				if strings.HasPrefix(input, "/") {
					m.messages = append(m.messages, Message{Sender: "tool", Content: "Executed command: " + input, IsTool: true})
					m.updateViewport()
				} else {
					m.agent.TextChan <- input
					m.streamLine("you", input)
				}
				m.textarea.Reset()
			}
		}

	case responseMsg:
		m.streamLine("ora", string(msg))
		return m, m.waitForResponse()

	case errorMsg:
		m.streamLine("system", "CONNECTION CRITICAL: "+msg.Error())
		return m, m.waitForError()

	case tickMsg:
		if m.agent.GetMic() != nil {
			m.micWave.Update(m.agent.GetMic().CurrentAmplitude())
		}
		if m.agent.GetSpeaker() != nil {
			m.speakerWave.Update(m.agent.GetSpeaker().CurrentAmplitude())
		}
		return m, m.doTick()

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

		// Central Anchor Layout Math
		// Header(1) + Waveforms(5) + InputWrap(4) = ~10 lines
		m.viewport.Width = msg.Width
		m.viewport.Height = max(5, msg.Height-12)
		m.textarea.SetWidth(msg.Width - 10)

		waveWidth := max(20, (msg.Width-20)/2)
		m.micWave.SetWidth(waveWidth)
		m.speakerWave.SetWidth(waveWidth)
		m.updateViewport()
	}

	return m, tea.Batch(tiCmd, vpCmd)
}

func (m *model) updateViewport() {
	var wrapped []string

	renderWidth := max(20, m.viewport.Width-12)

	for _, msg := range m.messages {
		var line string

		if msg.Sender == "system" {
			if strings.Contains(msg.Content, "██") {
				// Centered Banner - No background stripe
				bannerLines := strings.Split(msg.Content, "\n")
				var centeredLines []string
				for _, l := range bannerLines {
					if strings.Contains(l, "██") {
						// Center the ASCII logo lines
						padding := max(0, (renderWidth-lipgloss.Width(l))/2)
						centeredLines = append(centeredLines, strings.Repeat(" ", padding)+m.styles.OraLogo.Render(l))
					} else if strings.TrimSpace(l) != "" {
						// Center and style the tagline
						padding := max(0, (renderWidth-lipgloss.Width(l))/2)
						centeredLines = append(centeredLines, strings.Repeat(" ", padding)+m.styles.HeaderPath.Render(l))
					} else {
						centeredLines = append(centeredLines, "")
					}
				}
				wrapped = append(wrapped, strings.Join(centeredLines, "\n"))
				continue
			} else {
				// Full-width recessed thought - force the background to fill the width
				line = m.styles.PrefixSystem.Render("system") + m.styles.TextSystem.Render(msg.Content)
				block := m.styles.TextSystem.Copy().Width(renderWidth).Align(lipgloss.Left).Render(line)
				wrapped = append(wrapped, block)
				continue
			}
		} else if msg.IsTool {
			padding := strings.Repeat(" ", 10)
			dot := m.styles.ToolDot.Render("●")
			line = padding + dot + " " + m.styles.ToolText.Render(msg.Content)
		} else if msg.Sender == "you" {
			line = m.styles.PrefixYou.Render("you") + m.styles.TextYou.Render(msg.Content)
		} else if msg.Sender == "ora" {
			if msg.IsThought {
				// Full-width recessed thought - force the background to fill the width
				line = m.styles.PrefixSystem.Render("thought") + m.styles.TextSystem.Render(msg.Content)
				block := m.styles.TextSystem.Copy().Width(renderWidth).Align(lipgloss.Left).Render(line)
				wrapped = append(wrapped, block)
				continue
			} else {
				line = m.styles.PrefixOra.Render("ora") + m.styles.TextOra.Render(msg.Content)
			}
		}

		block := lipgloss.NewStyle().Width(renderWidth).Render(line)
		wrapped = append(wrapped, block)
	}

	m.viewport.SetContent(strings.Join(wrapped, "\n\n"))
	m.viewport.GotoBottom()
}

func (m *model) streamLine(sender, content string) {
	if sender == "ora" && strings.Contains(content, "**") {
		m.isThinking = !m.isThinking
		content = strings.ReplaceAll(content, "**", "")

		if content != "" {
			m.messages = append(m.messages, Message{Sender: sender, Content: content, IsThought: m.isThinking})
			m.lastSender = "" // Force new block
		}
	} else if len(m.messages) > 0 && m.messages[len(m.messages)-1].Sender == sender && m.messages[len(m.messages)-1].IsThought == m.isThinking && !m.messages[len(m.messages)-1].IsTool {
		m.messages[len(m.messages)-1].Content += content
	} else {
		m.messages = append(m.messages, Message{Sender: sender, Content: content, IsThought: m.isThinking})
		m.lastSender = sender
	}

	if time.Since(m.lastUpdate) > 80*time.Millisecond || sender == "you" {
		m.updateViewport()
		m.lastUpdate = time.Now()
	}
}

func (m model) View() string {
	if m.width == 0 || m.height == 0 {
		return "Initializing Ora..."
	}

	headerLeft := lipgloss.JoinHorizontal(lipgloss.Center,
		m.styles.OraLogo.Render("ORA"),
		m.styles.HeaderSep.Render("·"),
		m.styles.HeaderPath.Render("~/dev/ora"),
	)

	header := lipgloss.JoinHorizontal(lipgloss.Center,
		headerLeft,
		strings.Repeat(" ", max(0, m.width-lipgloss.Width(headerLeft)-10)),
		m.styles.LivePill.Render("● live"),
	)
	headerStr := m.styles.Header.Width(m.width).Render(header)

	micView := m.micWave.Render(m.styles.WaveUser, "MICROPHONE")
	speakerView := m.speakerWave.Render(m.styles.WaveOra, "ORA VOICE")
	
	// Join with a fixed gap and force the entire block to be centered within the full width
	waves := lipgloss.JoinHorizontal(lipgloss.Top, micView, "          ", speakerView)
	
	// Ground the signal field with a full-width solid-surface block
	signalField := m.styles.SignalField.Width(m.width).Align(lipgloss.Center).Render(waves)

	vpStr := m.styles.Viewport.Width(m.width).Render(m.viewport.View())

	inpRow := lipgloss.JoinHorizontal(lipgloss.Center,
		m.styles.InputPrefix.Render("❯"),
		m.textarea.View(),
	)

	hints := lipgloss.JoinHorizontal(lipgloss.Center,
		m.styles.KbdKey.Render("↵"), m.styles.KbdLabel.Render("send"), m.styles.KbdSep.Render("·"),
		m.styles.KbdKey.Render("/context"), m.styles.KbdLabel.Render("workspace"), m.styles.KbdSep.Render("·"),
		m.styles.KbdKey.Render("/voice"), m.styles.KbdLabel.Render("toggle"),
	)

	inputWrap := m.styles.InputWrap.Width(m.width).Render(
		lipgloss.JoinVertical(lipgloss.Left, inpRow, lipgloss.NewStyle().MarginTop(1).Render(hints)),
	)

	content := lipgloss.JoinVertical(lipgloss.Left,
		headerStr,
		vpStr,
		signalField,
		inputWrap,
	)

	return lipgloss.Place(
		m.width, m.height,
		lipgloss.Left, lipgloss.Top,
		m.styles.AppFrame.Render(content),
		lipgloss.WithWhitespaceChars(" "),
		lipgloss.WithWhitespaceBackground(m.styles.BgBase),
	)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func Run(a *agent.Agent) error {
	p := tea.NewProgram(NewModel(a), tea.WithAltScreen())
	_, err := p.Run()
	return err
}
