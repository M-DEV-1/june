package ui

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"time"

	"ora/internal/agent"
	"ora/internal/config"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/spinner"
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
	spinner       spinner.Model
	// activity is the shared live-status indicator — covers both "thinking" (sent text, no response yet) and an in-flight tool call, distinguished by kind. nil means nothing active and the status line is hidden.
	activity *liveStatus
}

type liveStatusKind int

const (
	statusThinking liveStatusKind = iota
	statusTool
)

type liveStatus struct {
	kind    liveStatusKind
	id      string // matches agent.ToolActivity.ID when kind==statusTool; "" for statusThinking
	label   string // precomputed text rendered next to the spinner glyph
	started time.Time
}

// staleActivityTimeout is a safety valve, not a normal-path timer — real tool calls finish in well under this. ToolActivityChan's non-blocking send (sendToolActivity in connect.go) can drop a Finished event under a burst of concurrent tool calls (buffer is 20, 10 calls' worth of Started+Finished pairs), which would otherwise leave the spinner stuck forever.
const staleActivityTimeout = 30 * time.Second

type responseMsg string
type tickMsg time.Time
type errorMsg error
type daemonPollMsg time.Time
type daemonStatusMsg bool

// daemonStatusURL/daemonPollClient duplicate cmd/root.go's pingDaemon pattern instead of importing cmd, since cmd/client.go already imports internal/ui and the reverse import would cycle. Keep the port in sync with cmd.DaemonPort (cmd/daemon.go) if it ever changes.
const daemonPort = "6942"
const daemonStatusURL = "http://127.0.0.1:" + daemonPort + "/status"

var daemonPollClient = &http.Client{Timeout: 300 * time.Millisecond}

