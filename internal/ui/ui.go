package ui

import (
	"strings"
	"time"

	"ora/internal/agent"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type AgentMode string

const (
	ModeBoth  AgentMode = "both"
	ModeVoice AgentMode = "voice"
	ModeText  AgentMode = "text"
)

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
	mode        AgentMode
	cmdList     list.Model
	showCmdList bool
}

type responseMsg string
type tickMsg time.Time
type errorMsg error

func NewModel(a *agent.Agent) model {
	s := DefaultStyles()

	ta := textarea.New()
	ta.Placeholder = "ask anything, or /command"
	ta.Focus()
	ta.Prompt = ""
	ta.CharLimit = 10000
	ta.SetHeight(1)
	ta.SetWidth(DefaultWidth) // default width before resize

	// ensure the textarea itself isn't transparent
	ta.FocusedStyle.Base = lipgloss.NewStyle().Background(s.BgInput)
	ta.BlurredStyle.Base = lipgloss.NewStyle().Background(s.BgInput)
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle().Background(s.BgInput)
	ta.ShowLineNumbers = false

	vp := viewport.New(DefaultWidth, DefaultViewportHeight)

	// starting with some whitespace and the banner to give it some breathing room
	banner := ` ██████╗ ██████╗  █████╗ 
██╔═══██╗██╔══██╗██╔══██╗
██║   ██║██████╔╝███████║
██║   ██║██╔══██╗██╔══██║
╚██████╔╝██║  ██║██║  ██║
 ╚═════╝ ╚═╝  ╚═╝╚═╝  ╚═╝`

	introMsg := Message{
		Sender: "system",
		// we use vertical space to push the banner to the middle-ish
		Content: "\n\n\n\n\n\n" + banner + "\n\nambient OS companion · v0.1.1-alpha · type /help for commands",
	}

	return model{
		agent:       a,
		styles:      s,
		textarea:    ta,
		viewport:    vp,
		messages:    []Message{introMsg},
		micWave:     NewWaveform(40),
		speakerWave: NewWaveform(40),
		mode:        ModeBoth,
		cmdList:     newCommandList(s),
	}
}

func (m model) Init() tea.Cmd {
	// start everything at once
	return tea.Batch(
		textarea.Blink,
		m.waitForResponse(),
		m.waitForError(),
		m.doTick(),
	)
}

