package ui

import (
	"fmt"
	"strings"

	"ora/internal/agent"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type model struct {
	agent    *agent.Agent
	viewport viewport.Model
	textarea textarea.Model
	messages []string
	err      error
}

type responseMsg string

func NewModel(a *agent.Agent) model {
	ta := textarea.New()
	ta.Placeholder = "Type a message or /command..."
	ta.Focus()

	ta.Prompt = "┃ "
	ta.CharLimit = 1000
	ta.SetWidth(80)
	ta.SetHeight(3)

	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.ShowLineNumbers = false

	vp := viewport.New(80, 20)
	vp.SetContent("Welcome to Ora. Press Ctrl+C to exit.\n\n")

	return model{
		agent:    a,
		textarea: ta,
		viewport: vp,
		messages: []string{},
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(
		textarea.Blink,
		m.waitForResponse(),
	)
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
				// send to agent
				m.agent.TextChan <- input
				m.messages = append(m.messages, lipgloss.NewStyle().Foreground(lipgloss.Color("5")).Render("You: ")+input)
				m.textarea.Reset()
				m.updateViewport()
			}
		}

	case responseMsg:
		m.messages = append(m.messages, lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Render("Ora: ")+string(msg))
		m.updateViewport()
		return m, m.waitForResponse()

	case tea.WindowSizeMsg:
		m.viewport.Width = msg.Width
		m.textarea.SetWidth(msg.Width)
		m.viewport.Height = msg.Height - m.textarea.Height() - 2
	}

	return m, tea.Batch(tiCmd, vpCmd)
}

func (m *model) updateViewport() {
	m.viewport.SetContent(strings.Join(m.messages, "\n\n"))
	m.viewport.GotoBottom()
}

func (m model) View() string {
	return fmt.Sprintf(
		"%s\n\n%s",
		m.viewport.View(),
		m.textarea.View(),
	) + "\n\n"
}

func Run(a *agent.Agent) error {
	p := tea.NewProgram(NewModel(a), tea.WithAltScreen())
	_, err := p.Run()
	return err
}
