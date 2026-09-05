// delegate.go is the smallest first version of handing a bounded piece of work to another agent — Claude Code running as a real collaborator in a project directory, rather than the sandboxed, tool-less "answer this question" mode AskClaude runs its own asks in (claude.go). Everything here is self-contained: nothing outside this file is edited, and the tool table reaches it through delegateTool and delegateHandler at the bottom (registered in tools.go).
package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"ora/internal/db"

	"google.golang.org/genai"
)

// Delegation is one request to hand a goal to another agent. To names the target — "claude" is the only one this version runs. Brief is the goal in the user's own words. CWD is the project directory to run the delegate in, so it works in a real repo rather than Ora's own sandbox; empty means the daemon's own working directory. Wait says whether the caller blocks for the result; this version always waits, but the field is here so a later fire-and-forget version does not change the shape callers already build.
type Delegation struct {
	To    string
	Brief string
	CWD   string
	Wait  bool
}

// Runner runs one delegate session to completion. Input: the working directory to run in, the system prompt (the brief) and the prompt (what to do). Output: the delegate's plain-text answer, or an error naming what went wrong. A test replaces this with a stub so Delegate runs without starting a CLI.
type Runner interface {
	Run(ctx context.Context, cwd, systemPrompt, prompt string) (string, error)
}

// claudeCodeBinary is the Claude Code command line, found on PATH — the same binary claude.go's ask path runs.
const claudeCodeBinary = "claude"

// ClaudeCodeRunner runs a delegate session through `claude -p` in the caller's own project directory, under the CLI's normal default permission mode — not the --restricted, no-tools sandbox AskClaude uses for its own asks, because a delegate call is a real hand-off to a collaborator working in a real project. --bare is never passed (it would bill the API key instead of the subscription, the same reason claude.go never passes it) and --dangerously-skip-permissions is never passed: in this headless `-p` run there is nobody to answer a permission prompt, so under --permission-mode default an approval-gated tool is simply denied rather than run unsupervised — that denial, plus the "do not send, publish, pay for or delete" line BuildBrief writes into every brief, is the actual guard, not a live approval gate.
type ClaudeCodeRunner struct{}

// newDelegateCmd builds (without starting) the `claude -p` command for one delegate run. Input: the context whose deadline bounds the run, the working directory (cwd, "" for the caller's own), and the path of the system-prompt file already written to disk. Output: the exec.Cmd, not yet given stdin/stdout/stderr — a test can inspect its process-group and cancel wiring without ever starting the real claude binary. The child runs in its own process group (SysProcAttr.Setpgid) and Cancel kills the whole group, not just the direct child, so a `npm run dev` or watcher the delegate started does not outlive the ten-minute wall or the caller's own context — killing only the direct process leaves such grandchildren running with nobody to stop them. WaitDelay still bounds how long Run waits for stdout to close once Cancel has fired.
func newDelegateCmd(ctx context.Context, cwd, promptPath string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, claudeCodeBinary, "-p", "--output-format", "text", "--system-prompt-file", promptPath, "--permission-mode", "default")
	if cwd != "" {
		cmd.Dir = cwd
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	return cmd
}

// Run starts one `claude -p` process in cwd. The system prompt goes to a 0600 file in a fresh temp directory rather than argv, the same reason writeClaudeAskFiles does it in claude.go: a single argv entry is capped at 128 KB on Linux and any local process can read another's argv via /proc/<pid>/cmdline. The prompt goes on stdin. Output: stdout, trimmed, as the result.
func (ClaudeCodeRunner) Run(ctx context.Context, cwd, systemPrompt, prompt string) (string, error) {
	dir, err := os.MkdirTemp("", "ora-delegate-")
	if err != nil {
		return "", fmt.Errorf("delegate: making the system prompt's temp dir: %w", err)
	}
	defer os.RemoveAll(dir)
	promptPath := filepath.Join(dir, "system-prompt.txt")
	if err := os.WriteFile(promptPath, []byte(systemPrompt), 0o600); err != nil {
		return "", fmt.Errorf("delegate: writing the system prompt: %w", err)
	}

	cmd := newDelegateCmd(ctx, cwd, promptPath)
	cmd.Stdin = strings.NewReader(prompt)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", errors.New("delegate: the run timed out")
		}
		return "", fmt.Errorf("delegate: %w: %s", err, claudeHead(stderr.String()))
	}
	text := strings.TrimSpace(out.String())
	if text == "" {
		return "", errors.New("delegate: the run returned no text")
	}
	return text, nil
}

