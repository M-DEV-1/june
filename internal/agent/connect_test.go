package agent

import (
	"context"
	"errors"
	"ora/internal/db"
	"ora/internal/tracker"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"google.golang.org/genai"
)

// TestBuildTurnContent_SingleTurnWithContextAndText verifies buildTurnContent packs the turn context and user text into ONE Content/turn as two Parts, not two separate SendClientContent calls — the old two-call shape let the model reply to the bare context line, doubling Live API round-trips.
func TestBuildTurnContent_SingleTurnWithContextAndText(t *testing.T) {
	now := time.Date(2026, 7, 6, 14, 30, 0, 0, time.UTC)
	recalls := []string{"working on the ora recall tool"}

	turns := buildTurnContent(now, recalls, "what time is it")

	if len(turns) != 1 {
		t.Fatalf("expected exactly one Content/turn, got %d", len(turns))
	}
	parts := turns[0].Parts
	if len(parts) != 2 {
		t.Fatalf("expected exactly 2 parts (context + user text) in the single turn, got %d: %+v", len(parts), parts)
	}
	if !strings.Contains(parts[0].Text, "14:30") || !strings.Contains(parts[0].Text, "working on the ora recall tool") {
		t.Errorf("expected first part to carry the turnContext (time + recalls), got %q", parts[0].Text)
	}
	if parts[1].Text != "what time is it" {
		t.Errorf("expected second part to be the verbatim user text, got %q", parts[1].Text)
	}
}

// TestNowAnchor_EncodesCurrentMoment verifies nowAnchor renders the weekday, calendar date, wall-clock time, and timezone — the temporal anchor injected into the system prompt so the model isn't blind to "now" (it was previously seen confusing the date and deriving IST by hand).
func TestNowAnchor_EncodesCurrentMoment(t *testing.T) {
	ist := time.FixedZone("IST", int(5.5*3600))
	now := time.Date(2026, 7, 6, 12, 44, 0, 0, ist) // a Monday

	anchor := nowAnchor(now)

	for _, want := range []string{"Monday", "2026", "12:44", "IST"} {
		if !strings.Contains(anchor, want) {
			t.Errorf("nowAnchor(%v) = %q, missing %q", now, anchor, want)
		}
	}
}

// TestBuildHandshakeContext_BufferProviderSet_AddsWorkingLinesAndDrivesSearchMemory verifies a configured bufferProvider's activities land as "[working] app: title" context lines, and that their app+title names drive a SearchMemory focus lookup — this is the client-side wiring point F2 restores (compiler was always nil in the client process, so this whole path was dead in production; bufferProvider is what a daemon-IPC provider now feeds).
func TestBuildHandshakeContext_BufferProviderSet_AddsWorkingLinesAndDrivesSearchMemory(t *testing.T) {
	brain := &toolTestBrain{searchMemoryResult: []db.MemoryHit{{Source: "note", Content: "debugging the cuda kernel", RefID: 1}}}
	a := NewAgent(nil, nil, brain, nil, "")
	a.SetBufferProvider(func() []tracker.Activity {
		return []tracker.Activity{{App: "Code", Title: "main.go"}, {App: "Chrome", Title: "cuda docs"}}
	})

	out := a.buildHandshakeContext(context.Background(), []string{"existing"})

	if out[0] != "existing" {
		t.Errorf("expected the pre-existing contextParts to be preserved, got %+v", out)
	}
	foundWorking := false
	for _, line := range out {
		if strings.Contains(line, "[working] Code: main.go") {
			foundWorking = true
		}
	}
	if !foundWorking {
		t.Errorf("expected a [working] line for the buffer activity, got %+v", out)
	}
	if !strings.Contains(brain.searchMemoryCalledFocus, "Code") || !strings.Contains(brain.searchMemoryCalledFocus, "main.go") {
		t.Errorf("expected SearchMemory called with a focus string built from the buffer's app/title, got %q", brain.searchMemoryCalledFocus)
	}
	foundHit := false
	for _, line := range out {
		if strings.Contains(line, "debugging the cuda kernel") {
			foundHit = true
		}
	}
	if !foundHit {
		t.Errorf("expected the SearchMemory hit to be appended to contextParts, got %+v", out)
	}
}

// TestBuildHandshakeContext_NoBufferProvider_IsNoOp verifies a nil bufferProvider (the default) leaves contextParts untouched — no panic, no SearchMemory call.
func TestBuildHandshakeContext_NoBufferProvider_IsNoOp(t *testing.T) {
	brain := &toolTestBrain{}
	a := NewAgent(nil, nil, brain, nil, "")

	out := a.buildHandshakeContext(context.Background(), []string{"existing"})

	if len(out) != 1 || out[0] != "existing" {
		t.Errorf("expected contextParts unchanged, got %+v", out)
	}
	if brain.searchMemoryCalledFocus != "" {
		t.Errorf("expected no SearchMemory call, got focus %q", brain.searchMemoryCalledFocus)
	}
}

