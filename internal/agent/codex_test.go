package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// writeCodexAuth writes a Codex CLI style auth.json with the given tokens into a temp dir and returns its path.
func writeCodexAuth(t *testing.T, tokens map[string]any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth.json")
	body, _ := json.Marshal(map[string]any{"auth_mode": "chatgpt", "OPENAI_API_KEY": nil, "tokens": tokens, "last_refresh": "2026-09-04T11:38:42.990082443Z"})
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// sse joins JSON events into the text/event-stream body the Codex backend sends.
func sse(events ...string) string {
	var b strings.Builder
	for _, ev := range events {
		b.WriteString("data: " + ev + "\n\n")
	}
	return b.String()
}

// The auth file Codex CLI keeps is the only credential source: auth_mode, the four tokens and last_refresh, with account_id used as is when present.
func TestLoadCodexAuth_ReadsTheCodexCLIFile(t *testing.T) {
	path := writeCodexAuth(t, map[string]any{"id_token": "id.x.y", "access_token": "acc.x.y", "refresh_token": "ref", "account_id": "acct_1"})
	auth, err := loadCodexAuth(path)
	if err != nil {
		t.Fatal(err)
	}
	if auth.AuthMode != "chatgpt" || auth.Tokens.AccessToken != "acc.x.y" || auth.Tokens.RefreshToken != "ref" || auth.Tokens.IDToken != "id.x.y" {
		t.Errorf("loaded %+v", auth)
	}
	if got := auth.Tokens.accountID(); got != "acct_1" {
		t.Errorf("accountID = %q, want acct_1", got)
	}
	if _, err := loadCodexAuth(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("a missing file must be an error")
	}
	if _, err := loadCodexAuth(writeCodexAuth(t, map[string]any{"id_token": "only"})); err == nil {
		t.Error("a file without an access token must be an error")
	}
}

// A streamed round is read from the SSE events: every finished output item is kept for echoing back, the answer text comes from the message item, and the model and usage come from response.completed.
// TestParseCodexStream drives parseCodexStream over one SSE stream per row and checks whatever that stream's shape is meant to prove: the happy path collects every item, joins the text deltas, and reads the model and usage off response.completed; a stream with no message item falls back to the deltas alone; a function_call item becomes a call that also stays in Items so the next round can echo it back; a payload split across several data lines is joined rather than dropped; and a cut-short turn, a failed response, a top-level error event and a stream that never reaches response.completed are all reported as the round's error rather than a false answer.
func TestParseCodexStream(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		check func(t *testing.T, round codexRound, err error)
	}{
		{
			name: "collects items, joins text deltas and reads model and usage",
			body: sse(
				`{"type":"response.created","response":{"id":"resp_1"}}`,
				`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","encrypted_content":"opaque","summary":[]}}`,
				`{"type":"response.output_text.delta","delta":"Hello"}`,
				`{"type":"response.output_text.delta","delta":" there"}`,
				`{"type":"response.output_item.done","output_index":1,"item":{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hello there","annotations":[]}]}}`,
				`{"type":"response.completed","response":{"id":"resp_1","model":"gpt-5.5-2026-06-01","status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`,
			),
			check: func(t *testing.T, round codexRound, err error) {
				if err != nil {
					t.Fatal(err)
				}
				if round.Text != "Hello there" {
					t.Errorf("Text = %q", round.Text)
				}
				if len(round.Items) != 2 || !strings.Contains(string(round.Items[0]), `"reasoning"`) {
					t.Errorf("Items = %s", round.Items)
				}
				if len(round.Calls) != 0 {
					t.Errorf("Calls = %+v", round.Calls)
				}
				if round.Model != "gpt-5.5-2026-06-01" || round.Usage.InputTokens != 10 || round.Usage.OutputTokens != 5 || round.Usage.TotalTokens != 15 {
					t.Errorf("Model = %q Usage = %+v", round.Model, round.Usage)
				}
			},
		},
		{
			name: "no message item: the text deltas are the answer",
			body: sse(`{"type":"response.output_text.delta","delta":"a"}`, `{"type":"response.output_text.delta","delta":"b"}`, `{"type":"response.completed","response":{"model":"gpt-5.5","usage":{}}}`),
			check: func(t *testing.T, round codexRound, err error) {
				if err != nil {
					t.Fatal(err)
				}
				if round.Text != "ab" {
					t.Errorf("Text = %q", round.Text)
				}
			},
		},
		{
			name: "a function_call item becomes a call and stays in Items",
			body: sse(
				`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"observe_screen","arguments":"{}","status":"completed"}}`,
				`{"type":"response.completed","response":{"model":"gpt-5.5","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
			),
			check: func(t *testing.T, round codexRound, err error) {
				if err != nil {
					t.Fatal(err)
				}
				if len(round.Calls) != 1 || round.Calls[0] != (codexCall{ID: "fc_1", CallID: "call_1", Name: "observe_screen", Arguments: "{}"}) {
					t.Errorf("Calls = %+v", round.Calls)
				}
				if len(round.Items) != 1 {
					t.Errorf("Items = %s", round.Items)
				}
			},
		},
		{
			name: "a payload split across several data lines is joined, not dropped",
			body: "data: {\"type\":\"response.output_item.done\",\n" +
				"data:  \"item\":{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"joined\"}]}}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-5.5\",\"usage\":{}}}\n\n",
			check: func(t *testing.T, round codexRound, err error) {
				if err != nil {
					t.Fatal(err)
				}
				if round.Text != "joined" {
					t.Errorf("Text = %q", round.Text)
				}
			},
		},
		{
			name: "a turn cut short by the token limit is a failure naming why",
			body: sse(
				`{"type":"response.output_text.delta","delta":"half a sen"}`,
				`{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`),
			check: func(t *testing.T, round codexRound, err error) {
				if err == nil || !strings.Contains(err.Error(), "max_output_tokens") {
					t.Errorf("err = %v", err)
				}
			},
		},
		{
			name: "a failed response surfaces its own error message",
			body: sse(`{"type":"response.failed","response":{"status":"failed","error":{"code":"server_error","message":"boom"}}}`),
			check: func(t *testing.T, round codexRound, err error) {
				if err == nil || !strings.Contains(err.Error(), "boom") {
					t.Errorf("response.failed: %v", err)
				}
			},
		},
		{
			name: "a top-level error event surfaces its message",
			body: sse(`{"type":"error","code":"rate_limit_exceeded","message":"slow down"}`),
			check: func(t *testing.T, round codexRound, err error) {
				if err == nil || !strings.Contains(err.Error(), "slow down") {
					t.Errorf("error event: %v", err)
				}
			},
		},
		{
			name: "a stream cut off before response.completed is an error",
			body: sse(`{"type":"response.output_text.delta","delta":"half"}`),
			check: func(t *testing.T, round codexRound, err error) {
				if err == nil {
					t.Error("a stream cut off before response.completed must be an error")
				}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			round, err := parseCodexStream(strings.NewReader(c.body), nil)
			c.check(t, round, err)
		})
	}
}

// A shape whose transport draw call fails while the stream is still arriving must not shift which of the remaining shapes counts as already drawn (finding 1 of the 2026-09 draw-batch review): onShape has to record the failure rather than drop it, so the prefix streamDrawnFrom hands back still lines up with the shapes the finished call lists. Two shapes stream in, the first's a.Draw call fails and the second's succeeds; the finished call must then draw nothing more, leaving a.Draw called exactly twice, once per shape, each with its own rectangle.
func TestAskCodex_StreamedDrawFailureDoesNotDoubleDrawTheNextShape(t *testing.T) {
	a, drawn := drawingAgent(t)
	calls := 0
	a.Draw = func(_, shape string, points [][2]int, x, y, w, h int, label string) error {
		calls++
		*drawn = append(*drawn, fmt.Sprintf("%d,%d,%d,%d", x, y, w, h))
		if calls == 1 {
			return errors.New("transport failed")
		}
		return nil
	}
	a.executeTool(context.Background(), "observe_screen", map[string]any{})

	ctx := context.Background()
	early := map[string][]streamDrawn{}
	// Mirrors the onShape closure askCodex builds around one round (codex.go), fixed per finding 1: a shape that fails to draw is recorded as an error entry rather than dropped.
	onShape := func(itemID string, shape map[string]any) {
		phrase, target, errText := a.drawOne(ctx, "g", shape)
		if errText != "" {
			early[itemID] = append(early[itemID], streamDrawn{Err: errText})
			return
		}
		early[itemID] = append(early[itemID], streamDrawn{Phrase: phrase, Target: target})
	}

	argsJSON := `{"shapes":[{"shape":"box","on":1},{"shape":"box","on":2}]}`
	split := strings.Index(argsJSON, `{"shape":"box","on":2}`)
	first, _ := json.Marshal(argsJSON[:split])
	second, _ := json.Marshal(argsJSON[split:])
	doneItem, _ := json.Marshal(map[string]any{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "draw", "arguments": argsJSON})
	body := sse(
		`{"type":"response.output_item.added","item":{"type":"function_call","id":"fc_1","name":"draw"}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":`+string(first)+`}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":`+string(second)+`}`,
		`{"type":"response.output_item.done","item":`+string(doneItem)+`}`,
		`{"type":"response.completed","response":{"model":"gpt-5.5","usage":{}}}`,
	)
	round, err := parseCodexStream(strings.NewReader(body), onShape)
	if err != nil {
		t.Fatal(err)
	}
	if len(round.Calls) != 1 {
		t.Fatalf("Calls = %+v", round.Calls)
	}
	call := round.Calls[0]
	var callArgs map[string]any
	json.Unmarshal([]byte(call.Arguments), &callArgs)
	a.executeTool(withStreamDrawn(ctx, "g", early[call.ID]), call.Name, callArgs)

	if calls != 2 {
		t.Fatalf("a.Draw called %d times, want exactly 2 (no double draw)", calls)
	}
	if len(*drawn) != 2 || (*drawn)[0] == (*drawn)[1] {
		t.Errorf("drew %v, want two different rectangles", *drawn)
	}
}

// codexTestClient returns a client pointed at the given fake backend and token endpoint, reading the given auth file.
func codexTestClient(responsesURL, tokenURL, authPath string) *codexClient {
	return &codexClient{ResponsesURL: responsesURL, TokenURL: tokenURL, AuthPath: authPath, Model: "gpt-5.5", HTTP: http.DefaultClient}
}

// A 401 is answered by one refresh through the OAuth token endpoint with the CLI's client id and the refresh token, then the round is retried with the new access token; a second 401 is an error, never a loop.
func TestCodexClient_RefreshesOnceAfter401(t *testing.T) {
	var calls atomic.Int32
	var refreshBody map[string]string
	var seen []http.Header
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Clone())
		if calls.Add(1) == 1 {
			http.Error(w, `{"detail":"token expired"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse(`{"type":"response.output_item.done","item":{"type":"message","content":[{"type":"output_text","text":"ok"}]}}`, `{"type":"response.completed","response":{"model":"gpt-5.5","usage":{}}}`))
	}))
	defer backend.Close()
	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&refreshBody)
		json.NewEncoder(w).Encode(map[string]string{"id_token": "new.id", "access_token": "new-access", "refresh_token": "new-refresh"})
	}))
	defer tokens.Close()
	auth := writeCodexAuth(t, map[string]any{"id_token": "old.id", "access_token": "old-access", "refresh_token": "old-refresh", "account_id": "acct_1"})
	c := codexTestClient(backend.URL, tokens.URL, auth)

	round, err := c.round(t.Context(), "sys", []any{map[string]any{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": "hi"}}}}, nil, "sess-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if round.Text != "ok" {
		t.Errorf("Text = %q", round.Text)
	}
	if refreshBody["client_id"] != codexClientID || refreshBody["grant_type"] != "refresh_token" || refreshBody["refresh_token"] != "old-refresh" {
		t.Errorf("refresh body = %v", refreshBody)
	}
	if len(seen) != 2 || seen[0].Get("Authorization") != "Bearer old-access" || seen[1].Get("Authorization") != "Bearer new-access" {
		t.Fatalf("authorization headers = %v", seen)
	}
	for _, h := range seen {
		if h.Get("chatgpt-account-id") != "acct_1" || h.Get("OpenAI-Beta") != "responses=experimental" || h.Get("originator") != codexOriginator || h.Get("Content-Type") != "application/json" {
			t.Errorf("headers = %v", h)
		}
		// The backend decides which models an account may use from the client identity, so the user agent and version travel with the originator.
		if !strings.HasPrefix(h.Get("User-Agent"), codexOriginator+"/") || h.Get("version") != codexClientVersion || h.Get("session-id") != "sess-1" {
			t.Errorf("identity headers = %v", h)
		}
	}
	// OpenAI spends a refresh token on use, so the rotated pair must go back to the file the Codex CLI reads or the user is logged out of their own CLI.
	var stored struct {
		AuthMode string            `json:"auth_mode"`
		Tokens   map[string]string `json:"tokens"`
	}
	raw, err := os.ReadFile(auth)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Tokens["access_token"] != "new-access" || stored.Tokens["refresh_token"] != "new-refresh" || stored.Tokens["account_id"] != "acct_1" {
		t.Errorf("stored tokens = %v", stored.Tokens)
	}
	if stored.AuthMode != "chatgpt" {
		t.Errorf("the rest of the file must survive the rewrite, auth_mode = %q", stored.AuthMode)
	}

	always401 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", http.StatusUnauthorized) }))
	defer always401.Close()
	c = codexTestClient(always401.URL, tokens.URL, auth)
	if _, err := c.round(t.Context(), "sys", nil, nil, "", nil); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("second 401 must fail with the status, got %v", err)
	}
}

// A second ask that was waiting on the refresh lock must not spend the rotated refresh token again; it uses the token the first refresh already fetched.
func TestCodexClient_RefreshHappensOnceForConcurrentAsks(t *testing.T) {
	var refreshes atomic.Int32
	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if refreshes.Add(1) > 1 {
			http.Error(w, `{"error":"refresh_token_reused"}`, http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"access_token": "new-access", "refresh_token": "new-refresh"})
	}))
	defer tokens.Close()
	c := codexTestClient("http://127.0.0.1:1/never", tokens.URL, writeCodexAuth(t, map[string]any{"access_token": "old-access", "refresh_token": "old-refresh", "account_id": "acct_1"}))
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.refresh(t.Context(), "old-access"); err != nil {
				t.Errorf("refresh: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := refreshes.Load(); got != 1 {
		t.Errorf("%d refreshes, want 1", got)
	}
	tok, _ := c.currentTokens()
	if tok.AccessToken != "new-access" {
		t.Errorf("access token = %q", tok.AccessToken)
	}
}

