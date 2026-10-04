package brain

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"june/internal/agent"
	"june/internal/config"
)

// TestMain lets the test binary stand in for the claude, agy and grok CLIs. A brain under test runs this test binary as its CLI with the arguments the production code builds; with JUNE_FAKE_CLI set, the child acts out the fake run described by the JUNE_FAKE_CLI_* variables and exits before the testing package ever parses those arguments as its own flags. This works the same on Linux and Windows, where a shell-script stub cannot run.
func TestMain(m *testing.M) {
	if os.Getenv("JUNE_FAKE_CLI") == "1" {
		fakeCLIMain()
	}
	// The routing tests hand duties on, which the user's own allow_fallback setting can forbid; they test the router, not the config of the machine running them.
	dutyFallbackAllowed = func() bool { return true }
	os.Exit(m.Run())
}

// fakeCLIMain is one run of the fake CLI. Input, from the environment: JUNE_FAKE_CLI_STDIN_TO names a file to copy stdin into, JUNE_FAKE_CLI_OUT is written to stdout, JUNE_FAKE_CLI_ERR to stderr, JUNE_FAKE_CLI_SLEEP is how long to linger afterwards and JUNE_FAKE_CLI_EXIT the exit code. Output: none; it always exits the process.
func fakeCLIMain() {
	if path := os.Getenv("JUNE_FAKE_CLI_STDIN_TO"); path != "" {
		in, _ := io.ReadAll(os.Stdin)
		os.WriteFile(path, in, 0o600)
	}
	fmt.Fprint(os.Stdout, os.Getenv("JUNE_FAKE_CLI_OUT"))
	fmt.Fprint(os.Stderr, os.Getenv("JUNE_FAKE_CLI_ERR"))
	if d, err := time.ParseDuration(os.Getenv("JUNE_FAKE_CLI_SLEEP")); err == nil {
		time.Sleep(d)
	}
	code, _ := strconv.Atoi(os.Getenv("JUNE_FAKE_CLI_EXIT"))
	os.Exit(code)
}

// fakeCLI makes the next CLI run in this test print out on stdout. Input: what the fake prints. Output: the binary to hand the brain, which is this test binary.
func fakeCLI(t *testing.T, out string) string {
	t.Helper()
	t.Setenv("JUNE_FAKE_CLI", "1")
	t.Setenv("JUNE_FAKE_CLI_OUT", out)
	// The absolute path, since runCLI starts the child in the temp directory and a relative argv[0] would not resolve from there.
	bin, err := os.Executable()
	if err != nil {
		t.Fatalf("locate the test binary: %v", err)
	}
	return bin
}