// pollDaemonHTTP is a GET-and-check-200 helper. Takes client/URL as params instead of reading the package-level ones directly, so tests can point it at an httptest.Server instead of the real daemon port. tokenPath points at the daemon's IPC auth token file (see internal/ipctoken) — /status now requires it like every other daemon IPC endpoint except /ping; a read failure just means the request goes out without the header and the daemon 401s it, same as any other unreachable-daemon case this already has to tolerate.
func pollDaemonHTTP(client *http.Client, url, tokenPath string) bool {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	if token, err := ipctoken.Read(tokenPath); err == nil {
		req.Header.Set(ipctoken.HeaderName, token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func NewModel(a *agent.Agent, daemonStatus, buildMismatch string) model {
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
	if buildMismatch != "" {
		introContent += "\n⚠ " + buildMismatch
	}

	introMsg := Message{
		Sender:  "system",
		Content: introContent,
	}

	daemonOK := daemonStatus == "connected" || daemonStatus == "started"

	sp := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	sp.Style = s.StatusLine

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
		spinner:     sp,
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(
		textarea.Blink,
		m.waitForResponse(),
		m.waitForError(),
		m.waitForToolRequest(),
		m.waitForToolActivity(),
		m.doTick(),
		m.daemonPollTick(),
	)
}

func (m model) doTick() tea.Cmd {
	// 50ms feels smooth enough for the waves
	return tea.Tick(time.Millisecond*50, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

// daemonPollTick arms the next daemon-status check on its own ticker rather than piggybacking on doTick's 50ms heartbeat, which is already dense with waveform/RAM/textarea work every 20th of a second.
func (m model) daemonPollTick() tea.Cmd {
	return tea.Tick(DaemonPollInterval, func(t time.Time) tea.Msg {
		return daemonPollMsg(t)
	})
}

func (m model) pollDaemonStatus() tea.Cmd {
	return func() tea.Msg {
		return daemonStatusMsg(pollDaemonHTTP(daemonPollClient, daemonStatusURL, ipctoken.DefaultPath))
	}
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

func (m model) waitForToolActivity() tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-m.agent.ToolActivityChan
		if !ok {
			return nil
		}
		return ev
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
		// The approval menu already says "paused, waiting on you" — a spinner behind it would misleadingly read as "still running".
		m.activity = nil
		m.recalcViewportHeight()
		return m, m.waitForToolRequest()

	case agent.ToolActivity:
		if msg.Phase == agent.ToolStarted {
			m.activity = &liveStatus{kind: statusTool, id: msg.ID, label: msg.Name + "(" + msg.ArgsSummary + ")", started: time.Now()}
			m.recalcViewportHeight()
			return m, tea.Batch(m.waitForToolActivity(), m.spinner.Tick)
		}

		// Finished: always append the transcript line even if a concurrent call has since taken over the live-status slot (see the StaleID test) — every call still gets its own permanent record.
		m.messages = append(m.messages, Message{
			Sender:        "tool",
			Content:       msg.Name + "(" + msg.ArgsSummary + ") → " + msg.ResultSummary,
			IsToolLog:     true,
			ToolLogFailed: msg.Err,
		})
		m.updateViewport()
		if m.activity != nil && m.activity.id == msg.ID {
			m.activity = &liveStatus{kind: statusThinking, label: "thinking", started: time.Now()}
			m.recalcViewportHeight()
		}
		return m, m.waitForToolActivity()

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		if m.activity != nil {
			return m, cmd
		}
		// Nothing active: don't return the spinner's self-re-arming cmd, or it would keep ticking forever in the background.
		return m, nil

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
					m.activity = &liveStatus{kind: statusThinking, label: "thinking", started: time.Now()}
					m.recalcViewportHeight()
					return m, m.spinner.Tick
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
		m.activity = nil
		m.recalcViewportHeight()
		m.streamLine("ora", string(msg))
		return m, m.waitForResponse()
	case errorMsg:
		m.isConnected = false
		m.activity = nil
		m.recalcViewportHeight()
		m.streamLine("system", "CONNECTION CRITICAL: "+msg.Error())
		return m, m.waitForError()

	case daemonPollMsg:
		return m, tea.Batch(m.daemonPollTick(), m.pollDaemonStatus())
	case daemonStatusMsg:
		m.daemonOK = bool(msg)
		return m, nil

	case tickMsg:
		// Safety valve — see staleActivityTimeout's doc comment. Only a dropped Finished event under a tool-call burst reaches this.
		if m.activity != nil && time.Since(m.activity.started) > staleActivityTimeout {
			m.activity = nil
			m.recalcViewportHeight()
		}

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
				m.recalcViewportHeight()
			}
		}

		return m, m.doTick()

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

		// recalculate textarea height for sizing
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
			taHeight := max(1, min(MaxTextareaHeight, lines))
			m.textarea.SetHeight(taHeight)
		}

		m.viewport.Width = msg.Width
		m.recalcViewportHeight()
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

// recalcViewportHeight is the single source of truth for viewport height: textarea height, signal field (0 in ModeText), and the status line (1 line when m.activity is set). Shared by WindowSizeMsg, tickMsg's textarea-grow branch, and every m.activity mutation site below, so the status line never overlaps the input row or lags a tick behind.
func (m *model) recalcViewportHeight() {
	signalHeight := SignalFieldHeight
	if m.mode == ModeText {
		signalHeight = 0
	}
	statusLineHeight := 0
	if m.activity != nil {
		statusLineHeight = 1
	}
	m.viewport.Height = max(MinViewportHeight, m.height-m.textarea.Height()-signalHeight-statusLineHeight-LayoutPadding)
}

// handles the real-time streaming logic, keeps the viewport updated
func (m *model) streamLine(sender, content string) {
	// In voice mode, Ora's audio plays through the speaker, so drop text responses from the viewport — they'd be incomplete fragments anyway. Thoughts, system, and tool messages stay visible.
	if m.mode == ModeVoice && sender == "ora" && !m.isThinking {
		return
	}

	// ora uses ** to signal thinking state changes
	if sender == "ora" && strings.Contains(content, "**") {
		m.isThinking = !m.isThinking
		content = strings.ReplaceAll(content, "**", "")

		if content != "" {
			m.messages = append(m.messages, Message{Sender: sender, Content: content, IsThought: m.isThinking})
		}
	} else if len(m.messages) > 0 && m.messages[len(m.messages)-1].Sender == sender && m.messages[len(m.messages)-1].IsThought == m.isThinking && !m.messages[len(m.messages)-1].IsTool {
		// append to the last message if it's the same sender and state
		m.messages[len(m.messages)-1].Content += content
	} else {
		// start a new message block
		m.messages = append(m.messages, Message{Sender: sender, Content: content, IsThought: m.isThinking})
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
  /voice          - Switch to Voice-Only mode
  /voice list     - List available TTS voices and show the current one
  /voice preview <name> - Hear a sample of a voice without changing it
  /voice <name>   - Change Ora's speaking voice (e.g. /voice Kore)
  /text           - Switch to Text-Only mode
  /both           - Switch to Voice + Text mode
  /mute           - Toggle global microphone mute
  /context        - View the semantic memory currently loaded
  /note <txt>     - Save a stable user-stated fact
  /notes          - List saved notes
  /clear          - Clear the chat screen
  /help           - Show this help menu`
		m.messages = append(m.messages, Message{Sender: "system", Content: helpText})
	case "/context":
		importCtx, err := m.agent.GetBrain().GetImplicitContext(context.Background())
		if err != nil {
			m.messages = append(m.messages, Message{Sender: "system", Content: "Failed to fetch context: " + err.Error()})
		} else {
			m.messages = append(m.messages, Message{Sender: "system", Content: "Active Context Window:\n" + strings.Join(importCtx, "\n")})
		}
	case "/notes":
		notes, err := m.agent.GetBrain().GetNotes(context.Background())
		if err != nil {
			m.messages = append(m.messages, Message{Sender: "system", Content: "Failed to fetch notes: " + err.Error()})
		} else if len(notes) == 0 {
			m.messages = append(m.messages, Message{Sender: "system", Content: "No notes yet. Save one with /note <text>."})
		} else {
			lines := make([]string, 0, len(notes))
			for _, n := range notes {
				lines = append(lines, fmt.Sprintf("  [%s] %s", n.Kind, n.Content))
			}
			m.messages = append(m.messages, Message{Sender: "system", Content: "Saved Notes:\n" + strings.Join(lines, "\n")})
		}
	default:
		// /voice list | /voice <name> — bare "/voice" above keeps its own meaning (switch to Voice-Only); anything after it is voice selection instead.
		if strings.HasPrefix(input, "/voice ") {
			m.handleVoiceCommand(strings.TrimPrefix(input, "/voice "))
		} else if strings.HasPrefix(input, "/note ") {
			content := strings.TrimSpace(strings.TrimPrefix(input, "/note "))
			if content == "" {
				m.messages = append(m.messages, Message{Sender: "system", Content: "Usage: /note <text>"})
			} else {
				_, err := m.agent.GetBrain().LogNote(context.Background(), content, "fact")
				if err != nil {
					m.messages = append(m.messages, Message{Sender: "system", Content: "Failed to save note: " + err.Error()})
				} else {
					m.messages = append(m.messages, Message{Sender: "system", Content: "Note saved: " + content})
				}
			}
		} else if input == "/note" {
			m.messages = append(m.messages, Message{Sender: "system", Content: "Usage: /note <text>"})
		} else {
			m.messages = append(m.messages, Message{Sender: "system", Content: "Unknown command: " + input + ". Type /help for available commands."})
		}
	}
	m.updateViewport()
}

// handleVoiceCommand implements "/voice list" and "/voice <name>". Bare "/voice" never reaches here — it's handled above as the Voice-Only mode switch.
func (m *model) handleVoiceCommand(arg string) {
	action, name := parseVoiceCommand(arg)

	current := m.agent.GetVoice()
	if current == "" {
		current = config.DefaultVoice
	}

	if action == voiceActionList {
		m.messages = append(m.messages, Message{Sender: "system", Content: voiceListMessage(current)})
		return
	}

	if action == voiceActionPreview {
		if name == "" {
			m.messages = append(m.messages, Message{Sender: "system", Content: voicePreviewUsageMessage()})
			return
		}
		canonical, ok := config.NormalizeVoice(name)
		if !ok {
			m.messages = append(m.messages, Message{Sender: "system", Content: voiceUnknownMessage(name)})
			return
		}
		m.messages = append(m.messages, Message{Sender: "system", Content: voicePreviewStartMessage(canonical)})
		if err := m.agent.PreviewVoice(context.Background(), canonical); err != nil {
			m.messages = append(m.messages, Message{Sender: "system", Content: voicePreviewErrorMessage(canonical, err)})
		}
		return
	}

	canonical, ok := config.NormalizeVoice(name)
	if !ok {
		m.messages = append(m.messages, Message{Sender: "system", Content: voiceUnknownMessage(name)})
		return
	}

	cfg := config.LoadConfig()
	if err := cfg.SetVoice(canonical); err != nil {
		m.messages = append(m.messages, Message{Sender: "system", Content: "Failed to save voice: " + err.Error()})
		return
	}

	m.agent.SetVoice(canonical)
	m.agent.TriggerReconnect()
	m.messages = append(m.messages, Message{Sender: "system", Content: voiceSetMessage(canonical)})
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