// AskCodex mirrors askText: the same handshake and per-turn recall, tools run through evalExecute so the read-only gate holds, the tool result goes back as function_call_output, and the trace carries the answer, the hops and the model.
func TestAskCodex_RunsToolsThroughTheGateAndFillsTheTrace(t *testing.T) {
	var calls atomic.Int32
	var bodies []map[string]any
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			io.WriteString(w, sse(
				`{"type":"response.output_item.done","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"shell_exec","arguments":"{\"command\":\"ls\"}"}}`,
				`{"type":"response.completed","response":{"model":"gpt-5.5","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`))
			return
		}
		io.WriteString(w, sse(
			`{"type":"response.output_item.done","item":{"type":"message","content":[{"type":"output_text","text":"  Done.  "}]}}`,
			`{"type":"response.completed","response":{"model":"gpt-5.5","usage":{"input_tokens":4,"output_tokens":1,"total_tokens":5}}}`))
	}))
	defer backend.Close()
	c := codexTestClient(backend.URL, "http://127.0.0.1:1/never", writeCodexAuth(t, map[string]any{"access_token": "a", "account_id": "acct_1"}))
	a := NewAgent(nil, nil, &toolTestBrain{retrieveRelevantResult: []string{"recalled line"}}, nil, "")

	tr, err := a.askCodex(t.Context(), c, nil, "list my files")
	if err != nil {
		t.Fatal(err)
	}
	if tr.Answer != "Done." || tr.Model != "codex/gpt-5.5" || tr.Channel != ChannelText || tr.Question != "list my files" || tr.Duration <= 0 {
		t.Errorf("trace = %+v", tr)
	}
	if len(tr.ToolHops) != 1 || tr.ToolHops[0].Name != "shell_exec" || tr.ToolHops[0].Args["command"] != "ls" || !strings.Contains(tr.ToolHops[0].Result, "not available in an ask") {
		t.Errorf("hops = %+v", tr.ToolHops)
	}
	if len(tr.Injected) != 1 || tr.Injected[0] != "recalled line" {
		t.Errorf("injected = %v", tr.Injected)
	}
	if len(bodies) != 2 {
		t.Fatalf("%d rounds", len(bodies))
	}
	first := bodies[0]["input"].([]any)
	if len(first) != 1 || first[0].(map[string]any)["role"] != "user" {
		t.Errorf("first input = %v", first)
	}
	if parts := first[0].(map[string]any)["content"].([]any); len(parts) != 2 || !strings.Contains(parts[0].(map[string]any)["text"].(string), "recalled line") || parts[1].(map[string]any)["text"] != "list my files" {
		t.Errorf("first content = %v", parts)
	}
	if bodies[0]["instructions"] == "" || len(bodies[0]["tools"].([]any)) != len(a.askToolDeclarations()) {
		t.Errorf("first body instructions/tools = %v / %d", bodies[0]["instructions"], len(bodies[0]["tools"].([]any)))
	}
	second := bodies[1]["input"].([]any)
	if len(second) != 3 {
		t.Fatalf("second input = %v", second)
	}
	echoed := second[1].(map[string]any)
	if echoed["type"] != "function_call" || echoed["call_id"] != "call_1" || echoed["id"] != "fc_1" {
		t.Errorf("echoed call = %v", echoed)
	}
	out := second[2].(map[string]any)
	if out["type"] != "function_call_output" || out["call_id"] != "call_1" || !strings.Contains(out["output"].(string), "not available in an ask") {
		t.Errorf("output item = %v", out)
	}
}

