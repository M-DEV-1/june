package ui

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"ora/internal/agent"
	"ora/internal/config"
	"ora/internal/ipctoken"

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
	isConnected   bool
	daemonOK      bool
	mode          AgentMode
	cmdList       list.Model
	showCmdList   bool
	hitlList      list.Model
	activeToolReq *agent.ToolRequest
	// pendingToolReqs holds ToolRequests that arrived while activeToolReq was still awaiting a user decision — see FINDING 3. One model turn can dispatch several concurrent tool calls (connect.go's runToolCall runs each in its own goroutine), so a second approval request can arrive mid-decision on the first; queueing instead of overwriting activeToolReq means every request eventually gets its result delivered, in arrival order.
	pendingToolReqs []agent.ToolRequest
	spinner         spinner.Model
	// activity is the shared live-status indicator — covers both "thinking" (sent text, no response yet) and an in-flight tool call, distinguished by kind. nil means nothing active and the status line is hidden.
	activity *liveStatus
	// viewportDirty is set when streamLine's 80ms throttle skips a render, so the tickMsg handler can flush it later — otherwise a reply's last chunk can go permanently unrendered if no further chunk ever arrives to trigger the next render.
	viewportDirty bool
	// expandThoughts is a global fold toggle (Ctrl+E): false renders every thought message collapsed to one dim preview line, true renders them in full. Streaming still accumulates into the message's Content regardless — folding is a render-time concern only.
	expandThoughts bool
	// quitConfirmArmed/quitConfirmArmedAt back the Ctrl+C double-press-to-quit confirmation: the first press arms it and shows a hint instead of quitting; a second press within quitConfirmWindow actually quits. Auto-cleared by the tickMsg handler once the window lapses, same shape as staleActivityTimeout.
	quitConfirmArmed   bool
	quitConfirmArmedAt time.Time
	// closeOraBlock is set by a TurnBoundary chunk and consumed by the next ora chunk streamLine sees — it forces that chunk to start a fresh message block instead of merging into whatever the finished turn left behind.
	closeOraBlock bool
}

// quitConfirmWindow is how long a Ctrl+C press stays "armed" waiting for the confirming second press.
const quitConfirmWindow = 1500 * time.Millisecond

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