// TestCLIBrains drives each CLI brain against a fake run and checks what the brain makes of the CLI's output: the answer field it reads, the failures it reports, and that the CLI's own words never reach the returned error, since callers log these errors at warn and the prompt is a day of the user's screen text.
func TestCLIBrains(t *testing.T) {
	claude := func(bin string, timeout int) Brain { return ClaudeCLI(bin, "", timeout) }
	agy := func(bin string, timeout int) Brain { return AgyCLI(bin, timeout) }
	grok := func(bin string, timeout int) Brain { return GrokCLI(bin, timeout) }
	tests := []struct {
		name    string
		brain   func(bin string, timeout int) Brain
		out     string
		errOut  string
		exit    string
		sleep   string
		timeout int
		want    string
		wantErr string
		// wantNotErr is text the CLI printed that must never reach the returned error.
		wantNotErr string
		// wantStdin is what the CLI must have read on stdin: claude takes the prompt there, because a meeting transcript is longer than one argv entry may be.
		wantStdin string
	}{
		{name: "claude's result field is the answer", brain: claude, out: `{"is_error":false,"subtype":"success","result":"# Meeting minutes\nall good\n"}`, want: "# Meeting minutes\nall good", wantStdin: "summarise this"},
		{name: "a claude is_error run fails with the subtype and not the CLI's own words", brain: claude, out: `{"is_error":true,"subtype":"error_during_execution","result":"Credit balance is too low"}`, wantErr: "error_during_execution", wantNotErr: "Credit balance is too low"},
		{name: "an empty claude result is not an answer", brain: claude, out: `{"is_error":false,"result":""}`, wantErr: "no text"},
		{name: "output that is not JSON fails without quoting the output", brain: claude, out: "Invalid API key · Please run /login", wantErr: "parse", wantNotErr: "Please run /login"},
		{name: "a non-zero exit carries the exit status and not the child's stderr", brain: claude, errOut: "not logged in", exit: "1", wantErr: "exit status 1", wantNotErr: "not logged in"},
		{name: "a run that outlives the timeout is killed", brain: claude, sleep: "5s", timeout: 1, wantErr: "timed out"},
		// On 2026-09-08 a meeting's minutes were lost to "claude timed out after 5m0s" with the answer already printed: the CLI had written its result and not exited. The answer is complete once stdout holds one whole JSON value, and that is when the runner returns.
		{name: "a run that prints its result and then lingers is not waited for", brain: claude, out: `{"is_error":false,"subtype":"success","result":"done"}`, sleep: "5s", timeout: 2, want: "done"},
		// agy takes the prompt on stdin as a stream-json turn, opened with the line telling it to use no tools, and prints stream-json back, so its answer is the result event's response field.
		{name: "agy's response field is the answer", brain: agy, out: "{\"event\":\"init\"}\n{\"event\":\"step_update\",\"step_update\":{\"text_delta\":\"ok\"}}\n{\"event\":\"result\",\"result\":{\"status\":\"SUCCESS\",\"response\":\"ok\\n\"}}\n", want: "ok", wantStdin: agyTurn(agyTextOnly + "summarise this")},
		{name: "an agy status other than SUCCESS is an error", brain: agy, out: "{\"event\":\"result\",\"result\":{\"status\":\"ERROR\",\"response\":\"\"}}\n", wantErr: "ERROR"},
		{name: "grok's text field is the answer", brain: grok, out: `{"text":"ok\n","stopReason":"end_turn"}`, want: "ok"},
		{name: "empty grok text is an error, not an empty answer", brain: grok, out: `{"text":"","stopReason":"refusal"}`, wantErr: "no text"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin := fakeCLI(t, tt.out)
			t.Setenv("JUNE_FAKE_CLI_ERR", tt.errOut)
			t.Setenv("JUNE_FAKE_CLI_EXIT", tt.exit)
			t.Setenv("JUNE_FAKE_CLI_SLEEP", tt.sleep)
			stdin := filepath.Join(t.TempDir(), "stdin")
			t.Setenv("JUNE_FAKE_CLI_STDIN_TO", stdin)
			timeout := tt.timeout
			if timeout == 0 {
				timeout = 10
			}
			got, err := tt.brain(bin, timeout)(context.Background(), "summarise this")

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected an error containing %q, got %q", tt.wantErr, got)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				if tt.wantNotErr != "" && strings.Contains(err.Error(), tt.wantNotErr) {
					t.Fatalf("error = %v, want it not to carry the CLI's own output %q", err, tt.wantNotErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
			if in, _ := os.ReadFile(stdin); tt.wantStdin != "" && string(in) != tt.wantStdin {
				t.Errorf("stdin = %q, want %q", in, tt.wantStdin)
			}
		})
	}
}

