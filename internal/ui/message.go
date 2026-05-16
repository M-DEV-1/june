package ui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

type Message struct {
	Sender    string
	Content   string
	IsTool    bool
	IsThought bool
}

func (m *model) renderMessage(msg Message, width int) string {
	// Fixed width for alignment
	gutterWidth := GutterWidth
	contentWidth := width - gutterWidth

	// Base row style with no background forced, to rely on lipgloss.Place
	rowStyle := lipgloss.NewStyle().Width(width).UnsetBackground()

	var prefix, content string

	switch msg.Sender {
	case "system":
		if strings.Contains(msg.Content, "██") {
			return m.renderBanner(msg.Content, width)
		}
		prefix = m.styles.PrefixSystem.Render("system")
		content = m.styles.TextSystem.Width(contentWidth).Render(msg.Content)
	case "you":
		prefix = m.styles.PrefixYou.Render("you")
		content = m.styles.TextYou.Width(contentWidth).Render(msg.Content)
	case "ora":
		if msg.IsThought {
			prefix = m.styles.PrefixThought.Render("thought")
			content = m.styles.TextThought.Width(contentWidth).Render(msg.Content)
		} else {
			prefix = m.styles.PrefixOra.Render("ora")
			content = m.styles.TextOra.Width(contentWidth).Render(msg.Content)
		}
	case "tool":
		// Center the dot in the gutter
		spacer := strings.Repeat(" ", gutterWidth-2)
		prefix = spacer + m.styles.ToolDot.Render("●")
		content = m.styles.ToolText.Width(contentWidth).Render(msg.Content)
	}

	// Join them up with top alignment so prefixes don't jump around
	line := lipgloss.JoinHorizontal(lipgloss.Top, prefix, content)
	return rowStyle.Render(line)
}

// centered banner no background stripe to keep it clean
func (m *model) renderBanner(content string, width int) string {
	bannerLines := strings.Split(content, "\n")
	var centeredLines []string
	for _, l := range bannerLines {
		if strings.Contains(l, "██") {
			// center the ascii logo lines
			padding := max(0, (width-lipgloss.Width(l))/2)
			centeredLines = append(centeredLines, strings.Repeat(" ", padding)+m.styles.OraLogo.Render(l))
		} else if strings.TrimSpace(l) != "" {
			// center and style the tagline
			padding := max(0, (width-lipgloss.Width(l))/2)
			centeredLines = append(centeredLines, strings.Repeat(" ", padding)+m.styles.HeaderPath.Render(l))
		} else {
			centeredLines = append(centeredLines, "")
		}
	}
	return strings.Join(centeredLines, "\n")
}

func (m *model) updateViewport() {
	var wrapped []string

	// internal width accounting for the padding
	renderWidth := m.viewport.Width - 4

	for _, msg := range m.messages {
		wrapped = append(wrapped, m.renderMessage(msg, renderWidth))
	}

	m.viewport.SetContent(strings.Join(wrapped, "\n\n"))
	m.viewport.GotoBottom()
}