// delegateThreadBudget bounds how much of the conversation thread a brief carries, in runes, newest kept — sized for a one-page brief rather than a full model prompt (compare maxHistoryBytes in ask.go, which bounds an ask's own thread the same way).
const delegateThreadBudget = 4000

// BuildBrief writes the one page handed to a delegate. Input: the goal in the user's own words, the conversation thread it grew out of (oldest first, as db.Store.ConversationTurns returns it — nil for none), and the personal-context block to include (as personalContextBlock renders it, "" for none). Output: the brief text: the goal, what Ora knows of the thread so far (the newest lines that fit delegateThreadBudget runes), the personal-context block, the constraints every delegate call carries, and where to report. Every one of those three sources — goal, thread, personal context — is filtered line by line through redactLine before it is written; see that function's own comment for exactly what it catches and what still gets through.
func BuildBrief(goal string, thread []db.Turn, personal string) string {
	var b strings.Builder
	b.WriteString("Goal: " + strings.TrimSpace(redactBlock(goal)) + "\n")

	lines := make([]string, 0, len(thread))
	for _, t := range thread {
		if t.Kind == "error" {
			continue
		}
		text := strings.TrimSpace(t.Text)
		if text == "" {
			continue
		}
		who := "ora:"
		if t.Role == "you" {
			who = "user:"
		}
		line := who + " " + text
		if redactLine(line) {
			continue
		}
		lines = append(lines, line)
	}
	lines = keepNewestRunes(lines, delegateThreadBudget)

	b.WriteString("\nWhat Ora knows:\n")
	if len(lines) == 0 {
		b.WriteString("(nothing said in this conversation yet)\n")
	} else {
		for _, l := range lines {
			b.WriteString(l + "\n")
		}
	}

	if p := strings.TrimSpace(redactBlock(personal)); p != "" {
		b.WriteString("\n" + p + "\n")
	}

	b.WriteString("\nConstraints: do not send, publish, pay for or delete anything. If the work needs one of those, stop and ask a question instead of doing it — report what you need and why. Report your result as plain text.\n")
	b.WriteString("\nWhere to report: your plain-text reply is the whole result; it is filed as the answer to this delegation.\n")
	return b.String()
}

// keepNewestRunes keeps the newest lines of lines that together fit within budget runes, measured backwards from the end so a thread over budget always loses its oldest part first — the same rule HistoryFromTurns applies to an ask's own thread (ask.go), in runes rather than bytes since a brief is read as text, not billed as tokens. The newest line is kept whatever its own size.
func keepNewestRunes(lines []string, budget int) []string {
	size, first := 0, len(lines)
	for i := len(lines) - 1; i >= 0; i-- {
		size += utf8.RuneCountInString(lines[i]) + 1
		if size > budget && i < len(lines)-1 {
			break
		}
		first = i
	}
	return lines[first:]
}

// redactLine reports whether line names a secret that must never leave the machine in a delegate brief: the field-label words secretPattern matches (password, card, cvv, cvc, otp, pin, account number — stopline.go), or one of the credential-file patterns isSensitivePath matches (.ssh/, .env, id_rsa, a .pem/.key suffix, and so on — tools.go). A matching line is dropped whole rather than partially redacted, since guessing which part of a sentence is the secret is how the rest of it leaks anyway. What it does not catch: a bare secret with no label word and no path shape — a raw API key or token pasted with nothing around it — passes through untouched, since both patterns key off a label or a path, not the shape of the value itself.
func redactLine(line string) bool {
	return secretPattern.MatchString(line) || isSensitivePath(line)
}

// redactBlock filters s line by line through redactLine, dropping any matching line and rejoining what is left with newlines. Input: any block of text bound for a delegate brief (the goal, or the personal-context block) — not just a conversation thread. Output: the same text with every secret-naming line removed.
func redactBlock(s string) string {
	lines := strings.Split(s, "\n")
	kept := lines[:0]
	for _, l := range lines {
		if redactLine(l) {
			continue
		}
		kept = append(kept, l)
	}
	return strings.Join(kept, "\n")
}

// delegateTimeout bounds one whole delegate call: long enough for a real Claude Code session to do a bounded piece of work, short enough that a stuck delegate does not hold its caller open indefinitely.
// ponytail: one fixed budget, not per-call configurable; add a field on Delegation if a caller ever needs a shorter or longer wall clock than this.
const delegateTimeout = 10 * time.Minute

