package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"google.golang.org/genai"

	"ora/internal/config"
	"ora/internal/obs"
	"ora/internal/tracker"
)

// GeminiSummarizer implements Summarizer via the genai SDK. Other providers can implement the same interface later.
type GeminiSummarizer struct {
	client *genai.Client
	// identity, when set, returns who the user is (the personal-context "identity" entry) so the attribution prompt can say it. Without it the model reads a calendar entry naming the user and writes them up as somebody they met.
	identity func(context.Context) string
	// backendMu guards stateBackend, backgroundFallback and gate, which the daemon installs after the background goroutines that read them have already started.
	backendMu sync.RWMutex
	// stateBackend, when set, answers DeriveState's prompt instead of the Gemini API, so the unattended working-state job can run on the local llama-server and spend no metered quota. See SetStateBackend.
	stateBackend TextBackend
	// backgroundFallback, when set, answers a background prompt whose Gemini call came back 429 or 503. See SetBackgroundFallback.
	backgroundFallback TextBackend
	// gate, when set, is checked before every direct Gemini request this summarizer makes. See SetRequestGate.
	gate RequestGate
}

// SetIdentity gives the summarizer a way to look up who the user is at prompt time. It is a lookup rather than a string because the entry can be corrected mid-session.
func (g *GeminiSummarizer) SetIdentity(lookup func(context.Context) string) {
	g.identity = lookup
}

// SetRequestGate installs the check every direct Gemini request runs first, so this summarizer's calls count against the same shared daily quota as the daemon's other Gemini-routed brains. A nil gate (the default) leaves every call unmetered.
func (g *GeminiSummarizer) SetRequestGate(gate RequestGate) {
	g.backendMu.Lock()
	defer g.backendMu.Unlock()
	g.gate = gate
}

// gateFn reads the installed request gate under the lock, for the same reason stateBackendFn does: a background goroutine must never race the daemon installing one.
func (g *GeminiSummarizer) gateFn() RequestGate {
	g.backendMu.RLock()
	defer g.backendMu.RUnlock()
	return g.gate
}

// allow checks the installed gate, if any, before a call spends a request on model. Output: nil when the call may proceed.
func (g *GeminiSummarizer) allow(model string) error {
	if gate := g.gateFn(); gate != nil {
		return gate.Allow(model)
	}
	return nil
}

func NewGeminiSummarizer(apiKey string) (*GeminiSummarizer, error) {
	ctx := context.Background()
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return nil, err
	}

	return &GeminiSummarizer{
		client: client,
	}, nil
}

// ReconcileNotes calls TextModel with structured JSON output to decide, per candidate, whether to add, update, or skip relative to existing notes.
func (g *GeminiSummarizer) ReconcileNotes(ctx context.Context, existing []NoteRef, candidates []string) ([]NoteOp, error) {
	if len(candidates) == 0 {
		return nil, nil
	}

	tracer := obs.GetTracer(ctx, "ora.memory")
	ctx, span := tracer.Start(ctx, "GeminiSummarizer.ReconcileNotes")
	defer span.End()

	// The genai client has no HTTP timeout of its own, so every call this type makes gets one here.
	ctx, cancel := context.WithTimeout(ctx, summarizerCallTimeout)
	defer cancel()

	var existingLines []string
	for _, n := range existing {
		existingLines = append(existingLines, fmt.Sprintf("%d: %s", n.ID, n.Content))
	}

	prompt := fmt.Sprintf(`You are a memory deduplication engine. You will be given a list of existing stored facts (id: content) and a list of candidate new facts. For EACH candidate decide:
- "add"    — genuinely new, durable information not covered by ANY existing note. Use sparingly.
- "update" — relates to, refines, supersedes, or overlaps an existing note. STRONGLY prefer this over "add" whenever the candidate is about the same topic/project/preference as an existing note; provide that note's id and the merged content. Default to merging.
- "skip"   — already known, OR the candidate is transient task detail (a command, flag, path, version, today's bug) rather than a durable user trait. Drop those.

Bias hard toward "update" and "skip": the goal is a small, stable set of facts, NOT an ever-growing list. Only "add" when nothing existing is even tangentially related.

Existing notes:
%s

Candidate facts:
%s

Return a JSON array with one object per candidate in the same order:
[{"action":"add"|"update"|"skip","id":<existing_id or 0>,"content":"<text for add/update, empty for skip>"}]
Be deterministic. Do not invent facts. Merge wording when updating.`,
		strings.Join(existingLines, "\n"),
		strings.Join(candidates, "\n"))

	model := config.BackgroundModel(config.JobPersonalContext)
	_, genSpan := tracer.Start(ctx, "Gemini.GenerateContent.ReconcileNotes")
	var resp *genai.GenerateContentResponse
	err := g.allow(model)
	if err == nil {
		resp, err = g.client.Models.GenerateContent(ctx, model, genai.Text(prompt), &genai.GenerateContentConfig{
			ResponseMIMEType: "application/json",
		})
	}
	if err != nil {
		genSpan.RecordError(err)
		genSpan.End()
		return nil, fmt.Errorf("reconcile notes llm call: %w", err)
	}
	genSpan.End()

	if len(resp.Candidates) == 0 || len(resp.Candidates[0].Content.Parts) == 0 {
		return nil, fmt.Errorf("empty reconcile response from model")
	}

	var ops []NoteOp
	if err := json.Unmarshal([]byte(resp.Candidates[0].Content.Parts[0].Text), &ops); err != nil {
		return nil, fmt.Errorf("parse reconcile ops: %w", err)
	}
	return ops, nil
}

