package ui

import (
	oratext "ora/internal/text"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

type Message struct {
	Sender    string
	Content   string
	IsTool    bool
	IsThought bool
	// IsToolLog marks a passive "tool ran" entry — different glyph/style than IsTool, which is reserved for the HITL confirm/approve/reject flow that demands user action.
	IsToolLog bool
	// ToolLogFailed marks an IsToolLog entry whose tool call errored (agent.ToolActivity.Err), rendered in a distinct color so it doesn't look like a normal completed call.
	ToolLogFailed bool
}

// collapsedThoughtRunes bounds the preview shown for a folded thought message.
const collapsedThoughtRunes = 60

// collapsedThoughtPreview renders a thought's first collapsedThoughtRunes runes as one flattened line (streamed content can carry its own newlines) plus a fixed expand hint — always appended, even for a thought shorter than the cap, since it's also the affordance telling the user Ctrl+E exists.
func collapsedThoughtPreview(content string) string {
	return oratext.Runes(oratext.OneLine(content), collapsedThoughtRunes) + "… (ctrl+e expands)"
}

// restartDetectionMinRunes is the minimum rune length an incoming ora chunk must have to be treated as a restarted utterance (see mergeOraChunk) rather than appended. The Live API's native-audio models are documented to sometimes restart an utterance from the beginning — often triggered by the model's own voice echoing into a hot mic — getting a little further each time, with no interrupted:true between attempts. A short chunk that happens to coincide with the block's own opening (a legitimately repeated short word) is common enough that a low threshold would misfire on it; ~10 runes is well past what any real short word collides on.
const restartDetectionMinRunes = 10

// mergeOraChunk folds an incoming non-thought ora chunk into the block's existing content, handling three ways OutputTranscription can resend text instead of cleanly continuing it:
//  1. Restart: the chunk is itself a prefix of the block so far (>= restartDetectionMinRunes) — the model restarted its utterance from the beginning, so the block resets to the new chunk instead of appending, collapsing a restart loop down to its final, longest attempt.
//  2. Cumulative snapshot: the block so far is a prefix of the chunk — the chunk is the whole utterance-to-date rather than a delta, so the block is replaced wholesale. Kept as defense even though OutputTranscription is documented as incremental fragments in practice.
//  3. Exact resend: the block already ends with the chunk — a duplicate re-send, dropped as a no-op.
//
// Anything else is a normal incremental fragment and is appended as-is.
func mergeOraChunk(msg *Message, content string) {
	switch {
	case len([]rune(content)) >= restartDetectionMinRunes && strings.HasPrefix(msg.Content, content):
		msg.Content = content
	case strings.HasPrefix(content, msg.Content):
		msg.Content = content
	case strings.HasSuffix(msg.Content, content):
		// exact resend of the tail — drop it
	default:
		msg.Content += content
	}
}

// cellWidthSafetyMargin shaves a couple of cells off the content column before word-wrapping, instead of wrapping exactly at width-gutterWidth. lipgloss/go-runewidth's cell-width math is internally self-consistent (see TestRenderMessage_MalayalamContent_EveryRenderedLineFitsWidth) but a real terminal's font can still disagree with it, most often on conjuncts/combining marks in complex scripts — this margin is cheap headroom against that, not a fix for a headlessly-provable bug.
const cellWidthSafetyMargin = 2

func (m *model) renderMessage(msg Message, width int) string {
	gutterWidth := GutterWidth
	contentWidth := width - gutterWidth - cellWidthSafetyMargin

	rowStyle := lipgloss.NewStyle().Width(width)

	// Trimmed only here, at render time — streamLine's own accumulation keeps the raw content untouched (a restart/snapshot merge in mergeOraChunk still needs the real trailing bytes). Thought parts commonly end in one or more blank lines, which otherwise render as empty bordered rows inside the thought box.
	displayContent := strings.TrimRight(msg.Content, "\n \t")

	var prefix, content string

	switch msg.Sender {
	case "system":
		if strings.Contains(msg.Content, "██") {
			return m.renderBanner(msg.Content, width)
		}
		prefix = m.styles.PrefixSystem.Render("system")
		content = m.styles.TextSystem.Width(contentWidth).Render(displayContent)
	case "you":
		prefix = m.styles.PrefixYou.Render("you")
		content = m.styles.TextYou.Width(contentWidth).Render(displayContent)
	case "ora":
		if msg.IsThought {
			prefix = m.styles.PrefixThought.Render("thought")
			if m.expandThoughts {
				content = m.styles.TextThought.Width(contentWidth).Render(displayContent)
			} else {
				collapsedStyle := lipgloss.NewStyle().Foreground(m.styles.Muted)
				content = collapsedStyle.Width(contentWidth).Render(collapsedThoughtPreview(displayContent))
			}
		} else {
			prefix = m.styles.PrefixOra.Render("ora")
			content = m.styles.TextOra.Width(contentWidth).Render(displayContent)
		}
	case "tool":
		// Center the dot in the gutter
		spacer := strings.Repeat(" ", gutterWidth-2)
		if msg.IsToolLog {
			// Passive "tool ran" record — distinct glyph from the HITL "●" above. Failed calls get their own color.
			dotStyle, textStyle := m.styles.ToolLogDot, m.styles.ToolLogText
			if msg.ToolLogFailed {
				dotStyle, textStyle = m.styles.ToolLogFailedDot, m.styles.ToolLogFailedText
			}
			prefix = spacer + dotStyle.Render("⏺ ")
			content = textStyle.Width(contentWidth).Render(displayContent)
		} else {
			prefix = spacer + m.styles.ToolDot.Render("●")
			content = m.styles.ToolText.Width(contentWidth).Render(displayContent)
		}
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

// updateViewport re-renders the transcript. force=true always scrolls to the bottom (a "you" send — the user just typed something, so show it). Otherwise it only follows to the bottom if the viewport was already there before this render — new content arriving while the user has scrolled up to read history shouldn't yank them back down.
func (m *model) updateViewport(force bool) {
	var wrapped []string

	// account for the viewport's own padding (2,4) → 8 chars horizontal
	renderWidth := m.viewport.Width - 8
	if renderWidth < 20 {
		renderWidth = 20
	}

	// Blank line between messages — no styling needed, the terminal's own background shows through everywhere in the transcript now (see renderMessage's doc comment).
	sep := lipgloss.NewStyle().Width(renderWidth).Render("")

	for _, msg := range m.messages {
		wrapped = append(wrapped, m.renderMessage(msg, renderWidth))
	}

	wasAtBottom := m.viewport.AtBottom()
	m.viewport.SetContent(strings.Join(wrapped, "\n"+sep+"\n"))
	if force || wasAtBottom {
		m.viewport.GotoBottom()
	}
}