// responseMsg carries a model output chunk into bubbletea's Update loop, tagged with the real Part.Thought bit from the Live API — the UI never infers thought-vs-final from content.
type responseMsg agent.ResponseChunk
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
		if m.activeToolReq != nil {
			// A request is already awaiting a user decision — queue this one instead of overwriting
			// activeToolReq (FINDING 3). It becomes active once the current one resolves.
			m.pendingToolReqs = append(m.pendingToolReqs, msg)
			return m, m.waitForToolRequest()
		}
		m.activateToolRequest(msg)
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
		m.updateViewport(false)
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
		// Checked before the mode branches below so it applies from every mode — ModeToolConfirm and ModeToolEdit each have their own KeyMsg switch that returns before reaching the hotkeys switch further down. Double-press-to-quit: the first press arms a ~1.5s confirmation window (shown in the hint area) instead of quitting immediately; only a second press within that window actually quits.
		if msg.Type == tea.KeyCtrlC {
			if m.quitConfirmArmed && time.Since(m.quitConfirmArmedAt) <= quitConfirmWindow {
				return m, tea.Quit
			}
			m.quitConfirmArmed = true
			m.quitConfirmArmedAt = time.Now()
			return m, nil
		}

		if m.mode == ModeToolConfirm {
			switch msg.Type {
			case tea.KeyEsc:
				m.messages = append(m.messages, Message{Sender: "tool", Content: "Command rejected."})
				m.activeToolReq.ResultChan <- "User rejected this command."
				m.mode = ModeBoth
				m.activeToolReq = nil
				m.popPendingToolReq()
				m.updateViewport(false)
				m.recalcViewportHeight()
				return m, nil
			case tea.KeyUp, tea.KeyDown:
				var cmd tea.Cmd
				m.hitlList, cmd = m.hitlList.Update(msg)
				return m, cmd
			case tea.KeyEnter:
				if i, ok := m.hitlList.SelectedItem().(commandItem); ok {
					switch i.title {
					case "Allow once":
						m.messages = append(m.messages, Message{Sender: "tool", Content: "Approved. Executing..."})
						go func(execute func() string, c chan<- string) {
							c <- execute()
						}(m.activeToolReq.Execute, m.activeToolReq.ResultChan)
						m.mode = ModeBoth
					case "Allow for session":
						m.messages = append(m.messages, Message{Sender: "tool", Content: "Approved for session. Executing..."})
						if m.activeToolReq.AllowKey != "" {
							m.agent.AllowedCmds.Store(m.activeToolReq.AllowKey, true)
						}
						go func(execute func() string, c chan<- string) {
							c <- execute()
						}(m.activeToolReq.Execute, m.activeToolReq.ResultChan)
						m.mode = ModeBoth
					case "Reject":
						m.messages = append(m.messages, Message{Sender: "tool", Content: "Command rejected."})
						m.activeToolReq.ResultChan <- "User rejected this command."
						m.mode = ModeBoth
					case "Suggest changes":
						m.textarea.SetValue(m.activeToolReq.EditableCommand)
						m.textarea.Focus()
						m.mode = ModeToolEdit
					}
					// "Suggest changes" is the one entry that isn't terminal: it hands the request on to ModeToolEdit, which still needs it to deliver a result. The other three have already sent theirs, so clearing here is right for them and a nil-deref crash for it.
					if m.mode != ModeToolEdit {
						m.activeToolReq = nil
						m.popPendingToolReq()
					}
					m.updateViewport(false)
					m.recalcViewportHeight()
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
				m.updateViewport(false)
				go func(cmd string, c chan<- string) {
					c <- agent.RunShellCommand(cmd)
				}(editedCmd, m.activeToolReq.ResultChan)
				m.mode = ModeBoth
				m.activeToolReq = nil
				m.popPendingToolReq()
				return m, nil
			}
			if msg.Type == tea.KeyEsc {
				m.textarea.Reset()
				m.messages = append(m.messages, Message{Sender: "tool", Content: "Edit cancelled. Command rejected."})
				m.activeToolReq.ResultChan <- "User rejected this command."
				m.mode = ModeBoth
				m.activeToolReq = nil
				m.popPendingToolReq()
				m.updateViewport(false)
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
					cmd := m.executeCommand(input)
					m.textarea.Reset()
					m.showCmdList = false
					m.recalcViewportHeight()
					return m, cmd
				}
			}
		}

		// hotkeys
		switch msg.Type {
		case tea.KeyPgDown:
			m.viewport.PageDown()
			return m, nil
		case tea.KeyPgUp:
			m.viewport.PageUp()
			return m, nil
		case tea.KeyCtrlD:
			// Only takes over when the textarea is empty — bubbles' textarea binds Ctrl+D to DeleteCharacterForward natively, and stealing it mid-sentence broke composing (X1). Falls through to the textarea-update section below when non-empty.
			if m.textarea.Value() == "" {
				m.viewport.HalfPageDown()
				return m, nil
			}
		case tea.KeyCtrlU:
			// Same textarea-empty gating as Ctrl+D above — bubbles binds this to DeleteBeforeCursor, and shell users clear the input line with it constantly (X1).
			if m.textarea.Value() == "" {
				m.viewport.HalfPageUp()
				return m, nil
			}
		case tea.KeyCtrlE:
			// Same textarea-empty gating as Ctrl+D/Ctrl+U above — bubbles binds this to LineEnd (X1).
			if m.textarea.Value() == "" {
				m.expandThoughts = !m.expandThoughts
				m.updateViewport(false)
				return m, nil
			}
		case tea.KeyEsc:
			// Esc never quits — see the top-of-KeyMsg Ctrl+C check for that. Context-scoped instead: close the open menu, else jump back to the bottom if scrolled away, else clear a non-empty textarea, else no-op. One rung per keypress, not all applicable rungs at once.
			if m.showCmdList {
				m.showCmdList = false
				m.recalcViewportHeight()
				return m, nil
			}
			if !m.viewport.AtBottom() {
				m.viewport.GotoBottom()
				return m, nil
			}
			if m.textarea.Value() != "" {
				m.textarea.Reset()
			}
			return m, nil
		case tea.KeyCtrlJ:
			m.textarea.InsertString("\n")
			return m, nil
		case tea.KeyEnter:
			input := strings.TrimSpace(m.textarea.Value())
			if input != "" {
				if strings.HasPrefix(input, "/") {
					cmd := m.executeCommand(input)
					m.textarea.Reset()
					m.showCmdList = false
					m.recalcViewportHeight()
					return m, cmd
				} else if m.mode == ModeVoice {
					// Voice-only: text sends are disabled. Only slash commands work.
					m.messages = append(m.messages, Message{Sender: "system", Content: "Text input disabled in voice mode. Use /both to enable."})
					m.textarea.Reset()
					m.updateViewport(false)
				} else {
					m.agent.TextChan <- input
					m.streamLine("you", input, false)
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
		m.recalcViewportHeight()

		return m, tiCmd

	case responseMsg:
		if msg.TurnBoundary {
			m.closeOraBlock = true
			return m, m.waitForResponse()
		}
		sender := "ora"
		switch msg.Sender {
		case agent.SenderYou:
			sender = "you"
		case agent.SenderSystem:
			sender = "system"
		}
		// Only ora/you chunks prove real server traffic — a system notice (e.g. "connection lost — reconnecting…") must not flip the header pill to live while the link is actually down.
		if sender != "system" {
			m.isConnected = true
		}
		// Only an actual ora reply clears the thinking spinner — a "you" transcription or a system notice isn't a reply arriving.
		if sender == "ora" {
			m.activity = nil
			m.recalcViewportHeight()
		}
		m.streamLine(sender, msg.Text, msg.IsThought)
		return m, m.waitForResponse()
	case errorMsg:
		m.isConnected = false
		m.activity = nil
		m.recalcViewportHeight()
		m.streamLine("system", "CONNECTION CRITICAL: "+msg.Error(), false)
		return m, m.waitForError()

	case daemonPollMsg:
		// tea.WindowSize() re-probes the terminal's actual current size and feeds it back in as a real WindowSizeMsg — a periodic self-heal in case a resize was missed (e.g. a tmux pane-only resize not delivering SIGWINCH the same way a full terminal resize does; see the WP10 addendum). Piggybacked on this existing 12s ticker rather than a new one. This can only be verified live — bubbletea's own size-probe plumbing isn't observable at the model.Update() level.
		return m, tea.Batch(m.daemonPollTick(), m.pollDaemonStatus(), tea.WindowSize())
	case daemonStatusMsg:
		m.daemonOK = bool(msg)
		return m, nil

	case tickMsg:
		// Safety valve — see staleActivityTimeout's doc comment. Only a dropped Finished event under a tool-call burst reaches this.
		if m.activity != nil && time.Since(m.activity.started) > staleActivityTimeout {
			m.activity = nil
			m.recalcViewportHeight()
		}

		// Clears the "press ctrl+c again to quit" hint once the confirm window lapses without a second press.
		if m.quitConfirmArmed && time.Since(m.quitConfirmArmedAt) > quitConfirmWindow {
			m.quitConfirmArmed = false
		}

		// Flush a render streamLine's throttle skipped — see viewportDirty's doc comment.
		if m.viewportDirty {
			m.updateViewport(false)
			m.viewportDirty = false
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
		// Logged so a live report of a stale/scrambled layout (e.g. a tmux pane resize) can be correlated against whether — and when — a resize was actually received; bubbletea's WindowSizeMsg relies on the OS delivering SIGWINCH to this process, which tmux pane-only resizes aren't guaranteed to do the same way a full terminal resize is.
		slog.Debug("window size changed", "width", msg.Width, "height", msg.Height)
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

		// cmdList/hitlList track the real input-deck content width (not the fixed DefaultListWidth they're constructed with) so a row's background band spans the whole deck instead of stopping at a stale 60 columns — set before recalcViewportHeight so its renderInput() measurement sees the resize too.
		deckWidth := max(0, msg.Width-m.styles.InputWrap.GetHorizontalPadding())
		m.cmdList.SetWidth(deckWidth)
		m.hitlList.SetWidth(deckWidth)

		m.viewport.Width = msg.Width
		m.recalcViewportHeight()
		m.textarea.SetWidth(msg.Width - GutterWidth)

		// split the width between the two waveforms
		waveWidth := max(MinWaveWidth, (msg.Width-MinWaveWidth)/2)
		m.micWave.SetWidth(waveWidth)
		m.speakerWave.SetWidth(waveWidth)
		m.updateViewport(false)
	}

	m.viewport, vpCmd = m.viewport.Update(msg)
	return m, tea.Batch(tiCmd, vpCmd)
}

// activateToolRequest makes req the pending approval the user is deciding on: opens the ModeToolConfirm
// menu, appends the "Ora wants to:" transcript line, and clears any live-status spinner since the approval
// menu already says "paused, waiting on you" — a spinner behind it would misleadingly read as "still running".
func (m *model) activateToolRequest(req agent.ToolRequest) {
	m.activeToolReq = &req
	m.mode = ModeToolConfirm
	items := hitlItems(req.EditableCommand != "")
	m.hitlList.SetItems(items)
	// Sized to the actual item count (3-4), not list.New's DefaultListHeight=8 default — with title/status/help/filtering all disabled, the list renders exactly item-count rows, so the fixed default was reserving 4-5 rows nobody used and could overflow a short terminal (see the WP7 finding this fixes).
	m.hitlList.SetHeight(len(items))
	m.messages = append(m.messages, Message{Sender: "tool", Content: "Ora wants to:\n  " + req.Description, IsTool: true})
	m.updateViewport(false)
	m.activity = nil
	m.recalcViewportHeight()
}

// popPendingToolReq promotes the next queued ToolRequest (FINDING 3) to active, if one is waiting. Callers
// invoke this every time they clear activeToolReq to nil, so a request queued behind a busy approval always
// gets its turn instead of being silently dropped.
func (m *model) popPendingToolReq() {
	if len(m.pendingToolReqs) == 0 {
		return
	}
	next := m.pendingToolReqs[0]
	m.pendingToolReqs = m.pendingToolReqs[1:]
	m.activateToolRequest(next)
}

// recalcViewportHeight is the single source of truth for viewport height: everything else on screen (signal field, input deck — status line, textarea, hints, and any open menu all included since they're all part of renderInput's own output) measured from an actual render rather than hand-maintained row-count constants, so it can't drift out of sync the way the old constant-based version silently did (a 7-row underestimate that pushed the input deck off-screen on any sufficiently tall transcript — see the WP5 bug report). Shared by WindowSizeMsg, tickMsg's textarea-grow branch, and every m.activity/showCmdList/ModeToolConfirm mutation site below, so the layout never overlaps or overflows a tick behind.
//
// The invariant View() height == m.height has NO exceptions (WP10) — bubbletea's renderer desyncs the moment it doesn't hold (see WP5/WP9). A menu open is the one case MinViewportHeight's normal floor can't guarantee that on a short terminal, so this drops the floor to 0 first and, if even that isn't enough, shrinks the open list itself as a last resort — the only two levers left once the viewport can't give any more.
func (m *model) recalcViewportHeight() {
	menuOpen := m.showCmdList || m.mode == ModeToolConfirm

	// Reset to the menu's full item count before measuring — otherwise a terminal that grows back after being squeezed by the last-resort shrink below would stay artificially truncated forever.
	if m.showCmdList {
		m.cmdList.SetHeight(len(m.cmdList.Items()))
	}
	if m.mode == ModeToolConfirm {
		m.hitlList.SetHeight(len(m.hitlList.Items()))
	}

	// paddingRows isolates the Viewport style's own vertical padding (Padding(2,4) as of writing), unambiguously: Render on one real content line, never Render("") — lipgloss's own line-counting floors at 1 line even for an empty string, so measuring against "" would silently fold that 1-line floor into what's supposed to be pure padding.
	paddingRows := lipgloss.Height(m.styles.Viewport.Render("x")) - 1

	chrome := lipgloss.Height(m.renderInput())
	if m.mode != ModeText {
		chrome += lipgloss.Height(m.renderSignalField())
	}

	// The floor drops to 0 unconditionally, not just when a menu is open — MinViewportHeight is a nicety
	// for the common case, but on a short terminal (see FINDING 8) it must never win over keeping the input
	// prompt on screen. Since floor only matters when available is already below it, this changes nothing
	// when there's ample room.
	available := m.height - chrome - paddingRows
	m.viewport.Height = max(0, available)

	// A rendered viewport is never actually 0 rows tall — bubbles' viewport.View() returns "" at Height=0, and lipgloss counts an empty string as 1 line same as any other — so the real practical minimum contribution is 1 content row, not 0.
	deficit := chrome + paddingRows + max(1, m.viewport.Height) - m.height

	// Last resort: even the practical minimum viewport doesn't close the gap — shrink the open menu list by exactly the shortfall. Each list row is exactly one line (WP7/WP9's delegate fixes), so trimming N rows off the list's height trims exactly N rows off renderInput()'s next measurement.
	if menuOpen && deficit > 0 {
		if m.mode == ModeToolConfirm {
			m.hitlList.SetHeight(max(0, m.hitlList.Height()-deficit))
		} else {
			m.cmdList.SetHeight(max(0, m.cmdList.Height()-deficit))
		}
	}
}

// handles the real-time streaming logic, keeps the viewport updated. isThought comes straight from genai's own Part.Thought (via agent.ResponseChunk) — never inferred from content, since any real reply that happens to contain markdown bold would desync a content-sniffing heuristic permanently.
func (m *model) streamLine(sender, content string, isThought bool) {
	// Only ora's own text merges across calls (it's a token-by-token stream); "you" and "system" chunks are complete discrete units — merging them runs consecutive utterances/notices together with no separator. A pending closeOraBlock (a turn boundary since the last ora chunk) also forces a fresh block even though sender/thought-state match — see its own doc comment.
	sameBlock := sender == "ora" && !m.closeOraBlock && len(m.messages) > 0 && m.messages[len(m.messages)-1].Sender == sender && m.messages[len(m.messages)-1].IsThought == isThought && !m.messages[len(m.messages)-1].IsTool
	if sameBlock && !isThought {
		mergeOraChunk(&m.messages[len(m.messages)-1], content)
	} else if sameBlock {
		// append to the last message if it's the same sender and thought-state
		m.messages[len(m.messages)-1].Content += content
	} else {
		// start a new message block
		m.messages = append(m.messages, Message{Sender: sender, Content: content, IsThought: isThought})
	}
	if sender == "ora" {
		m.closeOraBlock = false
	}

	// throttle viewport updates so it doesn't flicker too much
	if time.Since(m.lastUpdate) > 80*time.Millisecond || sender == "you" {
		m.updateViewport(sender == "you")
		m.lastUpdate = time.Now()
		m.viewportDirty = false
	} else {
		m.viewportDirty = true
	}
}

// executeCommand runs a "/" command. Returns a non-nil tea.Cmd only for /quit (immediate quit, no confirmation — unlike Ctrl+C, typing /quit is explicit); every other command returns nil and the caller falls back to its own default.
func (m *model) executeCommand(input string) tea.Cmd {
	switch input {
	case "/quit":
		return tea.Quit
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
  /help           - Show this help menu
  /quit           - Quit immediately (no confirmation)`
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
	m.updateViewport(false)
	return nil
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

	// Built as a slice so ModeText's empty signal field is OMITTED, not joined as a blank row — JoinVertical would otherwise count "" as one line same as recalcViewportHeight's chrome measurement doesn't, a 1-row version of the same overflow-past-the-terminal bug 2b fixed.
	blocks := []string{
		m.styles.Viewport.Width(m.width).Render(m.viewport.View()),
	}
	if sf := m.renderSignalField(); sf != "" {
		blocks = append(blocks, sf)
	}
	blocks = append(blocks, m.renderInput())

	// stack everything vertically
	content := lipgloss.JoinVertical(lipgloss.Left, blocks...)

	// Fills the screen to m.width x m.height (transcript area is transparent now — see message.go's renderMessage doc comment — so no whitespace background to paint; WithWhitespaceChars alone just pads with spaces, not color).
	return lipgloss.Place(
		m.width, m.height,
		lipgloss.Left, lipgloss.Top,
		m.styles.AppFrame.Render(content),
		lipgloss.WithWhitespaceChars(" "),
	)
}

func Run(a *agent.Agent, daemonStatus, buildMismatch string) error {
	p := tea.NewProgram(NewModel(a, daemonStatus, buildMismatch), tea.WithAltScreen(), tea.WithMouseCellMotion())
	_, err := p.Run()
	return err
}