// attributionRules is the rule block of the attribution prompt. "summary" is the line that ends up in memory as the record of a slice of the day, so it is told to name the activity rather than the application it happened in; "state" is held to the same voice for the same reason.
const attributionRules = `Rules:
- Reuse a thread id when the activity continues that throughline; use 0 only for a new one.
- Emit MULTIPLE threads for concurrent activities. NEVER collapse entertainment into work or vice-versa.
- "state" is the SPECIFIC position within the thread, from the screen: the scene of a show, the section of an article, the feature being worked on. Name the thing, not the app; no counts or times.
- "summary" is what the user did here, in plain words: the activity itself, never the app or site it happened in.
- "subject" is what the user would call this to a friend: short, stable, plain-spoken; no title case, ampersands, app or file names, or report labels.
- "novel" is true only if this throughline appears genuinely new.
- "identity" holds ONLY durable facts about the PERSON (identity, lasting preferences, skills, relationships). Projects and shows are threads, NOT identity. Usually empty.`

// AttributeThreads maps recent screen activity onto ongoing threads, one update per concurrent throughline (so watching + coding never collapse into one thread) with the SPECIFIC state within each, plus any durable PERSON facts as identity.
func (g *GeminiSummarizer) AttributeThreads(ctx context.Context, activities []tracker.Activity, existing []Thread) (*ThreadAttribution, error) {
	tracer := obs.GetTracer(ctx, "ora.memory")
	ctx, span := tracer.Start(ctx, "GeminiSummarizer.AttributeThreads")
	defer span.End()

	ctx, cancel := context.WithTimeout(ctx, summarizerCallTimeout)
	defer cancel()

	var identity string
	if g.identity != nil {
		identity = g.identity(ctx)
	}
	prompt := AttributePrompt(activities, existing, identity)

	model := config.BackgroundModel(config.JobEpisodeSummary)
	_, genSpan := tracer.Start(ctx, "Gemini.GenerateContent.AttributeThreads")
	var resp *genai.GenerateContentResponse
	err := g.allow(model)
	if err == nil {
		resp, err = g.client.Models.GenerateContent(ctx, model, genai.Text(prompt), &genai.GenerateContentConfig{
			ResponseMIMEType: "application/json",
		})
	}
	if err != nil {
		genSpan.RecordError(err)
		genSpan.End()
		return nil, fmt.Errorf("attribute threads llm call: %w", err)
	}
	genSpan.End()

	if len(resp.Candidates) == 0 || len(resp.Candidates[0].Content.Parts) == 0 {
		return nil, fmt.Errorf("empty attribution response from model")
	}

	var attr ThreadAttribution
	if err := json.Unmarshal([]byte(resp.Candidates[0].Content.Parts[0].Text), &attr); err != nil {
		return nil, fmt.Errorf("parse thread attribution: %w", err)
	}
	return &attr, nil
}