// When the model sends arguments that don't parse as JSON, the tool must not run with an empty-args fallback: the call is refused and the model sees an "error: " result naming what it sent, so it can retry with valid JSON instead of the tool silently doing the wrong thing.
func TestAskCodex_ToolCallWithUnparsableArgumentsIsRefused(t *testing.T) {
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			io.WriteString(w, sse(
				`{"type":"response.output_item.done","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"shell_exec","arguments":"{not json"}}`,
				`{"type":"response.completed","response":{"model":"gpt-5.5","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`))
			return
		}
		io.WriteString(w, sse(
			`{"type":"response.output_item.done","item":{"type":"message","content":[{"type":"output_text","text":"Done."}]}}`,
			`{"type":"response.completed","response":{"model":"gpt-5.5","usage":{"input_tokens":4,"output_tokens":1,"total_tokens":5}}}`))
	}))
	defer backend.Close()
	c := codexTestClient(backend.URL, "http://127.0.0.1:1/never", writeCodexAuth(t, map[string]any{"access_token": "a", "account_id": "acct_1"}))
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")

	tr, err := a.askCodex(t.Context(), c, nil, "list my files")
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.ToolHops) != 1 {
		t.Fatalf("hops = %+v", tr.ToolHops)
	}
	hop := tr.ToolHops[0]
	if hop.Name != "shell_exec" || len(hop.Args) != 0 {
		t.Errorf("hop = %+v, want empty args and no tool run", hop)
	}
	if !strings.HasPrefix(hop.Result, "error: ") || !strings.Contains(hop.Result, "shell_exec") {
		t.Errorf("result = %q, want an error naming shell_exec", hop.Result)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2 rounds", calls.Load())
	}
}