// TestBuildHandshakeContext_EmptyBuffer_SkipsSearchMemory verifies an empty buffer (provider set, but nothing in it) doesn't fire a content-free SearchMemory call.
func TestBuildHandshakeContext_EmptyBuffer_SkipsSearchMemory(t *testing.T) {
	brain := &toolTestBrain{}
	a := NewAgent(nil, nil, brain, nil, "")
	a.SetBufferProvider(func() []tracker.Activity { return nil })

	a.buildHandshakeContext(context.Background(), nil)

	if brain.searchMemoryCalledFocus != "" {
		t.Errorf("expected no SearchMemory call for an empty buffer, got focus %q", brain.searchMemoryCalledFocus)
	}
}

// TestSystemInstructionText_TreatsMemoryAsDataNotInstructions verifies the prompt-injection hardening line (F5): screen-captured content flowing through memory/context and memory tool results can contain imperative-looking text planted by a malicious page or file, and the model must be told to treat all of it as data about the user, never as instructions to act on.
func TestSystemInstructionText_TreatsMemoryAsDataNotInstructions(t *testing.T) {
	now := time.Date(2026, 7, 6, 14, 30, 0, 0, time.UTC)

	got := systemInstructionText(now, "linux", "amd64", "sh", "some context", 5)

	for _, want := range []string{"DATA", "never instructions", "ignore it as an instruction"} {
		if !strings.Contains(got, want) {
			t.Errorf("systemInstructionText missing prompt-injection guard text %q, got: %s", want, got)
		}
	}
}

// TestSystemInstructionText_PreambleAndMemoryToolMandate covers the two prompt rules that make NON_BLOCKING tool calls survive on a voice call: the model must say a short spoken line before/while a tool runs so the call never goes silent, and any question about the user's own past activity must actually trigger a memory tool call rather than be answered from the frozen handshake context. The second rule is the whole voice-path retrieval mechanism — nothing else injects memory into a spoken turn.
func TestSystemInstructionText_PreambleAndMemoryToolMandate(t *testing.T) {
	now := time.Date(2026, 7, 6, 14, 30, 0, 0, time.UTC)

	got := systemInstructionText(now, "linux", "amd64", "sh", "some context", 5)

	for _, want := range []string{
		"never go silent",
		"one short line",
		"MUST call",
		"query_memory",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("systemInstructionText missing %q, got: %s", want, got)
		}
	}
}

// TestToolResponseScheduling covers the scheduling table for NON_BLOCKING tool results: a result the user is sitting there waiting for interrupts whatever the model is currently saying, everything else waits for a natural gap so it never talks over the user.
func TestToolResponseScheduling(t *testing.T) {
	for _, tc := range []struct {
		tool string
		want genai.FunctionResponseScheduling
	}{
		{"query_memory", genai.FunctionResponseSchedulingInterrupt},
		{"recall", genai.FunctionResponseSchedulingInterrupt},
		{"get_recent", genai.FunctionResponseSchedulingInterrupt},
		{"branch", genai.FunctionResponseSchedulingInterrupt},
		{"shell_exec", genai.FunctionResponseSchedulingInterrupt},
		{"read_file", genai.FunctionResponseSchedulingInterrupt},
		{"save_note", genai.FunctionResponseSchedulingWhenIdle},
		{"update_note", genai.FunctionResponseSchedulingWhenIdle},
		{"delete_note", genai.FunctionResponseSchedulingWhenIdle},
		{"open_url", genai.FunctionResponseSchedulingWhenIdle},
		{"totally_unknown_tool", genai.FunctionResponseSchedulingWhenIdle},
	} {
		if got := toolResponseScheduling(tc.tool); got != tc.want {
			t.Errorf("toolResponseScheduling(%q) = %q, want %q", tc.tool, got, tc.want)
		}
	}
}