// FromConfig is the only thing the daemon calls: an absent brain block keeps June on the Gemini API, a configured CLI runs its binary, and a provider this package has no backend for fails rather than quietly answering on Gemini.
func TestFromConfig(t *testing.T) {
	tests := []struct {
		name    string
		cfg     config.BrainConfig
		asker   CodexAsker
		want    string
		wantErr error
		errText string
	}{
		// An empty API key keeps the Gemini case off the network: the key is checked before the client is built.
		{name: "the zero config is the Gemini API", cfg: config.BrainConfig{}, errText: "GEMINI_API_KEY"},
		{name: "claude-cli runs the configured binary", cfg: config.BrainConfig{Provider: config.BrainClaudeCLI, TimeoutSeconds: 10}, want: "from claude"},
		{name: "codex answers through the asker it is given", cfg: config.BrainConfig{Provider: config.BrainCodex}, asker: fakeAsker{trace: agent.TurnTrace{Answer: "from codex"}}, want: "from codex"},
		{name: "an asker's empty answer is an error, not an empty success", cfg: config.BrainConfig{Provider: config.BrainCodex}, asker: fakeAsker{}, errText: "no text"},
		{name: "an asker's failure passes through", cfg: config.BrainConfig{Provider: config.BrainCodex}, asker: fakeAsker{err: errors.New("codex login: run codex login again")}, errText: "codex login"},
		{name: "codex with no asker is an error, not a quiet Gemini call", cfg: config.BrainConfig{Provider: config.BrainCodex, Model: "gpt-5.5"}, wantErr: ErrNoBackend},
		{name: "ollama has no backend yet and says so", cfg: config.BrainConfig{Provider: config.BrainOllama, Model: "llama3.1:8b"}, wantErr: ErrNoBackend},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.cfg
			if cfg.Provider == config.BrainClaudeCLI {
				cfg.Binary = fakeCLI(t, `{"is_error":false,"result":"from claude"}`)
			}
			var askers []CodexAsker
			if tt.asker != nil {
				askers = append(askers, tt.asker)
			}
			// A real-looking API key on the no-backend rows: the point is that the call fails even when the Gemini path would have worked.
			key := ""
			if tt.wantErr != nil {
				key = "a-real-looking-key"
			}
			got, err := FromConfig(cfg, key, askers...)(context.Background(), "hi")
			switch {
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("error = %v, want %v", err, tt.wantErr)
				}
			case tt.errText != "":
				if err == nil || !strings.Contains(err.Error(), tt.errText) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.errText)
				}
			case err != nil:
				t.Fatalf("unexpected error: %v", err)
			case got != tt.want:
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestGeminiAPI_TimesOutOnAServerThatNeverAnswers checks that the configured timeout is a real deadline on the Gemini call and not a field only the CLI providers read. A server that accepts the connection and then never replies must fail the call within the configured seconds; before this the genai client had no timeout of its own and the daemon's root context never ends, so one dead connection blocked the proactive scheduler's single goroutine for the life of the process.
// The stub is reached through GOOGLE_GEMINI_BASE_URL, which is the only seam the genai package offers for pointing a client somewhere else.
func TestGeminiAPI_TimesOutOnAServerThatNeverAnswers(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer func() { close(release); srv.Close() }()
	t.Setenv("GOOGLE_GEMINI_BASE_URL", srv.URL)

	done := make(chan error, 1)
	go func() {
		_, err := GeminiAPI("test-key", config.TextModel, 1)(context.Background(), "hi")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a server that never answers returned no error")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the call never came back, so the configured timeout is not reaching the Gemini call")
	}
}