// TestAskCodex_RetriesOnceAfterARateLimit covers a live run where seven asks in about a minute ended with the backend answering "too many requests in a row" and the whole turn failing. A rate limit lifts on its own, so the ask waits the delay the backend named and asks once more before giving up, exactly as a 401 is answered by one refresh and one retry.
func TestAskCodex_RetriesOnceAfterARateLimit(t *testing.T) {
	var calls int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, `{"error":{"message":"too many requests in a row"}}`, http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse(
			`{"type":"response.output_item.done","item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Done."}]}}`,
			`{"type":"response.completed","response":{"model":"gpt-5.5","usage":{}}}`))
	}))
	defer backend.Close()
	c := codexTestClient(backend.URL, "http://127.0.0.1:1/never", writeCodexAuth(t, map[string]any{"access_token": "a", "account_id": "acct_1"}))
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	tr, err := a.askCodex(t.Context(), c, nil, "what did I do today")
	if err != nil {
		t.Fatalf("a rate limit that lifts must not fail the ask: %v", err)
	}
	if tr.Answer != "Done." {
		t.Errorf("answer = %q", tr.Answer)
	}
	if calls != 2 {
		t.Errorf("%d calls, want the first refused and one retry", calls)
	}
}

// codexScript stands a fake Codex backend up that answers each round with the next scripted stream body and records every request body it received. Input: the test and one SSE body per round, the last of which is repeated once the script runs out. Output: the client pointed at it and a pointer to the recorded bodies, which is only safe to read after the ask has returned.
func codexScript(t *testing.T, bodies *[]map[string]any, rounds ...string) *codexClient {
	t.Helper()
	var n int
	var mu sync.Mutex
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		*bodies = append(*bodies, body)
		i := n
		if i >= len(rounds) {
			i = len(rounds) - 1
		}
		n++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, rounds[i])
	}))
	t.Cleanup(backend.Close)
	return codexTestClient(backend.URL, "http://127.0.0.1:1/never", writeCodexAuth(t, map[string]any{"access_token": "a", "account_id": "acct_1"}))
}

