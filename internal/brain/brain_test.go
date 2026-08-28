package brain

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
			got, err := ClaudeCLI(bin, timeout)(context.Background(), "summarise this")

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
	if _, err := ClaudeCLI(bin, 10)(context.Background(), "the whole transcript"); err != nil {
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
	_, err := ClaudeCLI(bin, 1)(context.Background(), "summarise this")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error = %v, want it to contain %q", err, "timed out")
	}
}

func TestClaudeCLI_missingBinary(t *testing.T) {
	_, err := ClaudeCLI(filepath.Join(t.TempDir(), "not-installed"), 10)(context.Background(), "hi")
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
