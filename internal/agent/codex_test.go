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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ora/internal/act"
	"ora/internal/db"

	"google.golang.org/genai"
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

// fakeJWT builds an unsigned JWT whose payload is the given claims, the way the account id is read from the CLI's id_token.
func fakeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, _ := json.Marshal(claims)
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc(payload) + ".sig"
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

// When the file has no account_id the id comes from the chatgpt_account_id claim under https://api.openai.com/auth in the id_token, then the access_token.
func TestCodexTokens_AccountIDFallsBackToTheJWTClaim(t *testing.T) {
	claim := map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct_jwt"}}
	tok := codexTokens{IDToken: fakeJWT(t, claim)}
	if got := tok.accountID(); got != "acct_jwt" {
		t.Errorf("from id_token: %q", got)
	}
	tok = codexTokens{IDToken: "not.a.jwt", AccessToken: fakeJWT(t, claim)}
	if got := tok.accountID(); got != "acct_jwt" {
		t.Errorf("from access_token: %q", got)
	}
	if got := (codexTokens{}).accountID(); got != "" {
		t.Errorf("no tokens: %q", got)
	}
}

// An array or nested object parameter keeps its items and properties, because a bare {"type":"array"} is rejected and a flattened object tells the model nothing.
func TestJSONSchema_KeepsArraysAndNestedObjects(t *testing.T) {
	got := jsonSchema(&genai.Schema{
		Type: genai.TypeObject,
		Properties: map[string]*genai.Schema{
			"names": {Type: genai.TypeArray, Items: &genai.Schema{Type: genai.TypeString}},
			"where": {Type: genai.TypeObject, Properties: map[string]*genai.Schema{"city": {Type: genai.TypeString}}, Required: []string{"city"}},
			"mood":  {Type: genai.TypeString, Enum: []string{"calm", "urgent"}},
			"bare":  {},
		},
	})
	props := got["properties"].(map[string]any)
	names := props["names"].(map[string]any)
	if names["type"] != "array" || names["items"].(map[string]any)["type"] != "string" {
		t.Errorf("names = %v", names)
	}
	where := props["where"].(map[string]any)
	nested := where["properties"].(map[string]any)
	if nested["city"].(map[string]any)["type"] != "string" || where["required"].([]string)[0] != "city" {
		t.Errorf("where = %v", where)
	}
	if enum := props["mood"].(map[string]any)["enum"].([]string); len(enum) != 2 {
		t.Errorf("mood = %v", props["mood"])
	}
	// An unset type would serialise as "" and be rejected, so it falls back to a string.
	if props["bare"].(map[string]any)["type"] != "string" {
		t.Errorf("bare = %v", props["bare"])
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

// Any other non-2xx answer is an error naming the status and the start of the body, and the request carried what the backend requires: the model, a non-empty instruction, stream true and store false.
func TestCodexClient_ReportsHTTPErrorsAndSendsTheRequiredFields(t *testing.T) {
	var body map[string]any
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		http.Error(w, `{"error":{"message":"usage limit reached"}}`, http.StatusTooManyRequests)
	}))
	defer backend.Close()
	c := codexTestClient(backend.URL, "http://127.0.0.1:1/never", writeCodexAuth(t, map[string]any{"access_token": "a", "account_id": "acct_1"}))
	_, err := c.round(t.Context(), "sys", nil, codexTools(ToolDeclarations()[:1]), "", nil)
	var httpErr codexHTTPError
	if err == nil || !errors.As(err, &httpErr) || httpErr.Code != 429 || !strings.Contains(err.Error(), "usage limit") {
		t.Errorf("err = %v", err)
	}
	if body["model"] != "gpt-5.5" || body["instructions"] != "sys" || body["stream"] != true || body["store"] != false {
		t.Errorf("body = %v", body)
	}
	if tools, _ := body["tools"].([]any); len(tools) != 1 {
		t.Errorf("tools = %v", body["tools"])
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

// A round that keeps calling tools forever ends with the same out-of-steps error the Gemini path gives, with the hops kept in the trace. shell_exec is refused every time and so is never a look at a screen, which means every round here spends one of the ask's maxAskIterations steps and the loop stops there rather than running on to the round bound (maxAskRounds).
func TestAskCodex_StopsAtTheStepCapWithCapError(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse(
			`{"type":"response.output_item.done","item":{"type":"function_call","id":"fc","call_id":"c","name":"shell_exec","arguments":"{}"}}`,
			`{"type":"response.completed","response":{"model":"gpt-5.5","usage":{}}}`))
	}))
	defer backend.Close()
	c := codexTestClient(backend.URL, "http://127.0.0.1:1/never", writeCodexAuth(t, map[string]any{"access_token": "a", "account_id": "acct_1"}))
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	tr, err := a.askCodex(t.Context(), c, nil, "loop")
	if err == nil || !strings.Contains(err.Error(), "ran out of steps") {
		t.Errorf("err = %v", err)
	}
	if len(tr.ToolHops) != maxAskIterations {
		t.Errorf("%d hops, want exactly the step cap of %d", len(tr.ToolHops), maxAskIterations)
	}
}