// observeRound is the stream body for a round that calls observe_screen once, with the call id given so each round's call and its output pair up.
func observeRound(id string) string {
	return sse(
		`{"type":"response.output_item.done","item":{"type":"function_call","id":"fc_`+id+`","call_id":"call_`+id+`","name":"observe_screen","arguments":"{}"}}`,
		`{"type":"response.completed","response":{"model":"gpt-5.5","usage":{"input_tokens":100,"output_tokens":10,"total_tokens":110,"input_tokens_details":{"cached_tokens":40}}}}`)
}

// answerRound is the stream body for a round that stops and answers.
const answerRound = `data: {"type":"response.output_item.done","item":{"type":"message","content":[{"type":"output_text","text":"Done."}]}}` + "\n\n" +
	`data: {"type":"response.completed","response":{"model":"gpt-5.5","usage":{"input_tokens":200,"output_tokens":20,"total_tokens":220,"input_tokens_details":{"cached_tokens":150}}}}` + "\n\n"

// What one question cost is what the user pays, so the trace carries the whole ask: how many rounds it took, what each of them was charged as input and output, and how much of that input the backend answered from its prompt cache. Without the round count a 150,000-token ask and a 10,000-token one look the same in the ledger apart from the number.
func TestAskCodex_CountsRoundsAndCachedInput(t *testing.T) {
	var bodies []map[string]any
	c := codexScript(t, &bodies, observeRound("1"), answerRound)
	a, _ := observingAgent(t)

	tr, err := a.askCodex(t.Context(), c, nil, "click the merge button")
	if err != nil {
		t.Fatal(err)
	}
	if tr.Usage.Rounds != 2 {
		t.Errorf("rounds = %d, want 2", tr.Usage.Rounds)
	}
	if tr.Usage.InputTokens != 300 || tr.Usage.OutputTokens != 30 {
		t.Errorf("usage = %+v, want 300 in and 30 out", tr.Usage)
	}
	if tr.Usage.CachedInputTokens != 190 {
		t.Errorf("cached input = %d, want 190", tr.Usage.CachedInputTokens)
	}
}

