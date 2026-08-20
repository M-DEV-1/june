package companion

import (
	"fmt"
	"html"
	"strings"
	"time"

	"ora/evals/dataset"
	"ora/internal/agent"
)

// Pair is one question run on both APIs.
type Pair struct {
	Voice      agent.TurnTrace
	Text       agent.TurnTrace
	VoiceScore Score
	TextScore  Score
}

// RenderHTML is a self-contained report of the cache probe plus any live voice/text traces. Input: probes (always), pairs (optional live runs). Output: HTML document.
func RenderHTML(probes []Probe, pairs []Pair) string {
	var b strings.Builder
	b.WriteString(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Ora companion eval</title><style>
:root{--bg:#0f1115;--card:#181b22;--ink:#e8eaef;--muted:#9aa3b2;--line:#2a3140;--yes:#3dd68c;--no:#ff6b6b;--mid:#f5c542;--chip:#232836}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--ink);font:15px/1.5 ui-sans-serif,system-ui,sans-serif}
main{max-width:1100px;margin:0 auto;padding:32px 20px 64px}h1{font-size:28px;margin:0 0 8px}h2{font-size:18px;margin:32px 0 12px}
p.lead{color:var(--muted);margin:0 0 24px}table{width:100%;border-collapse:collapse;background:var(--card);border-radius:12px;overflow:hidden}
th,td{text-align:left;padding:10px 12px;border-bottom:1px solid var(--line);vertical-align:top}th{color:var(--muted);font-weight:600;font-size:12px;text-transform:uppercase;letter-spacing:.04em}
.chip{display:inline-block;padding:2px 8px;border-radius:999px;background:var(--chip);font-size:12px}
.yes{color:var(--yes)}.no{color:var(--no)}.mid{color:var(--mid)}
.trace{background:var(--card);border-radius:12px;padding:16px 18px;margin:0 0 16px;border:1px solid var(--line)}
.trace h3{margin:0 0 8px;font-size:16px}pre{white-space:pre-wrap;word-break:break-word;background:#0c0e13;padding:12px;border-radius:8px;font-size:13px;color:#d7dbe7}
.cols{display:grid;grid-template-columns:1fr 1fr;gap:12px}@media(max-width:800px){.cols{grid-template-columns:1fr}}
.k{color:var(--muted);font-size:12px;text-transform:uppercase;letter-spacing:.04em}
</style></head><body><main>`)
	fmt.Fprintf(&b, "<h1>Ora companion eval</h1><p class=\"lead\">Frozen handshake vs tools vs two APIs. Generated %s.</p>", html.EscapeString(time.Now().Format(time.RFC3339)))

	b.WriteString("<h2>Cache probe (no Gemini)</h2><p class=\"lead\">Recency is relative to the latest row in sqlite, not wall-clock. Stale episodic hits must be dated; calling a three-week-old thread \"recent\" is a miss. Voice turns never call RetrieveRelevant.</p>")
	b.WriteString("<table><tr><th>id</th><th>when</th><th>ask</th><th>handshake</th><th>retrieve</th><th>query_memory</th><th>get_recent</th><th>diagnosis</th></tr>")
	var hs, ret, tool, miss int
	for _, p := range probes {
		if p.HandshakeHas {
			hs++
		}
		if p.RetrieveHas {
			ret++
		}
		if p.QueryHas || p.RecentHas {
			tool++
		}
		if !p.HandshakeHas && !p.RetrieveHas && !p.QueryHas && !p.RecentHas {
			miss++
		}
		fmt.Fprintf(&b, "<tr><td><span class=\"chip\">%s</span></td><td>%s</td><td>%s</td><td class=\"%s\">%s</td><td class=\"%s\">%s</td><td class=\"%s\">%s</td><td class=\"%s\">%s</td><td>%s</td></tr>",
			html.EscapeString(p.Question.ID),
			html.EscapeString(whenLabel(p.Question)),
			html.EscapeString(p.Question.Ask),
			ynClass(p.HandshakeHas), yn(p.HandshakeHas),
			ynClass(p.RetrieveHas), yn(p.RetrieveHas),
			ynClass(p.QueryHas), yn(p.QueryHas),
			ynClass(p.RecentHas), yn(p.RecentHas),
			html.EscapeString(p.Diagnosis),
		)
	}
	b.WriteString("</table>")
	fmt.Fprintf(&b, "<p class=\"lead\">%d questions · handshake %d · retrieve inject %d · tools %d · store miss %d</p>", len(probes), hs, ret, tool, miss)

	if len(pairs) > 0 {
		b.WriteString("<h2>Voice vs text</h2>")
		for _, pair := range pairs {
			q := pair.Voice.Question
			if q == "" {
				q = pair.Text.Question
			}
			fmt.Fprintf(&b, "<div class=\"trace\"><h3>%s</h3><p>%s</p><div class=\"cols\">", html.EscapeString(q), voiceTextSummary(pair))
			writeTrace(&b, "voice", pair.Voice, pair.VoiceScore)
			writeTrace(&b, "text", pair.Text, pair.TextScore)
			b.WriteString("</div></div>")
		}
	}
	b.WriteString("</main></body></html>")
	return b.String()
}

func writeTrace(b *strings.Builder, label string, tr agent.TurnTrace, s Score) {
	fmt.Fprintf(b, "<div><p class=\"k\">%s · %s · %s</p>", html.EscapeString(label), html.EscapeString(tr.Model), tr.Duration.Round(time.Millisecond))
	fmt.Fprintf(b, "<p>answer %s · thought %s · tools %s · handshake %s · time %s</p>", yn(s.AnswerHit), yn(s.ThoughtHit), yn(s.ToolResultHit), yn(s.HandshakeHit), recencyLabel(s))
	if s.RecencyNote != "" {
		fmt.Fprintf(b, "<p class=\"k\">%s</p>", html.EscapeString(s.RecencyNote))
	}
	if len(s.ToolNames) > 0 {
		fmt.Fprintf(b, "<p>called %s</p>", html.EscapeString(strings.Join(s.ToolNames, ", ")))
	}
	if len(tr.Thoughts) > 0 {
		fmt.Fprintf(b, "<p class=\"k\">thoughts</p><pre>%s</pre>", html.EscapeString(strings.Join(tr.Thoughts, "\n\n")))
	} else {
		b.WriteString("<p class=\"k\">thoughts</p><pre>(none surfaced)</pre>")
	}
	fmt.Fprintf(b, "<p class=\"k\">answer</p><pre>%s</pre></div>", html.EscapeString(empty(tr.Answer)))
}

func voiceTextSummary(p Pair) string {
	switch {
	case p.VoiceScore.AnswerHit && p.TextScore.AnswerHit:
		return "both answered"
	case p.TextScore.AnswerHit && !p.VoiceScore.AnswerHit:
		return "text hit, voice missed — the usual cache/talkativeness gap"
	case p.VoiceScore.AnswerHit && !p.TextScore.AnswerHit:
		return "voice hit, text missed"
	default:
		return "both missed"
	}
}

func yn(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

func ynClass(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

func empty(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(empty)"
	}
	return s
}

func whenLabel(q dataset.Question) string {
	if q.AgeLabel != "" {
		return q.Horizon + " · " + q.AgeLabel
	}
	if q.Horizon != "" {
		return q.Horizon
	}
	return q.Kind
}

func recencyLabel(s Score) string {
	switch {
	case s.PresentSlip:
		return "present-slip"
	case s.TimeOK:
		return "ok"
	default:
		return "undated"
	}
}