// Delegate hands one goal to another agent and waits for its answer. Input: the delegation (who to hand it to, the goal, the working directory) and the conversation thread it grew out of, oldest first, nil for none. Output: the delegate's plain-text answer, or an error naming what went wrong — including a timeout error once delegateTimeout has passed.
func (a *Agent) Delegate(ctx context.Context, d Delegation, thread []db.Turn) (string, error) {
	return a.delegate(ctx, ClaudeCodeRunner{}, d, thread)
}

// delegate is Delegate against the given runner, so a test can drive a whole delegate call without starting the CLI.
func (a *Agent) delegate(ctx context.Context, run Runner, d Delegation, thread []db.Turn) (string, error) {
	if strings.TrimSpace(d.Brief) == "" {
		return "", errors.New("delegate: needs a brief")
	}
	if d.To != "" && d.To != "claude" {
		return "", fmt.Errorf("delegate: %q is not a delegate Ora can run", d.To)
	}
	if d.CWD != "" {
		info, err := os.Stat(d.CWD)
		if err != nil {
			return "", fmt.Errorf("delegate: cwd %q: %w", d.CWD, err)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("delegate: cwd %q is not a directory", d.CWD)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, delegateTimeout)
	defer cancel()

	var personal string
	if a.brain != nil {
		if entries, err := a.brain.PersonalContext(ctx); err != nil {
			slog.Warn("delegate: reading personal context failed, continuing without it", "error", err)
		} else {
			personal = personalContextBlock(entries)
		}
	}

	brief := BuildBrief(d.Brief, thread, personal)
	// The goal goes to the delegate twice, inside the brief and as the prompt itself, so it is redacted on both paths: a secret the user pasted must not reach the child on stdin after the system prompt stripped it.
	started := time.Now()
	result, err := run.Run(ctx, d.CWD, brief, redactBlock(d.Brief))
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			// The deadline that fired may be the delegate's own budget or a shorter one the caller set, so the message says how long it actually ran rather than naming a budget that may not have been the one that ended it.
			return "", fmt.Errorf("delegate: timed out after %s", ranFor(time.Since(started)))
		}
		return "", err
	}
	return strings.TrimSpace(result), nil
}

// delegateTool is the declaration for a "delegate" tool, shaped exactly like every entry in toolDefinitions()'s slice (tools.go) — wiring it in is appending this value to that slice's FunctionDeclarations.
var delegateTool = &genai.FunctionDeclaration{
	Behavior: genai.BehaviorNonBlocking,
	Name:     "delegate",
	Description: "Hand a bounded piece of work to Claude Code running as a real collaborator in a project directory, and wait for its plain-text answer. Use this for actual coding or shell work in a project, not a memory question — query_memory/recall/branch answer those instead. " +
		"The delegate sees the user's personal context and nothing else; it cannot see this conversation, the screen or the rest of Ora's memory, so the brief has to carry everything it needs.",
	Parameters: &genai.Schema{
		Type: genai.TypeObject,
		Properties: map[string]*genai.Schema{
			"to":    {Type: genai.TypeString, Description: "Who to hand the work to. Only \"claude\" is supported today."},
			"brief": {Type: genai.TypeString, Description: "The goal to hand over, in plain language — what the delegate should do and how to know it is done."},
			"cwd":   {Type: genai.TypeString, Description: "The project directory to run the delegate in. Defaults to Ora's own working directory when left out."},
		},
		Required: []string{"brief"},
	},
}

// delegateHandler runs the "delegate" tool: parses its arguments and hands the brief to Agent.Delegate, rendering the result the way every case in executeTool's switch does (tools.go). Shaped as `func(ctx, a, args) string` so wiring it in is one line: `case "delegate": return delegateHandler(ctx, a, args)`. This version has no conversation thread to attach — that needs executeTool's own signature to carry one, which is the caller's edit to make alongside registering the case.
func delegateHandler(ctx context.Context, a *Agent, args map[string]any) string {
	brief, ok := args["brief"].(string)
	if !ok || strings.TrimSpace(brief) == "" {
		return toolError("delegate needs a brief describing the work")
	}
	cwd, _ := args["cwd"].(string)
	to, _ := args["to"].(string)
	result, err := a.Delegate(ctx, Delegation{To: to, Brief: brief, CWD: cwd}, nil)
	if err != nil {
		slog.Error("delegate: failed", "error", err)
		return toolError("that delegate call failed: " + err.Error())
	}
	return result
}

// ranFor rounds a run's length for a message: to the second once it ran that long, to the millisecond under a second, so a run a short deadline ended never reads as "0s". Input: the duration. Output: the rounded duration.
func ranFor(d time.Duration) time.Duration {
	if d < time.Second {
		return d.Round(time.Millisecond)
	}
	return d.Round(time.Second)
}