// lookRound is the stream body for a round that calls look.
func lookRound(id string) string {
	return sse(
		`{"type":"response.output_item.done","item":{"type":"function_call","id":"fc_`+id+`","call_id":"call_`+id+`","name":"look","arguments":"{}"}}`,
		`{"type":"response.completed","response":{"model":"gpt-5.5","usage":{"input_tokens":100,"output_tokens":10,"total_tokens":110}}}`)
}

// The Responses API takes a picture as an input_image content item on a user message, so the one a look took goes in right behind the function output it belongs to. Without this the model would be told the size of a picture it never saw.
func TestAskCodex_SendsTheLookPictureAsAnInputImage(t *testing.T) {
	var bodies []map[string]any
	c := codexScript(t, &bodies, lookRound("1"), answerRound)
	a, _, _ := lookingAgent(t)

	tr, err := a.askCodex(t.Context(), c, nil, "who is who on screen")
	if err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 {
		t.Fatalf("%d rounds, want 2", len(bodies))
	}
	input := bodies[1]["input"].([]any)
	last := input[len(input)-1].(map[string]any)
	if last["type"] != "message" || last["role"] != "user" {
		t.Fatalf("the item after the tool output is %v, want a user message carrying the picture", last)
	}
	part := last["content"].([]any)[0].(map[string]any)
	if part["type"] != "input_image" {
		t.Fatalf("content = %v, want an input_image", part)
	}
	want := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString([]byte("fake-jpeg-bytes"))
	if part["image_url"] != want {
		t.Errorf("image_url = %v, want the picture as a data URL", part["image_url"])
	}
	if tr.ImageTokens != lookTokenCost(1280, 704) {
		t.Errorf("ImageTokens = %d, want %d", tr.ImageTokens, lookTokenCost(1280, 704))
	}
}