// What a turn cost in tokens is the sum of every round it took, not the last round's counts: a question the model answered after two tool calls cost what all three of its calls cost together. The counts are the backend's own, read from each round's response.completed, and the provider is named "codex" so the store can tell this path's calls from Gemini's.
func TestAskCodex_AddsUpTheTokenCountsAcrossRounds(t *testing.T) {
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		switch calls.Add(1) {
		case 1:
			io.WriteString(w, sse(
				`{"type":"response.output_item.done","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"shell_exec","arguments":"{}"}}`,
				`{"type":"response.completed","response":{"model":"gpt-5.5","usage":{"input_tokens":300,"output_tokens":40,"total_tokens":340}}}`))
		case 2:
			io.WriteString(w, sse(
				`{"type":"response.output_item.done","item":{"type":"function_call","id":"fc_2","call_id":"call_2","name":"shell_exec","arguments":"{}"}}`,
				`{"type":"response.completed","response":{"model":"gpt-5.5","usage":{"input_tokens":500,"output_tokens":60,"total_tokens":560}}}`))
		default:
			io.WriteString(w, sse(
				`{"type":"response.output_item.done","item":{"type":"message","content":[{"type":"output_text","text":"done"}]}}`,
				`{"type":"response.completed","response":{"model":"gpt-5.5","usage":{"input_tokens":700,"output_tokens":10,"total_tokens":710}}}`))
		}
	}))
	defer backend.Close()
	c := codexTestClient(backend.URL, "http://127.0.0.1:1/never", writeCodexAuth(t, map[string]any{"access_token": "a", "account_id": "acct_1"}))
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")

	tr, err := a.askCodex(t.Context(), c, nil, "three rounds")
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatalf("%d rounds ran, want 3", calls.Load())
	}
	if tr.Usage.Provider != ProviderCodex {
		t.Errorf("provider = %q, want %q", tr.Usage.Provider, ProviderCodex)
	}
	// 1500, not 700: the last round's counts must not replace the two before it.
	if tr.Usage.InputTokens != 1500 || tr.Usage.OutputTokens != 110 || tr.Usage.TotalTokens != 1610 {
		t.Errorf("usage = %+v, want 1500 in, 110 out, 1610 total", tr.Usage)
	}
}

// The Codex path builds its own request input, so the thread has to go in as the same message items a live turn uses: the prior question as a user message and the prior answer as an assistant message, oldest first, ahead of this turn's context and question. Without history the input is the one message the single-question path always sent.
func TestAskCodex_SendsThePriorTurnsAsMessageItems(t *testing.T) {
	var bodies []map[string]any
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse(
			`{"type":"response.output_item.done","item":{"type":"message","content":[{"type":"output_text","text":"Ringed it again."}]}}`,
			`{"type":"response.completed","response":{"model":"gpt-5.5","usage":{}}}`))
	}))
	defer backend.Close()
	c := codexTestClient(backend.URL, "http://127.0.0.1:1/never", writeCodexAuth(t, map[string]any{"access_token": "a", "account_id": "acct_1"}))
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")

	history := HistoryFromTurns([]db.Turn{
		{Role: "you", Text: "draw a ring around any one button you can see", Kind: "ask"},
		{Role: "ora", Text: "I ringed the Pause button.", Kind: "ask"},
	})
	if _, err := a.askCodex(t.Context(), c, history, "do it again"); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 1 {
		t.Fatalf("%d rounds, want 1", len(bodies))
	}
	input := bodies[0]["input"].([]any)
	if len(input) != 3 {
		t.Fatalf("input has %d items, want the prior question, the prior answer and this question: %v", len(input), input)
	}
	asked := input[0].(map[string]any)
	if asked["type"] != "message" || asked["role"] != "user" {
		t.Errorf("prior question item = %v", asked)
	}
	part := asked["content"].([]any)[0].(map[string]any)
	if part["type"] != "input_text" || part["text"] != "draw a ring around any one button you can see" {
		t.Errorf("prior question content = %v", part)
	}
	answered := input[1].(map[string]any)
	if answered["type"] != "message" || answered["role"] != "assistant" {
		t.Errorf("prior answer item = %v", answered)
	}
	part = answered["content"].([]any)[0].(map[string]any)
	if part["type"] != "output_text" || part["text"] != "I ringed the Pause button." {
		t.Errorf("prior answer content = %v", part)
	}
	live := input[2].(map[string]any)
	if live["role"] != "user" || len(live["content"].([]any)) != 2 {
		t.Errorf("this turn's item = %v", live)
	}
	if last := live["content"].([]any)[1].(map[string]any); last["text"] != "do it again" {
		t.Errorf("this turn's question = %v", last)
	}

	if _, err := a.askCodex(t.Context(), c, nil, "do it again"); err != nil {
		t.Fatal(err)
	}
	if input := bodies[1]["input"].([]any); len(input) != 1 {
		t.Errorf("a question asked with no history sent %d items, want 1", len(input))
	}
}