// OutermostJSON recovers the JSON answer from a model reply that says something before or around it; the dream and study packages read every model reply through it.
func TestOutermostJSON(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"object after prose", `Sure! {"a":1}`, `{"a":1}`},
		// A model that mentions a bracket before it answers used to defeat the recovery: taking the first bracket and the last matching closer returned a slice that spanned the prose and ended inside the real answer.
		{"a bracket in the prose before the answer", `Based on the notes [see above], here is the result: {"items": ["a"]}`, `{"items": ["a"]}`},
		{"no json at all", "I could not answer that", ""},
		{"opener with no closer", `{"a":1`, ""},
	} {
		if got := OutermostJSON(tc.in); got != tc.want {
			t.Errorf("%s: OutermostJSON(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// A duty whose first provider is down answers on the next one. Every duty had WithCodexFallback, which hands on for a spent Gemini allowance and hands on only to Codex; with Codex itself at its monthly limit it caught nothing, and on 2026-09-15 every meeting-minutes run died on a broken Antigravity login while Grok and Claude sat signed in and unasked.
func TestRouted_HandsOnFromAProviderThatCannotAnswer(t *testing.T) {
	agent.ResetRouter()
	t.Cleanup(agent.ResetRouter)
	for _, id := range []string{agent.ProviderAgy, agent.ProviderClaude} {
		agent.SetProviderReady(id, true)
	}
	for _, id := range []string{agent.ProviderGemini, agent.ProviderCodex, agent.ProviderGrok} {
		agent.SetProviderReady(id, false)
	}

	asked := []string{}
	routed := Routed(func(provider string) Brain {
		return func(context.Context, string) (string, error) {
			asked = append(asked, provider)
			if provider == config.BrainAgyCLI {
				return "", agent.ErrLoggedOut
			}
			return "the minutes", nil
		}
	})

	got, err := routed(context.Background(), "summarise this meeting")
	if err != nil {
		t.Fatalf("every provider refused a duty one of them could answer: %v", err)
	}
	if got != "the minutes" {
		t.Errorf("answer = %q, want the one the second provider gave", got)
	}
	if len(asked) != 2 || asked[0] != config.BrainAgyCLI || asked[1] != config.BrainClaudeCLI {
		t.Errorf("providers asked = %v, want agy then claude", asked)
	}
}

// A duty the config names a provider for asks that one first. background_brains exists so a duty can be pinned to the user's own Claude or Antigravity plan, and the daemon treats naming one as the more deliberate act; routing by the global preference instead would answer that duty on a provider the user did not pick, which is the thing ErrNoBackend was introduced to stop. The rest of the router's order still stands behind it, so a pinned provider that is spent or signed out still hands on rather than failing the duty.
func TestRouted_AsksTheConfiguredProviderFirst(t *testing.T) {
	agent.ResetRouter()
	t.Cleanup(agent.ResetRouter)
	for _, id := range []string{agent.ProviderGemini, agent.ProviderCodex, agent.ProviderAgy, agent.ProviderClaude, agent.ProviderGrok} {
		agent.SetProviderReady(id, true)
	}
	// The global preference is Gemini, and it can answer; the duty's own pin must still win.
	agent.SetPreferredProvider(config.BrainGeminiAPI)

	asked := []string{}
	routed := RoutedFor(config.BrainClaudeCLI, func(provider string) Brain {
		return func(context.Context, string) (string, error) {
			asked = append(asked, provider)
			return "the minutes", nil
		}
	})
	if _, err := routed(context.Background(), "summarise this meeting"); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 || asked[0] != config.BrainClaudeCLI {
		t.Errorf("providers asked = %v, want the pinned claude-cli alone", asked)
	}
}

// A duty that reaches a provider this caller cannot build hands on rather than stopping. The meeting recorder builds its own route with FromConfig and no Codex asker, so Codex answers every call there with ErrNoBackend; ProviderSpent said no to that, and every write-up stopped at Codex with Claude signed in behind it.
func TestRouted_HandsOnFromACodexThisCallerCannotBuild(t *testing.T) {
	agent.ResetRouter()
	t.Cleanup(agent.ResetRouter)
	for _, id := range []string{agent.ProviderCodex, agent.ProviderClaude} {
		agent.SetProviderReady(id, true)
	}
	for _, id := range []string{agent.ProviderGemini, agent.ProviderAgy, agent.ProviderGrok} {
		agent.SetProviderReady(id, false)
	}
	routed := Routed(func(provider string) Brain {
		if provider == config.BrainClaudeCLI {
			return func(context.Context, string) (string, error) { return "the minutes", nil }
		}
		return FromConfig(config.BrainConfig{Provider: provider}, "")
	})
	got, err := routed(context.Background(), "summarise this meeting")
	if err != nil || got != "the minutes" {
		t.Fatalf("got (%q, %v), want claude's answer after codex had no backend here", got, err)
	}
	// Codex was not refused by its provider, so asks must still be offered it.
	if !slices.Contains(agent.Route(agent.Need{}), agent.ProviderCodex) {
		t.Error("a caller with no Codex asker opened Codex's breaker for every ask")
	}
}

// A duty's own daily band running out is not Gemini refusing. The background band is Limit minus Reserved, so the share kept for asks is still there; opening the shared breaker on this refusal sent every interactive ask past Gemini for an hour.
func TestRouted_ADutysOwnQuotaRefusalHandsOnWithoutOpeningTheBreaker(t *testing.T) {
	agent.ResetRouter()
	t.Cleanup(agent.ResetRouter)
	for _, id := range []string{agent.ProviderGemini, agent.ProviderClaude} {
		agent.SetProviderReady(id, true)
	}
	for _, id := range []string{agent.ProviderCodex, agent.ProviderAgy, agent.ProviderGrok} {
		agent.SetProviderReady(id, false)
	}

	routed := Routed(func(provider string) Brain {
		return func(context.Context, string) (string, error) {
			if provider == config.BrainGeminiAPI {
				return "", &ErrDailyQuota{Model: "gemini-3.5-flash", Limit: 12}
			}
			return "the minutes", nil
		}
	})
	if got, err := routed(context.Background(), "summarise this meeting"); err != nil || got != "the minutes" {
		t.Fatalf("got (%q, %v), want claude's answer after Gemini's background band ran out", got, err)
	}
	if !slices.Contains(agent.Route(agent.Need{}), agent.ProviderGemini) {
		t.Error("the duty's own band refusal opened Gemini's breaker, so asks skip Gemini although their reserved share is untouched")
	}
}