// TestCodexRateLimits_ReadsTheHeadersTheBackendSends checks the x-codex-* rate-limit headers become the two usage windows the picker draws. The header names and their meaning are codex-rs/codex-api/src/rate_limits.rs on openai/codex main as of 2026-09: https://github.com/openai/codex/blob/main/codex-rs/codex-api/src/rate_limits.rs, which parses x-codex-primary-used-percent, x-codex-primary-window-minutes and x-codex-primary-reset-at into the RateLimitWindow{used_percent, window_minutes, resets_at} of https://github.com/openai/codex/blob/main/codex-rs/protocol/src/protocol.rs. The window lengths below, 299 and 10079 minutes, are the five-hour and weekly windows a ChatGPT plan reports.
func TestCodexRateLimits_ReadsTheHeadersTheBackendSends(t *testing.T) {
	now := time.Date(2026, 9, 5, 14, 0, 0, 0, time.UTC)
	h := http.Header{}
	h.Set("x-codex-primary-used-percent", "65")
	h.Set("x-codex-primary-window-minutes", "299")
	h.Set("x-codex-primary-reset-at", fmt.Sprint(now.Add(3*time.Hour+56*time.Minute).Unix()))
	h.Set("x-codex-secondary-used-percent", "37.5")
	h.Set("x-codex-secondary-window-minutes", "10079")
	h.Set("x-codex-secondary-reset-at", fmt.Sprint(now.Add(76*time.Hour).Unix()))

	got := codexRateLimits(h, now)
	if len(got) != 2 {
		t.Fatalf("codexRateLimits = %+v, want a primary and a secondary window", got)
	}
	if got[0].Window != "5h" || got[0].UsedFraction != 0.65 {
		t.Errorf("primary = %+v, want the 5h window at 0.65 spent", got[0])
	}
	if want := now.Add(3*time.Hour + 56*time.Minute); !got[0].ResetsAt.Equal(want) {
		t.Errorf("primary resets at %v, want %v", got[0].ResetsAt, want)
	}
	if got[0].Source != "x-codex-primary-*" {
		t.Errorf("primary source = %q, want the headers it was read from", got[0].Source)
	}
	if got[1].Window != "weekly" || got[1].UsedFraction != 0.375 {
		t.Errorf("secondary = %+v, want the weekly window at 0.375 spent", got[1])
	}
}