// TestCodexRetryWait_OnlyWaitsOutARateLimitBeforeAnyToolHasRun pins when a failed round is worth one more try. A 429 is the one failure that fixes itself given a moment, so it earns the single retry — but only before a tool has run, the same rule the Gemini handover follows, because a retry after a tool has run would take the user's action a second time. Any other failure, and any delay longer than the cap, is reported rather than waited out.
func TestCodexRetryWait_OnlyWaitsOutARateLimitBeforeAnyToolHasRun(t *testing.T) {
	limited := codexHTTPError{Code: 429, Body: "too many requests", RetryAfter: 3 * time.Second}
	if wait, ok := codexRetryWait(limited, 0); !ok || wait != 3*time.Second {
		t.Errorf("wait, ok = %v, %v; want the 3s the backend named", wait, ok)
	}
	if _, ok := codexRetryWait(limited, 1); ok {
		t.Error("a 429 after a tool has already run must not be retried")
	}
	if wait, ok := codexRetryWait(codexHTTPError{Code: 429, Body: "slow down"}, 0); !ok || wait != codexDefaultRetryWait {
		t.Errorf("wait, ok = %v, %v; want the default wait when the backend named none", wait, ok)
	}
	if _, ok := codexRetryWait(codexHTTPError{Code: 429, RetryAfter: codexMaxRetryWait + time.Second}, 0); ok {
		t.Error("a wait longer than the cap must be reported rather than sat through")
	}
	if _, ok := codexRetryWait(codexHTTPError{Code: 500, Body: "boom"}, 0); ok {
		t.Error("only a rate limit earns the retry")
	}
	if _, ok := codexRetryWait(nil, 0); ok {
		t.Error("a round that did not fail must not be retried")
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

// outputTexts pulls the function_call_output strings out of one recorded request body, in the order they were sent.
func outputTexts(body map[string]any) []string {
	var out []string
	for _, item := range body["input"].([]any) {
		m, ok := item.(map[string]any)
		if !ok || m["type"] != "function_call_output" {
			continue
		}
		out = append(out, m["output"].(string))
	}
	return out
}

// A screen listing is worth its tokens in the round straight after the look that produced it and worthless in every round after that: the numbers in an old list are dead, because only the newest observe_screen list is the one click and point_at resolve against. Sending them all again every round was the single largest line in the 2026-09-05 bill, so every listing a later listing supersedes is replaced by a one-line note.
func TestAskCodex_KeepsOnlyTheNewestScreenListing(t *testing.T) {
	var bodies []map[string]any
	c := codexScript(t, &bodies, observeRound("1"), observeRound("2"), answerRound)
	a := movingScreenAgent(t)

	if _, err := a.askCodex(t.Context(), c, nil, "click the merge button"); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 3 {
		t.Fatalf("%d rounds, want 3", len(bodies))
	}
	second := outputTexts(bodies[1])
	if len(second) != 1 || !strings.Contains(second[0], `[1] push button "Merge"`) {
		t.Fatalf("the round straight after a look must carry the whole list, got %q", second)
	}
	third := outputTexts(bodies[2])
	if len(third) != 2 {
		t.Fatalf("%d outputs in the last round, want 2", len(third))
	}
	if third[0] != supersededListingNote {
		t.Errorf("superseded listing = %q, want the note %q", third[0], supersededListingNote)
	}
	if !strings.Contains(third[1], `[1] push button "Approve"`) {
		t.Errorf("the newest listing must survive whole, got %q", third[1])
	}
}

// Once a turn has called a screen tool it is a screen task, and the handshake it opened with — how to talk, what memory is for, the personal context block — is no longer what the model needs. From that round on the instruction is the screen-task guidance and the stop line alone, which is both far shorter and byte-identical from one ask to the next, so the prompt cache can match it.
// A question that says nothing about a screen still opens on the whole handshake and the whole tool list — it may turn out to be a memory question, and the screen prompt teaches nothing about who the people in the user's life are — but once the first round's own observe_screen call proves it a screen task, the next round drops the conversational handshake for the far shorter screen-task guidance and the stop line, and narrows the tool list to the screen set.
func TestAskCodex_TrimsTheInstructionOnceItIsAScreenTask(t *testing.T) {
	var bodies []map[string]any
	c := codexScript(t, &bodies, observeRound("1"), answerRound)
	a, _ := observingAgent(t)

	if _, err := a.askCodex(t.Context(), c, nil, "what did we settle on for the venue"); err != nil {
		t.Fatal(err)
	}
	first := bodies[0]["instructions"].(string)
	if !strings.Contains(first, "composed, dry-witted aide") {
		t.Fatalf("the first round must still carry the whole handshake, got %d bytes", len(first))
	}
	if got := len(bodies[0]["tools"].([]any)); got != len(a.askToolDeclarations()) {
		t.Errorf("round 0 offered %d tools, want every one of them", got)
	}

	second := bodies[1]["instructions"].(string)
	if strings.Contains(second, "composed, dry-witted aide") {
		t.Errorf("a screen round still carried the conversational handshake, %d bytes", len(second))
	}
	if !strings.Contains(second, "one task to see through") {
		t.Errorf("a screen round must keep the screen-task guidance, got %q", second)
	}
	if !strings.Contains(second, stopLineText) {
		t.Errorf("a screen round must keep the stop line, got %q", second)
	}
	if len(second) >= len(first)/2 {
		t.Errorf("the screen instruction is %d bytes against the handshake's %d; it must be far shorter", len(second), len(first))
	}

	names := func(body map[string]any) []string {
		var out []string
		for _, tool := range body["tools"].([]any) {
			out = append(out, tool.(map[string]any)["name"].(string))
		}
		return out
	}
	screenNames := names(bodies[1])
	for _, want := range []string{"observe_screen", "click", "scroll_to", "type_text", "point_at", "open_url"} {
		if !slices.Contains(screenNames, want) {
			t.Errorf("a screen round must still offer %s, offered %v", want, screenNames)
		}
	}
	for _, gone := range []string{"query_memory", "query_store", "recall", "personal_context", "revise", "action_items"} {
		if slices.Contains(screenNames, gone) {
			t.Errorf("a screen round still offered %s", gone)
		}
	}
}

// The instruction a screen round sends carries no clock, no context block and no question, so it is the same bytes on every screen round of every ask. That is what lets the backend's prompt cache match it across asks rather than only within one.
func TestAskCodex_ScreenInstructionIsTheSameBytesAcrossAsks(t *testing.T) {
	var first, second []map[string]any
	a, _ := observingAgent(t)
	if _, err := a.askCodex(t.Context(), codexScript(t, &first, observeRound("1"), answerRound), nil, "what did we settle on for the venue"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.askCodex(t.Context(), codexScript(t, &second, observeRound("1"), answerRound), nil, "something else entirely, asked at another moment"); err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || len(second) != 2 {
		t.Fatalf("%d and %d rounds, want 2 each", len(first), len(second))
	}
	if first[1]["instructions"] == first[0]["instructions"] {
		t.Error("the screen round sent the handshake again; this test proves nothing unless the two differ")
	}
	if first[1]["instructions"] != second[1]["instructions"] {
		t.Errorf("two asks sent different screen instructions:\n%q\n%q", first[1]["instructions"], second[1]["instructions"])
	}
}

// A prompt cache matches a prefix, so a round that opens on different bytes than the round before it starts from nothing. When the question's own words already name a screen task there is nothing to learn from the first round, so the ask opens on the screen instruction, the short tool list and the cut thread and never changes any of them: the instructions, the tools and the leading input items are the same bytes on every round, and only what each round appends behind them differs.
func TestAskCodex_ScreenAskSendsOnePrefixOnEveryRound(t *testing.T) {
	var bodies []map[string]any
	c := codexScript(t, &bodies, observeRound("1"), observeRound("2"), answerRound)
	a, _ := observingAgent(t)

	history := HistoryFromTurns([]db.Turn{
		{Role: "you", Text: "first question", Kind: "ask"},
		{Role: "ora", Text: "first answer", Kind: "ask"},
		{Role: "you", Text: "second question", Kind: "ask"},
		{Role: "ora", Text: "second answer", Kind: "ask"},
		{Role: "you", Text: "third question", Kind: "ask"},
		{Role: "ora", Text: "third answer", Kind: "ask"},
	})
	if _, err := a.askCodex(t.Context(), c, history, "click the merge button"); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 3 {
		t.Fatalf("%d rounds, want 3", len(bodies))
	}
	// The kept thread is maxScreenHistoryTurns items plus the turn item carrying this question, and that is the whole of what every round sends before its own trail.
	const prefixItems = maxScreenHistoryTurns + 1
	prefix := func(body map[string]any) string {
		raw, err := json.Marshal(body["input"].([]any)[:prefixItems])
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	for i := 1; i < len(bodies); i++ {
		if bodies[i]["instructions"] != bodies[0]["instructions"] {
			t.Errorf("round %d sent a different instruction than round 0:\n%q\n%q", i, bodies[i]["instructions"], bodies[0]["instructions"])
		}
		got, want := toolsJSON(t, bodies[i]), toolsJSON(t, bodies[0])
		if got != want {
			t.Errorf("round %d sent a different tool list than round 0:\n%s\n%s", i, got, want)
		}
		if prefix(bodies[i]) != prefix(bodies[0]) {
			t.Errorf("round %d sent different leading input items than round 0:\n%s\n%s", i, prefix(bodies[i]), prefix(bodies[0]))
		}
	}
	if !strings.Contains(bodies[0]["instructions"].(string), "one task to see through") {
		t.Errorf("round 0 of a question naming a screen task must open on the screen instruction, got %q", bodies[0]["instructions"])
	}
	if got := len(bodies[0]["tools"].([]any)); got != len(screenRoundTools) {
		t.Errorf("round 0 offered %d tools, want the %d screen ones", got, len(screenRoundTools))
	}
}

// toolsJSON renders one recorded request's tool list as JSON so two rounds' copies of it can be compared as bytes. Input: the test and the recorded body. Output: the tool list's JSON text.
func toolsJSON(t *testing.T, body map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(body["tools"])
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// A screen task refers to what is on the screen now, not to what was said six questions ago, and the thread is re-sent whole on every round. Once the turn is a screen task the thread is cut to the last two turns, which is what a follow-up like "do it again" actually reaches for.
func TestAskCodex_CutsTheThreadOnAScreenTask(t *testing.T) {
	var bodies []map[string]any
	c := codexScript(t, &bodies, observeRound("1"), answerRound)
	a, _ := observingAgent(t)

	history := HistoryFromTurns([]db.Turn{
		{Role: "you", Text: "first question", Kind: "ask"},
		{Role: "ora", Text: "first answer", Kind: "ask"},
		{Role: "you", Text: "second question", Kind: "ask"},
		{Role: "ora", Text: "second answer", Kind: "ask"},
		{Role: "you", Text: "third question", Kind: "ask"},
		{Role: "ora", Text: "third answer", Kind: "ask"},
	})
	if _, err := a.askCodex(t.Context(), c, history, "what did we settle on for the venue"); err != nil {
		t.Fatal(err)
	}
	messages := func(body map[string]any) []string {
		var out []string
		for _, item := range body["input"].([]any) {
			m, ok := item.(map[string]any)
			if !ok || m["type"] != "message" {
				continue
			}
			for _, part := range m["content"].([]any) {
				out = append(out, part.(map[string]any)["text"].(string))
			}
		}
		return out
	}
	if got := messages(bodies[0]); len(got) < 8 {
		t.Fatalf("the first round must carry the whole thread, got %d message parts: %q", len(got), got)
	}
	second := strings.Join(messages(bodies[1]), "\n")
	if strings.Contains(second, "first question") || strings.Contains(second, "second question") {
		t.Errorf("a screen round still carried the older turns: %q", second)
	}
	if !strings.Contains(second, "third question") || !strings.Contains(second, "third answer") {
		t.Errorf("a screen round must keep the last two turns, got %q", second)
	}
	if !strings.Contains(second, "what did we settle on for the venue") {
		t.Errorf("a screen round must keep the question being asked, got %q", second)
	}
}

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

// The prompt cache key routes a request to the machine holding the prefix it starts with, so a key freshly made for every ask throws away every hit the next ask could have had on the same instruction and the same tools. The key is stable across asks; the session id, which names one ask, stays fresh per ask.
func TestAskCodex_SendsAStablePromptCacheKeyAcrossAsks(t *testing.T) {
	var first, second []map[string]any
	var sessions []string
	record := func(bodies *[]map[string]any) *codexClient {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			*bodies = append(*bodies, body)
			sessions = append(sessions, r.Header.Get("session-id"))
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, answerRound)
		}))
		t.Cleanup(backend.Close)
		return codexTestClient(backend.URL, "http://127.0.0.1:1/never", writeCodexAuth(t, map[string]any{"access_token": "a", "account_id": "acct_1"}))
	}
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	if _, err := a.askCodex(t.Context(), record(&first), nil, "one question"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.askCodex(t.Context(), record(&second), nil, "another question"); err != nil {
		t.Fatal(err)
	}
	key, _ := first[0]["prompt_cache_key"].(string)
	if key == "" {
		t.Fatal("no prompt cache key was sent")
	}
	if second[0]["prompt_cache_key"] != key {
		t.Errorf("two asks sent cache keys %q and %q; they must match for the shared prefix to hit", key, second[0]["prompt_cache_key"])
	}
	if len(sessions) != 2 || sessions[0] == sessions[1] {
		t.Errorf("session ids = %v, want one per ask", sessions)
	}
}

// A turn that has already called a memory tool keeps every definition for the rest of its rounds, whatever it does on the screen afterwards: the call it made is echoed back into every later round, and answering it with a tool the request no longer declares is a request the backend can reject. (The tool definitions are 3,091 tokens, measured against the real backend on 2026-09-05, and two thirds of them describe memory tools a screen round has no use for — see TestAskCodex_TrimsTheInstructionOnceItIsAScreenTask for the round that narrows to the screen set and open_url.)
func TestAskCodex_KeepsEveryToolOnceOneOutsideTheScreenSetHasRun(t *testing.T) {
	var bodies []map[string]any
	c := codexScript(t, &bodies,
		sse(`{"type":"response.output_item.done","item":{"type":"function_call","id":"fc_m","call_id":"call_m","name":"query_memory","arguments":"{\"query\":\"the venue\"}"}}`,
			`{"type":"response.completed","response":{"model":"gpt-5.5","usage":{}}}`),
		observeRound("1"), answerRound)
	a, _ := observingAgent(t)
	if _, err := a.askCodex(t.Context(), c, nil, "what did we say about the venue, and click it"); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 3 {
		t.Fatalf("%d rounds, want 3", len(bodies))
	}
	if got := len(bodies[2]["tools"].([]any)); got != len(a.askToolDeclarations()) {
		t.Errorf("the last round offered %d tools, want all %d kept because a memory tool had already run", got, len(a.askToolDeclarations()))
	}
}

// TestSystemInstructionText_ClockSentenceIsLastForPromptCaching pins the reordering that makes the Codex backend's prompt-prefix cache actually pay: measured against the real endpoint on 2026-09-05, the clock sentence used to sit ahead of the personal-context block and the memory context, so those bytes — and everything after them — moved every ask and could never be served from cache. The clock sentence is now the very last thing in the prompt, so two asks a minute apart, with the same personal context and memory context, are byte-identical up to it.
func TestSystemInstructionText_ClockSentenceIsLastForPromptCaching(t *testing.T) {
	personal := "Personal context — things known for certain about the user and their world:\n  Their name is Vexil."
	context := "  [working] Brave: some tab"
	a := systemInstructionText(time.Date(2026, 9, 5, 9, 0, 0, 0, time.UTC), "linux", "amd64", "sh", personal, context, 20)
	b := systemInstructionText(time.Date(2026, 9, 5, 9, 1, 0, 0, time.UTC), "linux", "amd64", "sh", personal, context, 20)

	if a == b {
		t.Fatal("a minute apart must still render a different clock sentence")
	}
	if !strings.Contains(a, personal) || !strings.Contains(a, context) {
		t.Fatal("the prompt must still carry the personal context and the memory context")
	}

	shared := 0
	for shared < len(a) && shared < len(b) && a[shared] == b[shared] {
		shared++
	}
	// Only the trailing clock sentence, and nothing before it, may differ: the shared head must reach past where the personal block and the memory context were rendered.
	if want := strings.Index(a, personal); shared < want {
		t.Errorf("the shared prefix ends at byte %d, before the personal context block at %d — something ahead of it still moves with the clock", shared, want)
	}
	if want := strings.Index(a, context); shared < want {
		t.Errorf("the shared prefix ends at byte %d, before the memory context at %d — something ahead of it still moves with the clock", shared, want)
	}
	// The clock sentence itself is short; anything left undifferentiated much past its own length means static text after it is moving too.
	if tail := len(a) - shared; tail > 200 {
		t.Errorf("%d bytes differ after the shared prefix, want roughly the clock sentence alone (well under 200)", tail)
	}
	if !strings.HasSuffix(strings.TrimRight(a, "."), "concrete since/until dates yourself") {
		t.Errorf("the clock sentence must be the last thing in the prompt, got prompt ending %q", a[len(a)-120:])
	}
}

// A screen round used to drop the personal-context store entirely once a turn became a screen task (see TestAskCodex_TrimsTheInstructionOnceItIsAScreenTask), which meant a round choosing where to click also lost the user's own name and anything the store knew about the app in front of it. Trimmed to what the front app or the user's identity actually need, it comes back.
func TestAskCodex_ScreenRoundCarriesTrimmedPersonalContext(t *testing.T) {
	var bodies []map[string]any
	c := codexScript(t, &bodies, observeRound("1"), answerRound)
	a := NewAgent(nil, nil, &toolTestBrain{personal: map[string]string{
		"identity": "Their name is Vexil.",
		"browser":  "Uses Brave as their daily browser.",
		"diet":     "Vegetarian, no onion or garlic.",
	}}, nil, "")
	a.observe = func(ctx context.Context) (string, string, []act.Node, error) {
		return "brave", "PR #13 · GitHub", []act.Node{
			{Role: "push button", Label: "Merge", X: 10, Y: 20, W: 80, H: 30, Showing: true, Ref: "r-merge"},
		}, nil
	}

	if _, err := a.askCodex(t.Context(), c, nil, "what did we settle on for the venue"); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 {
		t.Fatalf("%d rounds, want 2", len(bodies))
	}
	second := bodies[1]["instructions"].(string)
	if !strings.Contains(second, "Their name is Vexil.") {
		t.Errorf("a screen round must still carry the user's own identity, got %q", second)
	}
	if !strings.Contains(second, "Uses Brave as their daily browser.") {
		t.Errorf("a screen round must carry a personal entry naming the front app, got %q", second)
	}
	if strings.Contains(second, "Vegetarian") {
		t.Errorf("a screen round must not carry an entry naming neither the app nor the user, got %q", second)
	}
}

// bigScreenListingSize is how many nodes bigScreenAgent's screen carries: real runs on 2026-09-05 measured about 1,300 tokens for one observe_screen listing (see screenRoundTools's doc comment on the same package), which is roughly this many items at this shape.
const bigScreenListingSize = 40

// bigScreenNodes builds n act.Node items with distinct, page-length labels, tagged with set so two calls never render as the same page.
func bigScreenNodes(set, n int) []act.Node {
	nodes := make([]act.Node, n)
	for i := 0; i < n; i++ {
		nodes[i] = act.Node{
			Role: "link", Label: fmt.Sprintf("Item %d-%d", set, i),
			X: i, Y: i, W: 80, H: 20, Showing: true, Ref: fmt.Sprintf("r-%d-%d", set, i),
		}
	}
	return nodes
}

// bigScreenAgent is observingAgent with a full-sized page rather than the two-item fixture the rest of this file uses: every observe_screen look returns bigScreenListingSize nodes, alternating between two entirely different sets so every look renders as a full listing (see observeResult in tools.go) rather than the "unchanged" or "changed lines" shortcuts a short fixture page would take — matching the size of a real, content-heavy page.
func bigScreenAgent(t *testing.T) *Agent {
	t.Helper()
	a, _ := observingAgent(t)
	calls := 0
	a.observe = func(ctx context.Context) (string, string, []act.Node, error) {
		calls++
		return "brave", "a long real page title · PR #13 · GitHub", bigScreenNodes(calls, bigScreenListingSize), nil
	}
	return a
}

// TestAskCodex_ScreenRoundsStayUnderThreeThousandTokens is the regression guard (the bound is now 3,700, see screenRoundTokenBudget) for the trimming this file does. Real runs on 2026-09-05 measured about 10.6k input tokens on the opening round of a screen task, cut by the personal-context and tool-set trimming to about 5.4k, and about 3.0k on every round after that — the rounds a screen task actually spends most of its budget on, since the first round is one-time cost and every later one repeats. This replays four looks at a full-sized page (see bigScreenAgent) — the shape a real multi-step screen task takes, since the model observes again after every action — through the real round-building path in askCodex, and checks every round after the first stays under that 3k figure, using the tokenizer estimate the rest of the codebase already applies to a provider that reports none: four characters to the token (see internal/tally/weekly.go's promptChars/4).
// This stands in for a replay against a copy of the user's own act_runs table, which a unit test cannot depend on existing or being safe to read while the daemon has it open. The sizes it replays are not invented: they come from the token counts already measured against the real backend and recorded in this package's comments (see screenRoundTools, observe_screen's docs), the same evidence a db-backed replay would be checked against.
func TestAskCodex_ScreenRoundsStayUnderThreeThousandTokens(t *testing.T) {
	var bodies []map[string]any
	c := codexScript(t, &bodies, observeRound("1"), observeRound("2"), answerRound)
	a := bigScreenAgent(t)

	if _, err := a.askCodex(t.Context(), c, nil, "click the item about the merge and read it out"); err != nil {
		t.Fatal(err)
	}
	if len(bodies) < 3 {
		t.Fatalf("%d rounds, want at least 3", len(bodies))
	}

	const tokenCharDivisor = 4 // the estimate internal/tally/weekly.go already uses for a provider that reports no usage
	// Every round's size is logged, round 0 included, because this replay is also where the round-0 drop is measured. Back-to-back runs of this test on 2026-09-05, with and without the change that lets a question naming a screen task open on the screen instruction and the screen tool list: round 0 went from 27,710 bytes (about 6,927 tokens) to 10,041 (about 2,510), and rounds 1 and 2 were unchanged at 11,553 and 11,857 bytes. What rounds 1 and 2 gained is not size but a prefix they share with round 0, which is what the backend's cache can match.
	for i, body := range bodies {
		raw, _ := json.Marshal(body)
		t.Logf("round %d: %d bytes, about %d tokens", i, len(raw), len(raw)/tokenCharDivisor)
	}
	// The bound started the day at 3,000, was raised to 3,500 for press_key, click_at and scroll_at, and to 3,700 for switch_window, which buys the one thing no other tool in the round can do at all: bringing another application forward, which GNOME gives an unprivileged daemon no other way to do. It is back to 3,000 because the fifteen declarations were then cut from 9,022 bytes to 6,330 (see TestScreenRoundDeclarations_StayShort), which took a round of this replay from 3,637 tokens to 2,964. Every tool added here costs its declaration on every round of every screen task, so the next one has to pay for itself or replace one.
	const screenRoundTokenBudget = 3000
	// Round 0 is checked too, not only the rounds after it: the question names a screen task in its own words, so it opens on the screen instruction and the screen tool list like every other round of the ask.
	for i, body := range bodies {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("round %d: marshal the recorded request: %v", i, err)
		}
		if tokens := len(raw) / tokenCharDivisor; tokens > screenRoundTokenBudget {
			t.Errorf("round %d is about %d tokens (estimated from a %d-byte request), want under %d", i, tokens, len(raw), screenRoundTokenBudget)
		}
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

// TestActionHops_CountsOnlyToolsThatChangedSomething checks the hand-over rule counts clicks and typing, not reads: a routine that looked at the screen and then hit Gemini's quota must still be handed to Codex, since repeating a look changes nothing on the user's machine.
func TestActionHops_CountsOnlyToolsThatChangedSomething(t *testing.T) {
	hops := []ToolHop{{Name: "observe_screen"}, {Name: "search_memory"}, {Name: "look"}, {Name: "point_at"}}
	if got := actionHops(hops); got != 0 {
		t.Fatalf("read-only hops counted as %d actions, want 0", got)
	}
	hops = append(hops, ToolHop{Name: "click"}, ToolHop{Name: "type_text"}, ToolHop{Name: "press_key"}, ToolHop{Name: "click_at"})
	if got := actionHops(hops); got != 4 {
		t.Fatalf("four actions counted as %d", got)
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

// TestCodexRateLimits_TakesTheOlderResetAfterSpelling checks the delay-shaped header an older backend sends is read too, since the two spellings are both in the wild: x-codex-primary-reset-after-seconds counts forward from now, x-codex-primary-reset-at is an absolute unix second. Source: https://github.com/headroomlabs-ai/headroom/issues/577 lists the reset-after-seconds family, and https://github.com/openai/codex/blob/main/codex-rs/codex-api/src/rate_limits.rs the reset-at one.
func TestCodexRateLimits_TakesTheOlderResetAfterSpelling(t *testing.T) {
	now := time.Date(2026, 9, 5, 14, 0, 0, 0, time.UTC)
	h := http.Header{}
	h.Set("x-codex-primary-used-percent", "10")
	h.Set("x-codex-primary-window-minutes", "299")
	h.Set("x-codex-primary-reset-after-seconds", "17940")

	got := codexRateLimits(h, now)
	if len(got) != 1 {
		t.Fatalf("codexRateLimits = %+v, want the one window the headers named", got)
	}
	if want := now.Add(17940 * time.Second); !got[0].ResetsAt.Equal(want) {
		t.Errorf("resets at %v, want %v", got[0].ResetsAt, want)
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
			if _, err := c.round(t.Context(), "you are ora", nil, nil, "s1", nil); err == nil {
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

// A window the headers describe with neither a length nor a reset is not a window anyone can read. This backend sends x-codex-secondary-used-percent with no window-minutes and no reset-at, which drew a bar in the picker labelled "Secondary", sitting at 0%, resetting in the year 1 — worse than drawing nothing, because it reads as a real allowance that is untouched.
func TestCodexRateLimits_DropsAWindowWithNoLengthAndNoReset(t *testing.T) {
	h := http.Header{}
	h.Set("x-codex-primary-used-percent", "100")
	h.Set("x-codex-primary-window-minutes", "43200")
	h.Set("x-codex-primary-reset-at", "1791073410")
	h.Set("x-codex-secondary-used-percent", "0")

	limits := codexRateLimits(h, time.Now())
	if len(limits) != 1 {
		t.Fatalf("read %d windows, want only the one the headers actually describe: %+v", len(limits), limits)
	}
	if limits[0].Window != "monthly" {
		t.Errorf("window = %q, want monthly for a 43200-minute window", limits[0].Window)
	}
}
