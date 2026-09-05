package brain

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"ora/internal/config"
)

// fakeCLI writes a stub executable that records how it was called and then runs body, so the CLI brain can be tested without running the real claude.
// Input: the file name to give the stub and the shell lines that produce its output. Output: the path to the executable; its argv lands in <dir>/args, one per line, and its stdin in <dir>/stdin.
func fakeCLI(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	script := "#!/bin/sh\nd=$(dirname \"$0\")\nprintf '%s\\n' \"$@\" > \"$d/args\"\ncat > \"$d/stdin\"\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
	return path
}

// recorded returns what the stub at path saw: its argv and its stdin.
func recorded(t *testing.T, path string) (args, stdin string) {
	t.Helper()
	a, err := os.ReadFile(filepath.Join(filepath.Dir(path), "args"))
	if err != nil {
		t.Fatalf("the stub recorded no argv: %v", err)
	}
	s, err := os.ReadFile(filepath.Join(filepath.Dir(path), "stdin"))
	if err != nil {
		t.Fatalf("the stub recorded no stdin: %v", err)
	}
	return string(a), string(s)
}

func TestClaudeCLI(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		timeout int
		want    string
		wantErr string
	}{
		{
			name: "the result field is the answer",
			body: `printf '%s' '{"is_error":false,"subtype":"success","total_cost_usd":0.05,"result":"# Meeting minutes\nall good\n"}'`,
			want: "# Meeting minutes\nall good",
		},
		{
			name:    "an is_error run fails with what the CLI said",
			body:    `printf '%s' '{"is_error":true,"subtype":"error_during_execution","result":"Credit balance is too low"}'`,
			wantErr: "Credit balance is too low",
		},
		{
			name:    "an empty result is not an answer",
			body:    `printf '%s' '{"is_error":false,"result":""}'`,
			wantErr: "no text",
		},
		{
			name:    "output that is not JSON fails loudly",
			body:    `printf '%s' 'Invalid API key · Please run /login'`,
			wantErr: "parse",
		},
		{
			name:    "a non-zero exit carries the exit code and stderr",
			body:    `echo "not logged in" >&2; exit 1`,
			wantErr: "not logged in",
		},
		{
			name:    "a run that outlives the timeout is killed",
			body:    `sleep 5`,
			timeout: 1,
			wantErr: "timed out",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin := fakeCLI(t, "claude", tt.body)
			timeout := tt.timeout
			if timeout == 0 {
				timeout = 10
			}
			got, err := ClaudeCLI(bin, "", timeout)(context.Background(), "summarise this")

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected an error containing %q, got %q", tt.wantErr, got)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// The prompt goes in on stdin, because a meeting transcript is far longer than a single argv entry may be. The flags are the subscription path: -p with JSON output, no --bare (which would switch billing to an API key), and no tools, because the prompt carries text the user never wrote.
func TestClaudeCLI_invocation(t *testing.T) {
	bin := fakeCLI(t, "claude", `printf '%s' '{"is_error":false,"result":"ok"}'`)
	if _, err := ClaudeCLI(bin, "", 10)(context.Background(), "the whole transcript"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	args, stdin := recorded(t, bin)
	for _, want := range []string{"-p", "--output-format", "json", "--restricted"} {
		if !strings.Contains(args, want+"\n") {
			t.Errorf("argv is missing %q:\n%s", want, args)
		}
	}
	if strings.Contains(args, "--bare") {
		t.Errorf("--bare switches billing to an API key and must never be passed:\n%s", args)
	}
	if stdin != "the whole transcript" {
		t.Errorf("stdin = %q, want the prompt", stdin)
	}
}

// A headless hang is cut off by the timeout.
func TestClaudeCLI_timeout(t *testing.T) {
	bin := fakeCLI(t, "claude", `sleep 5`)
	_, err := ClaudeCLI(bin, "", 1)(context.Background(), "summarise this")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error = %v, want it to contain %q", err, "timed out")
	}
}

func TestClaudeCLI_missingBinary(t *testing.T) {
	_, err := ClaudeCLI(filepath.Join(t.TempDir(), "not-installed"), "", 10)(context.Background(), "hi")
	if err == nil {
		t.Fatal("a missing binary should be an error, not an empty answer")
	}
}

// FromConfig is the only thing the daemon calls: an absent or unrecognised brain block must keep ORA on the Gemini API exactly as it was before this package existed.
func TestFromConfig(t *testing.T) {
	claudeBin := fakeCLI(t, "claude", `printf '%s' '{"is_error":false,"result":"from claude"}'`)

	tests := []struct {
		name    string
		cfg     config.BrainConfig
		want    string
		wantErr string
	}{
		{
			name:    "the zero config is the Gemini API",
			cfg:     config.BrainConfig{},
			wantErr: "GEMINI_API_KEY",
		},
		{
			name:    "an unknown provider falls back to the Gemini API",
			cfg:     config.BrainConfig{Provider: "chatgpt"},
			wantErr: "GEMINI_API_KEY",
		},
		{
			name: "claude-cli runs the configured binary",
			cfg:  config.BrainConfig{Provider: config.BrainClaudeCLI, Binary: claudeBin, TimeoutSeconds: 10},
			want: "from claude",
		},
		{
			name:    "codex with no asker falls back to the Gemini API",
			cfg:     config.BrainConfig{Provider: config.BrainCodex},
			wantErr: "GEMINI_API_KEY",
		},
		{
			name:    "ollama has no backend yet and falls back to the Gemini API",
			cfg:     config.BrainConfig{Provider: config.BrainOllama},
			wantErr: "GEMINI_API_KEY",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// An empty API key keeps the Gemini cases off the network: the key is checked before the client is built.
			got, err := FromConfig(tt.cfg, "")(context.Background(), "hi")
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// A configured model rides through to --model, so writing duties can run on a cheaper tier than the login's default; empty keeps the default.
func TestClaudeCLI_ModelFlag(t *testing.T) {
	bin := fakeCLI(t, "claude", `printf '%s' '{"is_error":false,"result":"ok"}'`)
	if _, err := ClaudeCLI(bin, "sonnet", 10)(context.Background(), "hi"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	args, _ := recorded(t, bin)
	if !strings.Contains(args, "--model\nsonnet\n") {
		t.Errorf("argv is missing --model sonnet:\n%s", args)
	}
}

// grok answers on its "text" field; every tool is denied since the prompt carries unvetted text.
func TestGrokCLI(t *testing.T) {
	bin := fakeCLI(t, "grok", `printf '%s' '{"text":"ok\n","stopReason":"end_turn"}'`)
	got, err := GrokCLI(bin, 10)(context.Background(), "summarise this")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "ok" {
		t.Fatalf("got %q, want ok", got)
	}
	args, _ := recorded(t, bin)
	for _, want := range []string{"--output-format\njson\n", "--deny\n*\n"} {
		if !strings.Contains(args, want) {
			t.Errorf("argv is missing %q:\n%s", want, args)
		}
	}
	if _, err := GrokCLI(fakeCLI(t, "grok", `printf '%s' '{"text":"","stopReason":"refusal"}'`), 10)(context.Background(), "x"); err == nil {
		t.Error("empty text must be an error, not an empty answer")
	}
}

// agy answers on its "response" field and only a SUCCESS status counts.
func TestAgyCLI_Restored(t *testing.T) {
	bin := fakeCLI(t, "agy", `printf '%s' '{"status":"SUCCESS","response":"ok"}'`)
	if got, err := AgyCLI(bin, 10)(context.Background(), "hi"); err != nil || got != "ok" {
		t.Fatalf("got %q err %v, want ok", got, err)
	}
	if _, err := AgyCLI(fakeCLI(t, "agy", `printf '%s' '{"status":"ERROR","response":""}'`), 10)(context.Background(), "x"); err == nil {
		t.Error("a non-SUCCESS status must be an error")
	}
}

// The gold-set eval pins each CLI arm to a named model so two runs compare the same thing. An empty model has to leave the flag off entirely, which is what keeps the CLI's own default.
func TestCLIArgs_ModelFlag(t *testing.T) {
	if got := grokArgs("hello", ""); contains(got, "-m") {
		t.Errorf("empty model must not pass -m: %v", got)
	}
	got := grokArgs("hello", "grok-4")
	if !contains(got, "-m") || got[indexOf(got, "-m")+1] != "grok-4" {
		t.Errorf("grokArgs = %v, want -m grok-4", got)
	}
	if got := agyArgs("hello", ""); contains(got, "--model") {
		t.Errorf("empty model must not pass --model: %v", got)
	}
	got = agyArgs("hello", "gemini-3-pro")
	if !contains(got, "--model") || got[indexOf(got, "--model")+1] != "gemini-3-pro" {
		t.Errorf("agyArgs = %v, want --model gemini-3-pro", got)
	}
}

func contains(ss []string, s string) bool { return indexOf(ss, s) >= 0 }

func indexOf(ss []string, s string) int {
	for i, v := range ss {
		if v == s {
			return i
		}
	}
	return -1
}

// TestHead_CutsOnRunesSoALogLineIsAlwaysValidUTF8 pins that the CLI-stderr excerpt cuts on rune boundaries. It used to slice s[:300] by byte, which lands in the middle of a multi-byte character whenever a CLI's error output carries one and writes a broken half-character into the log.
func TestHead_CutsOnRunesSoALogLineIsAlwaysValidUTF8(t *testing.T) {
	long := strings.Repeat("é", 400) // two bytes each, so a 300-byte cut lands mid-character
	got := head(long)
	if !utf8.ValidString(got) {
		t.Errorf("head returned invalid UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("head(%d runes) = %q, want it marked as cut", utf8.RuneCountInString(long), got)
	}
	if n := utf8.RuneCountInString(got); n != 301 {
		t.Errorf("head returned %d runes, want 300 plus the ellipsis", n)
	}
	if got := head("  a\n\tb  "); got != "a b" {
		t.Errorf("head(%q) = %q, want %q", "  a\n\tb  ", got, "a b")
	}
	if got := head(""); got != "" {
		t.Errorf("head(\"\") = %q, want \"\"", got)
	}
}

// TestGeminiModel_DropsAnotherProvidersModelName pins the fix for the 404 loop of 2026-09-05: the config named provider "codex-direct" with model "gpt-5.5", the meeting summariser built its brain with no asker, and FromConfig's fallback to the Gemini API carried "gpt-5.5" through as the model — so every hourly retry of the 00-53-59 recording failed with "models/gpt-5.5 is not found for API version v1beta". A model name only means anything to the provider it was written for, so the fallback uses the Gemini default instead.
func TestGeminiModel_DropsAnotherProvidersModelName(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.BrainConfig
		want string
	}{
		{"codex model name", config.BrainConfig{Provider: config.BrainCodex, Model: "gpt-5.5"}, config.TextModel},
		{"claude cli alias", config.BrainConfig{Provider: config.BrainClaudeCLI, Model: "sonnet"}, config.TextModel},
		{"ollama model name", config.BrainConfig{Provider: config.BrainOllama, Model: "llama3.1:8b"}, config.TextModel},
		{"unknown provider", config.BrainConfig{Provider: "made-up", Model: "gpt-5.5"}, config.TextModel},
		{"a gemini config keeps its own model", config.BrainConfig{Provider: config.BrainGeminiAPI, Model: "gemini-3.5-flash-lite"}, "gemini-3.5-flash-lite"},
		{"no provider keeps its own model", config.BrainConfig{Model: "gemini-3.5-flash-lite"}, "gemini-3.5-flash-lite"},
		{"no provider and no model is the text model", config.BrainConfig{}, config.TextModel},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := geminiModel(c.cfg); got != c.want {
				t.Errorf("geminiModel(%+v) = %q, want %q", c.cfg, got, c.want)
			}
		})
	}
}