// A Codex login that can no longer be refreshed is a dead login, and the picker has to say so rather than leave the row available until a question dies on it. Codex is the one brain with no cheap pre-flight probe: its allowance rides the headers of real calls, and the only proof the login still works is a refresh round-trip that spends and rotates the refresh token, so doing one speculatively on every picker render would churn the user's own auth file. The refusal of a refresh is therefore the moment it becomes known, and it has to be recorded when it happens.
// A refusal from the token endpoint is the signed-out case; the endpoint being down is not, and must leave the row alone.
func TestCodexRound_ARefusedRefreshMarksTheLoginSignedOut(t *testing.T) {
	for _, tc := range []struct {
		name      string
		tokenCode int
		auth      map[string]any
		signedOut bool
	}{
		{"the refresh token is refused", http.StatusBadRequest, map[string]any{"id_token": "id.x.y", "access_token": "acc.x.y", "refresh_token": "ref", "account_id": "acct_1"}, true},
		{"there is no refresh token at all", http.StatusOK, map[string]any{"id_token": "id.x.y", "access_token": "acc.x.y", "account_id": "acct_1"}, true},
		{"the token endpoint is down", http.StatusBadGateway, map[string]any{"id_token": "id.x.y", "access_token": "acc.x.y", "refresh_token": "ref", "account_id": "acct_1"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recordedUsage{}
			SetUsageRecorder(rec)
			t.Cleanup(func() { SetUsageRecorder(nil) })

			always401 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
			}))
			defer always401.Close()
			tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.tokenCode)
			}))
			defer tokens.Close()

			c := codexTestClient(always401.URL, tokens.URL, writeCodexAuth(t, tc.auth))
			if _, err := c.round(t.Context(), "you are june", nil, nil, "s1", nil); err == nil {
				t.Fatal("the round answered on a login that could not be refreshed")
			}
			rec.mu.Lock()
			got := rec.signedOut
			rec.mu.Unlock()
			if tc.signedOut && got != ProviderCodex {
				t.Errorf("signed out = %q, want the codex row marked so the picker greys it before a question dies on it", got)
			}
			if !tc.signedOut && got != "" {
				t.Errorf("signed out = %q, want nothing recorded — the token endpoint being down says nothing about the login", got)
			}
		})
	}
}

// Codex does have a cheap pre-flight check after all, and it is the one the CLI itself uses for its /usage card: GET /wham/profiles/me on the ChatGPT backend, with the login's bearer token and account id. It spends no model tokens, rotates nothing, and answers 401 when the login is dead, so unlike a refresh it can be made speculatively whenever the picker renders. Verified against the live endpoint on 2026-09-15.
// It carries no rate-limit windows — those stay in the headers of real calls — so this is a login check and nothing else.
func TestCodexLoginCheck_SaysWhenTheLoginIsDeadWithoutSpendingAnything(t *testing.T) {
	auth := writeCodexAuth(t, map[string]any{"id_token": "id.x.y", "access_token": "acc.x.y", "refresh_token": "ref", "account_id": "acct_1"})
	for _, tc := range []struct {
		name               string
		code               int
		loggedOut, wantErr bool
	}{
		{"the login works", http.StatusOK, false, false},
		{"the login is refused", http.StatusUnauthorized, true, true},
		{"the account is forbidden", http.StatusForbidden, true, true},
		{"the backend is down", http.StatusBadGateway, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotAuth, gotAccount string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotAuth, gotAccount = r.Header.Get("Authorization"), r.Header.Get("ChatGPT-Account-Id")
				w.WriteHeader(tc.code)
				w.Write([]byte(`{"profile":{},"stats":{}}`))
			}))
			defer srv.Close()

			err := codexLoginCheck(t.Context(), srv.Client(), srv.URL, auth)
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, want an error: %v", err, tc.wantErr)
			}
			if got := errors.Is(err, ErrLoggedOut); got != tc.loggedOut {
				t.Errorf("logged out = %v, want %v (err %v)", got, tc.loggedOut, err)
			}
			if tc.code == http.StatusOK {
				if gotAuth != "Bearer acc.x.y" {
					t.Errorf("Authorization = %q, want the login's own access token", gotAuth)
				}
				if gotAccount != "acct_1" {
					t.Errorf("ChatGPT-Account-Id = %q, want the login's account id", gotAccount)
				}
			}
		})
	}
}