const screenSightPrompt = `You are the eyes of an ambient OS companion. Extract what is on this screen as JSON so it can be searched later.
Be factual and concise. Present tense. Do not invent.

- user_activity: one short phrase for what the user is doing (editing a file, watching a scene, reading an article).
- visible_text: the meaningful visible strings as separate items — messages as "Name: text", terminal errors, headings, code symbols. Not chrome (tabs, buttons, cookies).
- summary: one sentence backup if the lists are thin.

{"user_activity":"","visible_text":[],"summary":""}`

// AnalyzeScreen sends a screenshot to the multimodal model and returns a structured moment (activity + visible chunks). Zero value on any failure so the caller can fall back to accessibility text.
func (g *GeminiSummarizer) AnalyzeScreen(ctx context.Context, png []byte) ScreenSight {
	if len(png) == 0 {
		return ScreenSight{}
	}

	tracer := obs.GetTracer(ctx, "ora.memory")
	ctx, span := tracer.Start(ctx, "GeminiSummarizer.AnalyzeScreen")
	defer span.End()

	ctx, cancel := context.WithTimeout(ctx, summarizerCallTimeout)
	defer cancel()

	parts := []*genai.Part{
		genai.NewPartFromText(screenSightPrompt),
		genai.NewPartFromBytes(png, "image/png"),
	}
	contents := []*genai.Content{genai.NewContentFromParts(parts, genai.RoleUser)}

	model := config.BackgroundModel(config.JobScreenSight)
	var resp *genai.GenerateContentResponse
	err := g.allow(model)
	if err == nil {
		resp, err = g.client.Models.GenerateContent(ctx, model, contents, &genai.GenerateContentConfig{
			ResponseMIMEType: "application/json",
		})
	}
	if err != nil {
		span.RecordError(err)
		return ScreenSight{}
	}
	if len(resp.Candidates) == 0 || len(resp.Candidates[0].Content.Parts) == 0 {
		return ScreenSight{}
	}
	return ParseScreenSight(resp.Candidates[0].Content.Parts[0].Text)
}

// DescribeScreen is AnalyzeScreen flattened to one string. Kept for callers that only need text.
func (g *GeminiSummarizer) DescribeScreen(ctx context.Context, png []byte) string {
	s := g.AnalyzeScreen(ctx, png)
	return ComposeMoment(s.UserActivity, s.VisibleText, s.Summary)
}

// AttributePrompt renders the attribution call's prompt. Input: the recent activities, the existing threads, and who the user is ("" when unknown). Output: the prompt text. Exported so the prompt shape is testable without a model.
func AttributePrompt(activities []tracker.Activity, existing []Thread, identity string) string {
	var activityList []string
	for _, a := range activities {
		entry := fmt.Sprintf("- %s: %s", a.App, a.Title)
		if a.ScreenText != "" {
			entry += fmt.Sprintf("\n  screen: %s", a.ScreenText)
		}
		activityList = append(activityList, entry)
	}

	var existingLines []string
	for _, t := range existing {
		existingLines = append(existingLines, fmt.Sprintf("id=%d [%s] %s :: %s", t.ID, t.Kind, t.Subject, t.State))
	}

	who := ""
	if identity != "" {
		who = fmt.Sprintf("\nWHO THE USER IS: %s\nEvery activity below is this person's own screen. When their name appears in a title, a calendar entry or a chat, it is them, never a third party: write \"attended the standup\", never \"met with <their name>\".\n", identity)
	}

	return fmt.Sprintf(`You are the memory compiler for an ambient OS companion. The user often does several things AT ONCE (e.g. watching a show while coding). Attribute the recent screen activity to ongoing "threads" — durable throughlines in the user's life — and say where they currently are within each.

You are given EXISTING THREADS (id, kind, subject :: current state) and RECENT ACTIVITIES (app, window title, and any screen text/description).
%s
%s

EXISTING THREADS:
%s

RECENT ACTIVITIES:
%s

Respond strictly as JSON:
{"threads":[{"id":0,"subject":"","kind":"work|project|entertainment|learning|routine|person","state":"","summary":"","novel":false}],"identity":[]}`,
		who,
		attributionRules,
		strings.Join(existingLines, "\n"),
		strings.Join(activityList, "\n"))
}