func (m model) doTick() tea.Cmd {
	// 50ms feels smooth enough for the waves
	return tea.Tick(time.Millisecond*50, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

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

	switch msg := msg.(type) {
	case tea.MouseMsg:
		if m.showCmdList {
			m.cmdList, _ = m.cmdList.Update(msg)
			m.textarea, tiCmd = m.textarea.Update(msg)
			return m, tiCmd
		}
		m.viewport, vpCmd = m.viewport.Update(msg)
		m.textarea, tiCmd = m.textarea.Update(msg)
		return m, tea.Batch(vpCmd, tiCmd)
	}

	m.textarea, tiCmd = m.textarea.Update(msg)
	m.viewport, vpCmd = m.viewport.Update(msg)

	// all key/slash commands
	switch msg := msg.(type) {
	case tea.KeyMsg:
		inputVal := m.textarea.Value()
		m.showCmdList = strings.HasPrefix(inputVal, "/") || (len(inputVal) == 0 && msg.Type == tea.KeyRunes && msg.String() == "/")

		if m.showCmdList {
			if strings.HasPrefix(inputVal, "/") {
				FilterCommands(&m.cmdList, inputVal[1:])
			}
			switch msg.Type {
			case tea.KeyUp, tea.KeyDown:
				var cmd tea.Cmd
				m.cmdList, cmd = m.cmdList.Update(msg)
				return m, cmd
			case tea.KeyEnter:
				if i, ok := m.cmdList.SelectedItem().(commandItem); ok {
					input := "/" + i.Title()
					m.executeCommand(input)
					m.textarea.Reset()
					m.showCmdList = false
					return m, nil
				}
			}
		}

		switch msg.Type {
		case tea.KeyCtrlC, tea.KeyEsc:
			return m, tea.Quit
		case tea.KeyCtrlJ:
			m.textarea.InsertString("\n")
		case tea.KeyEnter:
			input := strings.TrimSpace(m.textarea.Value())
			if input != "" {
				if strings.HasPrefix(input, "/") {
					m.executeCommand(input)
					m.textarea.Reset()
					m.showCmdList = false
				} else {
					m.agent.TextChan <- input
					m.streamLine("you", input)
					m.textarea.Reset()
				}
			}
		}

	case responseMsg:
		m.streamLine("ora", string(msg))
		return m, m.waitForResponse()
	case errorMsg:
		m.streamLine("system", "CONNECTION CRITICAL: "+msg.Error())
		return m, m.waitForError()

	case tickMsg:
		// update waves from the live audio buffers
		if m.agent.GetMic() != nil {
			m.micWave.Update(m.agent.GetMic().CurrentAmplitude())
		}
		if m.agent.GetSpeaker() != nil {
			m.speakerWave.Update(m.agent.GetSpeaker().CurrentAmplitude())
		}

		// check if textarea needs to grow
		{
			val := m.textarea.Value()
			taWidth := m.width - GutterWidth
			if taWidth <= 0 {
				taWidth = DefaultWidth
			}
			lines := 0
			for _, line := range strings.Split(val, "\n") {
				if len(line) == 0 {
					lines++
					continue
				}
				lines += (len(line) + taWidth - 1) / taWidth
			}
			taHeight := max(1, min(MaxTextareaHeight, lines))
			if taHeight != m.textarea.Height() {
				m.textarea.SetHeight(taHeight)

				signalHeight := SignalFieldHeight
				if m.mode == ModeText {
					signalHeight = 0
				}
				m.viewport.Height = max(MinViewportHeight, m.height-taHeight-signalHeight-LayoutPadding)
			}
		}

		return m, m.doTick()

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

		// recalculate textarea height for sizing
		var taHeight int
		{
			val := m.textarea.Value()
			taWidth := msg.Width - GutterWidth
			if taWidth <= 0 {
				taWidth = DefaultWidth
			}
			lines := 0
			for _, line := range strings.Split(val, "\n") {
				if len(line) == 0 {
					lines++
					continue
				}
				lines += (len(line) + taWidth - 1) / taWidth
			}
			taHeight = max(1, min(MaxTextareaHeight, lines))
			m.textarea.SetHeight(taHeight)
		}

		signalHeight := SignalFieldHeight
		if m.mode == ModeText {
			signalHeight = 0
		}
		m.viewport.Width = msg.Width
		m.viewport.Height = max(MinViewportHeight, msg.Height-taHeight-signalHeight-LayoutPadding)
		m.textarea.SetWidth(msg.Width - GutterWidth)

		// split the width between the two waveforms
		waveWidth := max(MinWaveWidth, (msg.Width-MinWaveWidth)/2)
		m.micWave.SetWidth(waveWidth)
		m.speakerWave.SetWidth(waveWidth)
		m.updateViewport()
	}

	return m, tea.Batch(tiCmd, vpCmd)
}

// handles the real-time streaming logic, keeps the viewport updated
func (m *model) streamLine(sender, content string) {
	// If in voice mode, drop standard Ora text responses (keep thoughts)
	if m.mode == ModeVoice && sender == "ora" && !strings.Contains(content, "**") && !m.isThinking {
		return
	}

	// ora uses ** to signal thinking state changes
	if sender == "ora" && strings.Contains(content, "**") {
		m.isThinking = !m.isThinking
		content = strings.ReplaceAll(content, "**", "")

		if content != "" {
			m.messages = append(m.messages, Message{Sender: sender, Content: content, IsThought: m.isThinking})
			m.lastSender = ""
		}
	} else if len(m.messages) > 0 && m.messages[len(m.messages)-1].Sender == sender && m.messages[len(m.messages)-1].IsThought == m.isThinking && !m.messages[len(m.messages)-1].IsTool {
		// append to the last message if it's the same sender and state
		m.messages[len(m.messages)-1].Content += content
	} else {
		// start a new message block
		m.messages = append(m.messages, Message{Sender: sender, Content: content, IsThought: m.isThinking})
		m.lastSender = sender
	}

	// throttle viewport updates so it doesn't flicker too much
	if time.Since(m.lastUpdate) > 80*time.Millisecond || sender == "you" {
		m.updateViewport()
		m.lastUpdate = time.Now()
	}
}

func (m *model) executeCommand(input string) {
	switch input {
	case "/voice":
		m.mode = ModeVoice
		m.agent.SetMute(false)
		m.messages = append(m.messages, Message{Sender: "system", Content: "Switched to Voice-Only Mode (Ora text output hidden)"})
	case "/text":
		m.mode = ModeText
		m.agent.SetMute(true)
		m.messages = append(m.messages, Message{Sender: "system", Content: "Switched to Text-Only Mode (Microphone paused)"})
	case "/both":
		m.mode = ModeBoth
		m.agent.SetMute(false)
		m.messages = append(m.messages, Message{Sender: "system", Content: "Switched to Voice + Text Mode"})
	case "/mute":
		isMuted := m.agent.ToggleMute()
		status := "unmuted"
		if isMuted {
			status = "muted"
		}
		m.messages = append(m.messages, Message{Sender: "system", Content: "Microphone globally " + status})
	case "/clear":
		m.messages = []Message{}
	default:
		m.messages = append(m.messages, Message{Sender: "tool", Content: "Executed command: " + input, IsTool: true})
	}
	m.updateViewport()
}

func (m model) View() string {
	if m.width == 0 || m.height == 0 {
		return "Initializing Ora..."
	}

	// stack everything vertically
	content := lipgloss.JoinVertical(lipgloss.Left,
		m.renderHeader(),
		m.styles.Viewport.Width(m.width).Render(m.viewport.View()),
		m.renderSignalField(),
		m.renderInput(),
	)

	// force the background to fill the whole screen
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
	// allows for clickable terminal
	p := tea.NewProgram(NewModel(a), tea.WithAltScreen(), tea.WithMouseCellMotion())
	_, err := p.Run()
	return err
}