// TestRunToolCall_SendsScheduling proves the scheduling table is actually attached to the FunctionResponse that goes back over the wire, not just computed. Without it the Live API defaults every NON_BLOCKING result to WHEN_IDLE, so an answer the user asked for waits for a gap that may never come.
func TestRunToolCall_SendsScheduling(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	fs := &fakeLiveSession{
		msgCh:     make(chan *genai.LiveServerMessage, 2),
		responses: make(chan genai.LiveSendToolResponseParameters, 2),
		closeErr:  errors.New("fake session closed"),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.receiveLoop(ctx, fs, "test-model", make(chan error, 2))

	fs.msgCh <- &genai.LiveServerMessage{ToolCall: &genai.LiveServerToolCall{
		FunctionCalls: []*genai.FunctionCall{
			{ID: "call-1", Name: "query_memory", Args: map[string]any{"query": "riddler"}},
		},
	}}

	select {
	case resp := <-fs.responses:
		fr := resp.FunctionResponses[0]
		if fr.Scheduling != genai.FunctionResponseSchedulingInterrupt {
			t.Errorf("expected query_memory's response to carry INTERRUPT scheduling, got %q", fr.Scheduling)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the tool response")
	}
}

// TestProactivityConfig verifies the proactive-audio knob maps to the Live API config: on means the model may stay quiet when what it heard wasn't addressed to it, off means the field is omitted entirely so the API keeps its own default.
func TestProactivityConfig(t *testing.T) {
	if cfg := proactivityConfig(false); cfg != nil {
		t.Errorf("expected nil config when disabled, got %+v", cfg)
	}
	cfg := proactivityConfig(true)
	if cfg == nil || cfg.ProactiveAudio == nil || !*cfg.ProactiveAudio {
		t.Errorf("expected ProactiveAudio true when enabled, got %+v", cfg)
	}
}

// TestFormatFocusHits_TruncatesOverlongContent verifies the handshake's focus-lookup formatting excerpts content via db.FormatHit/FormatNoteHit like every other read path — this was the one site injecting SearchMemory hits raw and uncapped straight into the system instruction. Raw Activity Log summaries in production run tens of KB; an unformatted hit here can blow the system-prompt budget on a single row.
func TestFormatFocusHits_TruncatesOverlongContent(t *testing.T) {
	overlong := strings.Repeat("x", 500) // well past db's excerpt budget (200 runes)
	hits := []db.MemoryHit{{Source: "summary", Content: overlong}}

	got := formatFocusHits(hits, 2)

	if len(got) != 1 {
		t.Fatalf("expected exactly 1 formatted line, got %d: %+v", len(got), got)
	}
	if strings.Contains(got[0], overlong) {
		t.Fatalf("expected overlong content to be truncated, got full %d-char content: %q", len(overlong), got[0])
	}
}

// TestFormatFocusHits_NoteCarriesRefID verifies a note hit is formatted with its ref_id (matching query_memory's note formatting), not the generic "[note] ..." shape without an id.
func TestFormatFocusHits_NoteCarriesRefID(t *testing.T) {
	hits := []db.MemoryHit{{Source: "note", Content: "Samara is my wife", RefID: 105}}

	got := formatFocusHits(hits, 2)

	if len(got) != 1 || !strings.Contains(got[0], "note#105") {
		t.Fatalf(`expected a "note#105" line, got %+v`, got)
	}
}

// TestReceiveLoop_ModelTurn_ForwardsThoughtButNotFinalText verifies receiveLoop still forwards Thought:true parts (tagged via genai's own Part.Thought bit, not content-sniffed) but no longer forwards non-thought ModelTurn text — those are incomplete fragments under ResponseModalities=[Audio]; OutputTranscription (see TestReceiveLoop_OutputTranscription_StreamsAsOraText) is the sole source of ora's final text now.
func TestReceiveLoop_ModelTurn_ForwardsThoughtButNotFinalText(t *testing.T) {
	a := NewAgent(nil, nil, nil, nil, "")

	fs := &fakeLiveSession{
		msgCh:     make(chan *genai.LiveServerMessage, 2),
		responses: make(chan genai.LiveSendToolResponseParameters, 2),
		closeErr:  errors.New("fake session closed"),
	}
	errChan := make(chan error, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go a.receiveLoop(ctx, fs, "test-model", errChan)

	fs.msgCh <- &genai.LiveServerMessage{
		ServerContent: &genai.LiveServerContent{
			ModelTurn: &genai.Content{
				Parts: []*genai.Part{
					{Text: "**Planning the reply**", Thought: true},
					{Text: "Here's your answer.", Thought: false},
				},
			},
		},
	}

	var got ResponseChunk
	select {
	case got = <-a.TextResponseChan:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the thought chunk on TextResponseChan")
	}
	if !got.IsThought || got.Text != "**Planning the reply**" {
		t.Errorf("expected the thought part unmodified, got %+v", got)
	}

	select {
	case extra := <-a.TextResponseChan:
		t.Fatalf("expected no further chunk (final ModelTurn text must not be forwarded), got %+v", extra)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestReceiveLoop_OutputTranscription_StreamsAsOraText verifies the transcript of Ora's own spoken audio (ServerContent.OutputTranscription, enabled at handshake since the config landed) is forwarded to TextResponseChan as ordinary ora text. With ResponseModalities=[Audio], ModelTurn text parts are incomplete fragments — this transcription stream is the only complete text form of what Ora actually said, so it's what the TUI renders.
func TestReceiveLoop_OutputTranscription_StreamsAsOraText(t *testing.T) {
	a := NewAgent(nil, nil, nil, nil, "")
	fs := &fakeLiveSession{
		msgCh:     make(chan *genai.LiveServerMessage, 2),
		responses: make(chan genai.LiveSendToolResponseParameters, 2),
		closeErr:  errors.New("fake session closed"),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.receiveLoop(ctx, fs, "test-model", make(chan error, 2))

	fs.msgCh <- &genai.LiveServerMessage{ServerContent: &genai.LiveServerContent{
		OutputTranscription: &genai.Transcription{Text: "Hello there."},
	}}

	select {
	case chunk := <-a.TextResponseChan:
		if chunk.Text != "Hello there." {
			t.Errorf("expected the transcription text verbatim, got %q", chunk.Text)
		}
		if chunk.IsThought {
			t.Error("expected a transcription chunk to not be marked as a thought")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the output transcription chunk on TextResponseChan")
	}
}

// TestReceiveLoop_Interrupted_EmitsSystemStoppedChunk verifies a server-side barge-in (VAD interrupt) is surfaced to the UI as a plain SenderSystem chunk, not markdown-wrapped text — the UI's own "system" sender already has its own rendering style, so the asterisks were redundant.
func TestReceiveLoop_Interrupted_EmitsSystemStoppedChunk(t *testing.T) {
	a := NewAgent(nil, &fakeSpeaker{}, nil, nil, "")
	fs := &fakeLiveSession{
		msgCh:     make(chan *genai.LiveServerMessage, 1),
		responses: make(chan genai.LiveSendToolResponseParameters, 1),
		closeErr:  errors.New("fake session closed"),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.receiveLoop(ctx, fs, "test-model", make(chan error, 2))

	fs.msgCh <- &genai.LiveServerMessage{ServerContent: &genai.LiveServerContent{Interrupted: true}}

	select {
	case chunk := <-a.TextResponseChan:
		if chunk.Text != "[ora stopped]" || chunk.Sender != SenderSystem {
			t.Errorf("expected {Text: \"[ora stopped]\", Sender: %q}, got %+v", SenderSystem, chunk)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the interrupted chunk on TextResponseChan")
	}
}

// TestReceiveLoop_InputTranscriptionFinished_EmitsAsYouText verifies a finished voice utterance is forwarded to TextResponseChan as ResponseChunk{Sender: SenderYou} — the UI transcript's only source of what the user actually said in voice mode.
func TestReceiveLoop_InputTranscriptionFinished_EmitsAsYouText(t *testing.T) {
	a := NewAgent(nil, nil, nil, nil, "")
	fs := &fakeLiveSession{
		msgCh:     make(chan *genai.LiveServerMessage, 1),
		responses: make(chan genai.LiveSendToolResponseParameters, 1),
		closeErr:  errors.New("fake session closed"),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.receiveLoop(ctx, fs, "test-model", make(chan error, 2))

	fs.msgCh <- &genai.LiveServerMessage{ServerContent: &genai.LiveServerContent{
		InputTranscription: &genai.Transcription{Text: "what show am I watching", Finished: true},
	}}

	select {
	case chunk := <-a.TextResponseChan:
		if chunk.Text != "what show am I watching" || chunk.Sender != SenderYou {
			t.Errorf("expected {Text: %q, Sender: %q}, got %+v", "what show am I watching", SenderYou, chunk)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the finished utterance on TextResponseChan")
	}
}

// TestReceiveLoop_InputFragmentsNeverFinished_FlushedAsYouBeforeOutputTranscription is WP10 Part A: current Live API model versions never set InputTranscription.Finished (documented: googleapis/js-genai#1429 — only text fragments arrive, the finished flag never updates), so waiting on it exclusively left the accumulated utterance stuck in the buffer forever — the user's own speech never rendered ("no way to know if Ora heard me"). The fix flushes the pending buffer as a SenderYou chunk on the first sign the model is responding; here that's the first OutputTranscription fragment. The "you" chunk must arrive before the ora chunk that triggered the flush.
func TestReceiveLoop_InputFragmentsNeverFinished_FlushedAsYouBeforeOutputTranscription(t *testing.T) {
	a := NewAgent(nil, nil, nil, nil, "")
	fs := &fakeLiveSession{
		msgCh:     make(chan *genai.LiveServerMessage, 2),
		responses: make(chan genai.LiveSendToolResponseParameters, 1),
		closeErr:  errors.New("fake session closed"),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.receiveLoop(ctx, fs, "test-model", make(chan error, 2))

	fs.msgCh <- &genai.LiveServerMessage{ServerContent: &genai.LiveServerContent{
		InputTranscription: &genai.Transcription{Text: "what show am ", Finished: false},
	}}
	fs.msgCh <- &genai.LiveServerMessage{ServerContent: &genai.LiveServerContent{
		InputTranscription: &genai.Transcription{Text: "I watching", Finished: false},
	}}
	fs.msgCh <- &genai.LiveServerMessage{ServerContent: &genai.LiveServerContent{
		OutputTranscription: &genai.Transcription{Text: "You're watching Suits."},
	}}

	var got []ResponseChunk
	for i := 0; i < 2; i++ {
		select {
		case chunk := <-a.TextResponseChan:
			got = append(got, chunk)
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for chunk %d, got so far: %+v", i, got)
		}
	}

	if len(got) != 2 || got[0].Sender != SenderYou || got[0].Text != "what show am I watching" {
		t.Fatalf("expected the first chunk to be the flushed you-utterance, got %+v", got)
	}
	if got[1].Sender != "" || got[1].Text != "You're watching Suits." {
		t.Errorf("expected the second chunk to be the ora reply, got %+v", got[1])
	}
}

// TestReceiveLoop_InputFragmentsNeverFinished_FlushedAsYouBeforeToolCall is the ToolCall-triggered variant of the flush above — a tool call is just as much "the model responding" as an output transcription is.
func TestReceiveLoop_InputFragmentsNeverFinished_FlushedAsYouBeforeToolCall(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	fs := &fakeLiveSession{
		msgCh:     make(chan *genai.LiveServerMessage, 2),
		responses: make(chan genai.LiveSendToolResponseParameters, 1),
		closeErr:  errors.New("fake session closed"),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.receiveLoop(ctx, fs, "test-model", make(chan error, 2))

	fs.msgCh <- &genai.LiveServerMessage{ServerContent: &genai.LiveServerContent{
		InputTranscription: &genai.Transcription{Text: "what am I watching", Finished: false},
	}}
	fs.msgCh <- &genai.LiveServerMessage{ToolCall: &genai.LiveServerToolCall{
		FunctionCalls: []*genai.FunctionCall{{ID: "call-1", Name: "list_files", Args: map[string]any{"path": "."}}},
	}}

	select {
	case chunk := <-a.TextResponseChan:
		if chunk.Sender != SenderYou || chunk.Text != "what am I watching" {
			t.Errorf("expected the flushed you-utterance ahead of the tool call, got %+v", chunk)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the flushed you chunk")
	}
}

// TestReceiveLoop_NoInputFragments_OutputTranscription_NoEmptyYouChunk verifies a typed (non-voice) turn — no InputTranscription fragments ever accumulated — doesn't emit a spurious empty "you" chunk when the model responds; only the ora chunk should appear.
func TestReceiveLoop_NoInputFragments_OutputTranscription_NoEmptyYouChunk(t *testing.T) {
	a := NewAgent(nil, nil, nil, nil, "")
	fs := &fakeLiveSession{
		msgCh:     make(chan *genai.LiveServerMessage, 1),
		responses: make(chan genai.LiveSendToolResponseParameters, 1),
		closeErr:  errors.New("fake session closed"),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.receiveLoop(ctx, fs, "test-model", make(chan error, 2))

	fs.msgCh <- &genai.LiveServerMessage{ServerContent: &genai.LiveServerContent{
		OutputTranscription: &genai.Transcription{Text: "the answer is 4"},
	}}

	select {
	case chunk := <-a.TextResponseChan:
		if chunk.Sender == SenderYou {
			t.Fatalf("expected no you-chunk for a typed turn with no input fragments, got %+v", chunk)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the ora chunk")
	}

	select {
	case extra := <-a.TextResponseChan:
		t.Fatalf("expected only one chunk, got an extra: %+v", extra)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestReceiveLoop_InputTranscriptionFinished_DoesNotAutoInject is a regression guard: receiveLoop must NOT call RetrieveRelevant or send anything back into the session when a transcription finishes. A real live session on 2026-08-09 showed exactly this — an unsolicited SendClientContent call fired automatically on every finished transcription — caused the model to loop on "is the user still there?" reasoning for 3+ minutes with zero final replies; an unsolicited client turn mid-session breaks native-audio turn-taking. The auto-injection code (injectVoiceRecalls) was deleted in WP6 as dead — parked and never called from anywhere since that regression — but the risk it was parked for is unchanged, so this guard stays. If this starts failing because someone re-wires a call site to send context back into an active session on transcription-finished, the safety of doing so against a real Live session needs to be re-verified first, not assumed.
func TestReceiveLoop_InputTranscriptionFinished_DoesNotAutoInject(t *testing.T) {
	brain := &toolTestBrain{retrieveRelevantCalled: make(chan string, 1)}
	a := NewAgent(nil, nil, brain, nil, "")

	fs := &fakeLiveSession{
		msgCh:       make(chan *genai.LiveServerMessage, 1),
		responses:   make(chan genai.LiveSendToolResponseParameters, 1),
		sentContent: make(chan genai.LiveSendClientContentParameters, 1),
		closeErr:    errors.New("fake session closed"),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.receiveLoop(ctx, fs, "test-model", make(chan error, 2))

	fs.msgCh <- &genai.LiveServerMessage{ServerContent: &genai.LiveServerContent{
		InputTranscription: &genai.Transcription{Text: "what show am I watching", Finished: true},
	}}

	select {
	case focus := <-brain.retrieveRelevantCalled:
		t.Fatalf("expected receiveLoop not to call RetrieveRelevant on its own, got focus %q", focus)
	case sent := <-fs.sentContent:
		t.Fatalf("expected receiveLoop not to send anything on its own, got %+v", sent)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestThinkingConfig_BoundsThinkingBudget verifies the Live config sets an explicit, bounded ThinkingBudget instead of leaving it unset. Confirmed via a real session on 2026-08-09: with ThinkingConfig unset, gemini-2.5-flash-native-audio-preview-12-2025 ran multi-minute silent "thought" chains (12 thought parts, zero final replies logged across a 5-minute session) — genuinely relevant reasoning, but never surfaced as an actual spoken answer. ThinkingBudget=0 (Pipecat's low-latency voice preset) was considered and rejected: the observed thinking content was real synthesis the user explicitly wants (e.g. connecting a paper's argument to their own work), so the fix is a bound, not a kill switch.
func TestThinkingConfig_BoundsThinkingBudget(t *testing.T) {
	cfg := thinkingConfig()

	if cfg == nil || cfg.ThinkingBudget == nil {
		t.Fatalf("expected an explicit ThinkingBudget to be set, got %+v", cfg)
	}
	if got := *cfg.ThinkingBudget; got <= 0 {
		t.Errorf("expected a positive bounded budget (not 0 — thinking should still happen, just not run away), got %d", got)
	}
	if !cfg.IncludeThoughts {
		t.Error("expected IncludeThoughts=true — receiveLoop already routes Thought:true parts correctly (see ResponseChunk) and they're valuable for debugging turn-taking issues")
	}
}

// TestCompressionConfig_TriggerAboveTarget verifies the context-window-compression config's trigger point is strictly above its shrink target — a session must accumulate real headroom before every compression, not just barely exceed the target and re-trigger immediately.
func TestCompressionConfig_TriggerAboveTarget(t *testing.T) {
	cfg := compressionConfig()

	if cfg == nil || cfg.TriggerTokens == nil || cfg.SlidingWindow == nil || cfg.SlidingWindow.TargetTokens == nil {
		t.Fatalf("expected TriggerTokens and SlidingWindow.TargetTokens both set, got %+v", cfg)
	}
	trigger, target := *cfg.TriggerTokens, *cfg.SlidingWindow.TargetTokens
	if trigger != 64000 {
		t.Errorf("expected TriggerTokens=64000, got %d", trigger)
	}
	if target != 32000 {
		t.Errorf("expected TargetTokens=32000, got %d", target)
	}
	if trigger <= target {
		t.Errorf("expected trigger (%d) strictly above target (%d)", trigger, target)
	}
}

// fakeSpeaker is a no-op audio.Speaker for receiveLoop tests that exercise the barge-in (Interrupted) path, which calls Flush() on the real speaker — a nil speaker panics there.
type fakeSpeaker struct{}

func (s *fakeSpeaker) Play(pcm []byte) error     { return nil }
func (s *fakeSpeaker) Flush()                    {}
func (s *fakeSpeaker) Close() error              { return nil }
func (s *fakeSpeaker) CurrentAmplitude() float64 { return 0 }

// budgetBrain records the remaining ctx budget RetrieveRelevant is handed, and returns recalls only when that budget covers minBudget. It stands in for the real RetrieveRelevant, whose Gemini embedContent round trip measures 2.3-2.8s against the live API — anything less than that and the semantic half of hybrid search never returns in time.
type budgetBrain struct {
	*toolTestBrain
	minBudget time.Duration
	budget    chan time.Duration
}

func (b *budgetBrain) RetrieveRelevant(ctx context.Context, focus string, maxItems int) ([]string, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		b.budget <- 0
		return nil, errors.New("retrieve got a context with no deadline")
	}
	budget := time.Until(deadline)
	b.budget <- budget
	if budget < b.minBudget {
		return nil, context.DeadlineExceeded
	}
	return []string{"kernel work on the retrieval path"}, nil
}

// TestTextSendLoop_RetrieveBudgetCoversEmbedRoundTrip verifies the deadline textSendLoop puts on RetrieveRelevant leaves room for a real embed round trip, so the recalls it fetches actually reach the turn. With a budget under the measured embed latency the semantic half of hybrid retrieval is cancelled on every typed turn and search silently degrades to lexical-only.
func TestTextSendLoop_RetrieveBudgetCoversEmbedRoundTrip(t *testing.T) {
	const measuredEmbedLatency = 2800 * time.Millisecond

	brain := &budgetBrain{toolTestBrain: &toolTestBrain{}, minBudget: measuredEmbedLatency, budget: make(chan time.Duration, 1)}
	a := NewAgent(nil, &fakeSpeaker{}, brain, nil, "")
	fs := &fakeLiveSession{sentContent: make(chan genai.LiveSendClientContentParameters, 1)}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.textSendLoop(ctx, fs)

	a.TextChan <- "what was I working on"

	select {
	case budget := <-brain.budget:
		if budget < measuredEmbedLatency {
			t.Errorf("RetrieveRelevant got %v of budget, too little for a %v embed round trip", budget, measuredEmbedLatency)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for RetrieveRelevant to be called")
	}

	select {
	case sent := <-fs.sentContent:
		if len(sent.Turns) != 1 || len(sent.Turns[0].Parts) != 2 {
			t.Fatalf("expected one turn with two parts, got %+v", sent.Turns)
		}
		if !strings.Contains(sent.Turns[0].Parts[0].Text, "kernel work on the retrieval path") {
			t.Errorf("expected the recall to reach the turn context, got %q", sent.Turns[0].Parts[0].Text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for SendClientContent")
	}
}

// TestTextSendLoop_HungRetrieveRelevant_DoesNotDelaySend verifies a slow/hung RetrieveRelevant call (e.g. a client-side embed API call over IPC that never returns) doesn't hold up sending the user's typed turn — the per-call timeout must let the send proceed promptly with whatever recalls (if any) came back in time, same degrade-gracefully shape HybridSearch's own resilience already has.
func TestTextSendLoop_HungRetrieveRelevant_DoesNotDelaySend(t *testing.T) {
	brain := &toolTestBrain{retrieveRelevantBlocksOnCtx: true}
	a := NewAgent(nil, &fakeSpeaker{}, brain, nil, "")
	fs := &fakeLiveSession{sentContent: make(chan genai.LiveSendClientContentParameters, 1)}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.textSendLoop(ctx, fs)

	start := time.Now()
	a.TextChan <- "what's the weather"

	select {
	case sent := <-fs.sentContent:
		if elapsed := time.Since(start); elapsed > textSendLoopRetrieveTimeout+500*time.Millisecond {
			t.Errorf("expected the send to proceed once the retrieve budget expired, took %v", elapsed)
		}
		if len(sent.Turns) != 1 || len(sent.Turns[0].Parts) != 2 || sent.Turns[0].Parts[1].Text != "what's the weather" {
			t.Errorf("expected the turn to still carry the user's text, got %+v", sent.Turns)
		}
	case <-time.After(2 * textSendLoopRetrieveTimeout):
		t.Fatal("timed out waiting for SendClientContent — the turn never got sent")
	}
}

// fakeLiveSession is a minimal liveSession for testing receiveLoop's concurrency behavior without a live websocket.
// msgCh feeds messages in order; Receive blocks until one is available and returns closeErr once msgCh is closed (mirrors session.Receive() erroring after the connection drops).
// SendToolResponse records what would have been sent back to the model.
type fakeLiveSession struct {
	msgCh     chan *genai.LiveServerMessage
	responses chan genai.LiveSendToolResponseParameters
	closeErr  error

	// sentContent, if non-nil, receives every SendClientContent call's params — buffered by the test as needed. nil is a valid zero value: SendClientContent becomes a no-op recorder that just returns nil, for tests that don't care about it.
	sentContent chan genai.LiveSendClientContentParameters
}

func (f *fakeLiveSession) SendClientContent(p genai.LiveSendClientContentParameters) error {
	if f.sentContent != nil {
		f.sentContent <- p
	}
	return nil
}

func (f *fakeLiveSession) Receive() (*genai.LiveServerMessage, error) {
	msg, ok := <-f.msgCh
	if !ok {
		return nil, f.closeErr
	}
	return msg, nil
}

func (f *fakeLiveSession) SendToolResponse(p genai.LiveSendToolResponseParameters) error {
	f.responses <- p
	return nil
}

// TestReceiveLoop_ToolCallDoesNotBlockReceivePath proves a slow/blocking tool call (e.g. HITL shell_exec awaiting approval) doesn't stall session.Receive() and drop mic frames — executeTool used to run synchronously inline in receiveLoop, blocking the whole receive path for as long as the tool took.
//
// Drives a real blocking shell_exec (not in the allowlist, so it genuinely blocks on ToolApprovalChan/ResultChan) followed immediately by a fast tool call, and asserts the fast call's result comes back first. Under the old synchronous code this would hang until the 2s timeout instead.
func TestReceiveLoop_ToolCallDoesNotBlockReceivePath(t *testing.T) {
	a := NewAgent(nil, nil, nil, nil, "")

	fs := &fakeLiveSession{
		msgCh:     make(chan *genai.LiveServerMessage, 4),
		responses: make(chan genai.LiveSendToolResponseParameters, 4),
		closeErr:  errors.New("fake session closed"),
	}

	errChan := make(chan error, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		a.receiveLoop(ctx, fs, "test-model", errChan)
		close(done)
	}()

	// 1. Slow call: shell_exec on an unapproved command blocks in executeTool
	// on <-resChan until the test sends an approval result below.
	fs.msgCh <- &genai.LiveServerMessage{
		ToolCall: &genai.LiveServerToolCall{
			FunctionCalls: []*genai.FunctionCall{
				{ID: "call-slow", Name: "shell_exec", Args: map[string]any{"command": "echo never-approved"}},
			},
		},
	}

	// 2. Fast call, sent right after with no delay. Must complete without
	// waiting on the slow call above.
	fs.msgCh <- &genai.LiveServerMessage{
		ToolCall: &genai.LiveServerToolCall{
			FunctionCalls: []*genai.FunctionCall{
				{ID: "call-fast", Name: "list_files", Args: map[string]any{"path": "."}},
			},
		},
	}

	select {
	case resp := <-fs.responses:
		got := resp.FunctionResponses[0].ID
		if got != "call-fast" {
			t.Fatalf("expected the fast call's response to arrive first, got ID %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the fast tool call's response - receiveLoop appears blocked by the still-pending slow tool call")
	}

	// Now resolve the slow call's HITL approval.
	var req ToolRequest
	select {
	case req = <-a.ToolApprovalChan:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the slow tool call's HITL approval request")
	}
	if req.AllowKey != "echo never-approved" {
		t.Fatalf("unexpected approval request AllowKey %q", req.AllowKey)
	}
	if req.Description != "shell: echo never-approved" {
		t.Fatalf("unexpected approval request Description %q", req.Description)
	}
	req.ResultChan <- "ok: approved and ran"

	select {
	case resp := <-fs.responses:
		fr := resp.FunctionResponses[0]
		if fr.ID != "call-slow" || fr.Name != "shell_exec" {
			t.Fatalf("expected slow call's response (ID=call-slow, Name=shell_exec), got ID=%q Name=%q", fr.ID, fr.Name)
		}
		if fr.Response["output"] != "ok: approved and ran" {
			t.Fatalf("expected slow call's approved result to be delivered, got %v", fr.Response["output"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the slow tool call's response after approval")
	}

	close(fs.msgCh)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("receiveLoop did not return after the session closed")
	}
}

// TestRunToolCall_SessionEndsBeforeApproval_GoroutineExitsInsteadOfLeaking covers a HITL-blocked runToolCall goroutine that used to leak forever if the session disconnected before the user approved/rejected — fixed by threading ctx through the `<-resChan` wait so it can also select on ctx.Done() (Connect()'s sessCancel() is what fires it in production).
//
// Drives a real blocking HITL call, confirms it's genuinely parked awaiting approval, then cancels the session context WITHOUT resolving the approval — the goroutine must exit on its own and must not try to deliver a response into a session that's gone.
func TestRunToolCall_SessionEndsBeforeApproval_GoroutineExitsInsteadOfLeaking(t *testing.T) {
	a := NewAgent(nil, nil, nil, nil, "")

	fs := &fakeLiveSession{
		msgCh:     make(chan *genai.LiveServerMessage, 2),
		responses: make(chan genai.LiveSendToolResponseParameters, 2),
		closeErr:  errors.New("fake session closed"),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go a.receiveLoop(ctx, fs, "test-model", make(chan error, 2))

	runtime.GC()
	baseline := runtime.NumGoroutine()

	fs.msgCh <- &genai.LiveServerMessage{
		ToolCall: &genai.LiveServerToolCall{
			FunctionCalls: []*genai.FunctionCall{
				{ID: "call-abandoned", Name: "shell_exec", Args: map[string]any{"command": "echo never-approved-2"}},
			},
		},
	}

	// Confirm it's genuinely blocked awaiting HITL approval (not already
	// finished) before cutting the session out from under it.
	select {
	case <-a.ToolApprovalChan:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the abandoned call's HITL approval request")
	}

	// Simulate the session dying before the user ever responds — never send
	// anything on ResultChan.
	cancel()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if runtime.NumGoroutine() <= baseline {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutine count stayed above pre-call baseline %d (currently %d) 2s after session cancellation — the HITL goroutine leaked", baseline, runtime.NumGoroutine())
		}
		time.Sleep(10 * time.Millisecond)
	}

	select {
	case resp := <-fs.responses:
		t.Fatalf("expected no response delivered for a session that ended before approval, got: %+v", resp)
	default:
	}
}

// TestTruncateUTF8_SplitsExactlyOnMultiByteRuneBoundary verifies truncating mid multi-byte UTF-8 char (e.g. the 3-byte U+FFFC object-replacement char a11y capture is full of) backs off to the last complete rune instead of returning a broken half-character — the exact shape of data a real tool result contains.
func TestTruncateUTF8_SplitsExactlyOnMultiByteRuneBoundary(t *testing.T) {
	// "ab" (2 bytes) + U+FFFC (3 bytes) == 5 bytes total. Cutting at byte 4 lands one byte into the 3-byte rune (bytes 2,3,4 of the string).
	s := "ab￼"

	got := truncateUTF8(s, 4)

	if got != "ab" {
		t.Errorf("truncateUTF8(%q, 4) = %q, want %q (the split rune dropped entirely)", s, got, "ab")
	}
	if !utf8.ValidString(got) {
		t.Errorf("truncateUTF8(%q, 4) = %q is not valid UTF-8", s, got)
	}
}

// TestReceiveLoop_TurnComplete_EmitsTurnBoundaryChunk verifies receiveLoop emits a boundary marker on TextResponseChan when the server marks a turn complete — the UI uses this to stop merging the NEXT turn's ora chunks into whatever block the current turn left behind (see streamLine's TurnBoundary handling), which is what let a restart/garbage turn glue onto a good prior reply.
func TestReceiveLoop_TurnComplete_EmitsTurnBoundaryChunk(t *testing.T) {
	a := NewAgent(nil, nil, nil, nil, "")
	fs := &fakeLiveSession{
		msgCh:     make(chan *genai.LiveServerMessage, 1),
		responses: make(chan genai.LiveSendToolResponseParameters, 1),
		closeErr:  errors.New("fake session closed"),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.receiveLoop(ctx, fs, "test-model", make(chan error, 2))

	fs.msgCh <- &genai.LiveServerMessage{ServerContent: &genai.LiveServerContent{TurnComplete: true}}

	select {
	case chunk := <-a.TextResponseChan:
		if !chunk.TurnBoundary {
			t.Errorf("expected a TurnBoundary chunk, got %+v", chunk)
		}
		if chunk.Text != "" {
			t.Errorf("expected an empty-text boundary marker, got %+v", chunk)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the turn boundary chunk on TextResponseChan")
	}
}
