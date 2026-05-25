package ui

import (
	"context"
	"fmt"
	"runtime"
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
	ModeBoth        AgentMode = "both"
	ModeVoice       AgentMode = "voice"
	ModeText        AgentMode = "text"
	ModeToolConfirm AgentMode = "tool_confirm"
	ModeToolEdit    AgentMode = "tool_edit"
)

type model struct {
	agent         *agent.Agent
	viewport      viewport.Model
	textarea      textarea.Model
	styles        Styles
	messages      []Message
	lastSender    string
	lastUpdate    time.Time
	micWave       *Waveform
	speakerWave   *Waveform
	width         int
	height        int
	isThinking    bool
	isConnected   bool
	daemonOK      bool
	mode          AgentMode
	cmdList       list.Model
	showCmdList   bool
	hitlList      list.Model
	activeToolReq *agent.ToolRequest
	cachedRAM     string
	lastRAMCheck  time.Time
}

type responseMsg string
type tickMsg time.Time
type errorMsg error

func NewModel(a *agent.Agent, daemonStatus string) model {
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

	var daemonLine string
	switch daemonStatus {
	case "connected":
		daemonLine = "tracker connected · activity memory active"
	case "started":
		daemonLine = "tracker started · activity memory active"
	default:
		if daemonStatus != "" {
			daemonLine = "⚠ " + daemonStatus
		}
	}

	introContent := "\n\n\n\n\n\n" + banner + "\n\nambient OS companion · v0.1.1-alpha · type /help for commands"
	if daemonLine != "" {
		introContent += "\n" + daemonLine
	}

	introMsg := Message{
		Sender:  "system",
		Content: introContent,
	}

	daemonOK := daemonStatus == "connected" || daemonStatus == "started"

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
		hitlList:    newHitlList(s),
		isConnected: true,
		daemonOK:    daemonOK,
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(
		textarea.Blink,
		m.waitForResponse(),
		m.waitForError(),
		m.waitForToolRequest(),
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

func (m model) waitForToolRequest() tea.Cmd {
	return func() tea.Msg {
		req, ok := <-m.agent.ToolApprovalChan
		if !ok {
			return nil
		}
		return req
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var (
		tiCmd tea.Cmd
		vpCmd tea.Cmd
	)

	// all slash cmds, and tui updates
	switch msg := msg.(type) {
	case agent.ToolRequest:
		m.activeToolReq = &msg
		m.mode = ModeToolConfirm
		m.messages = append(m.messages, Message{Sender: "tool", Content: "Ora wants to execute:\n  " + msg.Command, IsTool: true})
		m.updateViewport()
		return m, m.waitForToolRequest()

	case tea.MouseMsg:
		if m.mode == ModeToolConfirm {
			m.hitlList, _ = m.hitlList.Update(msg)
			return m, nil
		}
		if m.showCmdList {
			m.cmdList, _ = m.cmdList.Update(msg)
			m.textarea, tiCmd = m.textarea.Update(msg)
			return m, tiCmd
		}
		m.viewport, vpCmd = m.viewport.Update(msg)
		m.textarea, tiCmd = m.textarea.Update(msg)
		return m, tea.Batch(vpCmd, tiCmd)

	case tea.KeyMsg:
		if m.mode == ModeToolConfirm {
			switch msg.Type {
			case tea.KeyUp, tea.KeyDown:
				var cmd tea.Cmd
				m.hitlList, cmd = m.hitlList.Update(msg)
				return m, cmd
			case tea.KeyEnter:
				if i, ok := m.hitlList.SelectedItem().(commandItem); ok {
					switch i.title {
					case "Allow once":
						m.messages = append(m.messages, Message{Sender: "tool", Content: "Approved. Executing..."})
						go func(cmd string, c chan<- string) {
							c <- agent.RunShellCommand(cmd)
						}(m.activeToolReq.Command, m.activeToolReq.ResultChan)
						m.mode = ModeBoth
					case "Allow for session":
						m.messages = append(m.messages, Message{Sender: "tool", Content: "Approved for session. Executing..."})
						m.agent.AllowedCmds.Store(m.activeToolReq.Command, true)
						go func(cmd string, c chan<- string) {
							c <- agent.RunShellCommand(cmd)
						}(m.activeToolReq.Command, m.activeToolReq.ResultChan)
						m.mode = ModeBoth
					case "Reject":
						m.messages = append(m.messages, Message{Sender: "tool", Content: "Command rejected."})
						m.activeToolReq.ResultChan <- "User rejected this command."
						m.mode = ModeBoth
					case "Suggest changes":
						m.textarea.SetValue(m.activeToolReq.Command)
						m.textarea.Focus()
						m.mode = ModeToolEdit
					}
					m.activeToolReq = nil
					m.updateViewport()
					return m, nil
				}
			}
			return m, nil
		}

		if m.mode == ModeToolEdit {
			if msg.Type == tea.KeyEnter {
				editedCmd := strings.TrimSpace(m.textarea.Value())
				m.textarea.Reset()
				m.messages = append(m.messages, Message{Sender: "tool", Content: "Executing modified command:\n  " + editedCmd})
				m.updateViewport()
				go func(cmd string, c chan<- string) {
					c <- agent.RunShellCommand(cmd)
				}(editedCmd, m.activeToolReq.ResultChan)
				m.mode = ModeBoth
				m.activeToolReq = nil
				return m, nil
			}
			if msg.Type == tea.KeyEsc {
				m.textarea.Reset()
				m.messages = append(m.messages, Message{Sender: "tool", Content: "Edit cancelled. Command rejected."})
				m.activeToolReq.ResultChan <- "User rejected this command."
				m.mode = ModeBoth
				m.activeToolReq = nil
				m.updateViewport()
				return m, nil
			}
			m.textarea, tiCmd = m.textarea.Update(msg)
			return m, tiCmd
		}

		// navigate through the menu list
		if m.showCmdList {
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

		// hotkeys
		switch msg.Type {
		case tea.KeyCtrlC, tea.KeyEsc:
			return m, tea.Quit
		case tea.KeyCtrlJ:
			m.textarea.InsertString("\n")
			return m, nil
		case tea.KeyEnter:
			input := strings.TrimSpace(m.textarea.Value())
			if input != "" {
				if strings.HasPrefix(input, "/") {
					m.executeCommand(input)
					m.textarea.Reset()
					m.showCmdList = false
				} else if m.mode == ModeVoice {
					// Voice-only: text sends are disabled. Only slash commands work.
					m.messages = append(m.messages, Message{Sender: "system", Content: "Text input disabled in voice mode. Use /both to enable."})
					m.textarea.Reset()
					m.updateViewport()
				} else {
					m.agent.TextChan <- input
					m.streamLine("you", input)
					m.textarea.Reset()
				}
				return m, nil
			}
		}

		// textarea updates (typing)
		m.textarea, tiCmd = m.textarea.Update(msg)

		// recalculation to prevent lag altogether
		inputVal := m.textarea.Value()
		m.showCmdList = strings.HasPrefix(inputVal, "/")
		if m.showCmdList {
			FilterCommands(&m.cmdList, inputVal[1:])
		}

		return m, tiCmd

	case responseMsg:
		m.isConnected = true
		m.streamLine("ora", string(msg))
		return m, m.waitForResponse()
	case errorMsg:
		m.isConnected = false
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

		// ReadMemStats causes a GC stop-the-world — only sample every 5s
		if time.Since(m.lastRAMCheck) > 5*time.Second {
			var mem runtime.MemStats
			runtime.ReadMemStats(&mem)
			if mem.Sys >= 1024*1024*1024 {
				m.cachedRAM = fmt.Sprintf("%.1f GB", float64(mem.Sys)/(1024*1024*1024))
			} else {
				m.cachedRAM = fmt.Sprintf("%d MB", mem.Sys/(1024*1024))
			}
			m.lastRAMCheck = time.Now()
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

	m.viewport, vpCmd = m.viewport.Update(msg)
	return m, tea.Batch(tiCmd, vpCmd)
}

// handles the real-time streaming logic, keeps the viewport updated
func (m *model) streamLine(sender, content string) {
	// In voice mode, Ora's audio plays through the speaker — drop text responses
	// from the viewport (they'd be incomplete fragments anyway). Keep thoughts,
	// system messages, and tool messages always visible.
	if m.mode == ModeVoice && sender == "ora" && !m.isThinking {
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
		m.textarea.Placeholder = "voice active — /both or /text to enable typing"
		m.messages = append(m.messages, Message{Sender: "system", Content: "Voice-Only Mode — mic active, text input disabled"})
	case "/text":
		m.mode = ModeText
		m.agent.SetMute(true)
		m.textarea.Placeholder = "ask anything, or /command"
		m.messages = append(m.messages, Message{Sender: "system", Content: "Text Mode — mic paused, type to interact"})
	case "/both":
		m.mode = ModeBoth
		m.agent.SetMute(false)
		m.textarea.Placeholder = "ask anything, or /command"
		m.messages = append(m.messages, Message{Sender: "system", Content: "Voice + Text Mode — mic active, text input enabled"})
	case "/mute":
		if m.mode == ModeText {
			m.messages = append(m.messages, Message{Sender: "system", Content: "Already in Text Mode — mic is paused. Use /both to re-enable."})
		} else if m.mode == ModeVoice {
			m.messages = append(m.messages, Message{Sender: "system", Content: "Cannot mute in Voice Mode. Use /both first, then /mute."})
		} else {
			isMuted := m.agent.ToggleMute()
			status := "unmuted"
			if isMuted {
				status = "muted"
			}
			m.messages = append(m.messages, Message{Sender: "system", Content: "Microphone " + status})
		}
	case "/clear":
		m.messages = []Message{}
	case "/help":
		helpText := `Available Commands:
  /voice   - Switch to Voice-Only mode
  /text    - Switch to Text-Only mode
  /both    - Switch to Voice + Text mode
  /mute    - Toggle global microphone mute
  /context - View the semantic memory currently loaded
  /clear   - Clear the chat screen
  /help    - Show this help menu`
		m.messages = append(m.messages, Message{Sender: "system", Content: helpText})
	case "/context":
		importCtx, err := m.agent.GetBrain().GetImplicitContext(context.Background())
		if err != nil {
			m.messages = append(m.messages, Message{Sender: "system", Content: "Failed to fetch context: " + err.Error()})
		} else {
			m.messages = append(m.messages, Message{Sender: "system", Content: "Active Context Window:\n" + strings.Join(importCtx, "\n")})
		}
	default:
		m.messages = append(m.messages, Message{Sender: "system", Content: "Unknown command: " + input + ". Type /help for available commands."})
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

func Run(a *agent.Agent, daemonStatus string) error {
	p := tea.NewProgram(NewModel(a, daemonStatus), tea.WithAltScreen(), tea.WithMouseCellMotion())
	_, err := p.Run()
	return err
}
