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
	gutterWidth := GutterWidth
	contentWidth := width - gutterWidth

	// Explicit background on every row prevents terminal bleed-through in viewport.
	rowStyle := lipgloss.NewStyle().Width(width).Background(m.styles.BgViewport)

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

	// account for the viewport's own padding (2,4) → 8 chars horizontal
	renderWidth := m.viewport.Width - 8
	if renderWidth < 20 {
		renderWidth = 20
	}

	// Styled blank line so the separator between messages has the same background
	// as the viewport. Plain "\n\n" shows the terminal color through.
	sep := lipgloss.NewStyle().Background(m.styles.BgViewport).Width(renderWidth).Render("")

	for _, msg := range m.messages {
		wrapped = append(wrapped, m.renderMessage(msg, renderWidth))
	}

	m.viewport.SetContent(strings.Join(wrapped, "\n"+sep+"\n"))
	m.viewport.GotoBottom()
}
