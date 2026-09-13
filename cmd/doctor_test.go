package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ora/internal/config"
)

// The report ends with the first blocker's own fix, so a desk that cannot place clicks says "log out and in" rather than leaving the reader to work it out from five lines.
func TestDoctorReport_NamesTheFirstBlockerAndItsFix(t *testing.T) {
	got := doctorReport([]doctorCheck{
		{Name: "accessibility bus", Detail: "reachable", OK: true},
		{Name: "window frames", Detail: "3 windows listed, none with a frame", Fix: "log out and in"},
		{Name: "daemon", Detail: "not answering", Fix: "run ora"},
	})
	for _, want := range []string{"ok    accessibility bus", "FAIL  window frames", "not ready: window frames, daemon", "next: log out and in"} {
		if !strings.Contains(got, want) {
			t.Errorf("report lacks %q:\n%s", want, got)
		}
	}
	if !strings.Contains(doctorReport([]doctorCheck{{Name: "daemon", OK: true}}), "ready: everything") {
		t.Error("a report with no blockers must say it is ready")
	}
}

// A clean install reported "ready" and then could not answer a single question, because every check was about the desk and none about whether Ora had anything to think with. The check names the brain it found, and a desk with none says how to give it one.
func TestBrainCheck_SaysWhetherThereIsAnythingToThinkWith(t *testing.T) {
	withLogin := func(t *testing.T, rel string) string {
		t.Helper()
		home := t.TempDir()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(home, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, rel), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		return home
	}

	for _, c := range []struct {
		name   string
		home   string
		apiKey string
		wantOK bool
		want   string
	}{
		{name: "nothing at all", home: t.TempDir(), wantOK: false, want: "no brain"},
		{name: "a gemini key", home: t.TempDir(), apiKey: "AIzaNotReal", wantOK: true, want: "Gemini"},
		{name: "claude is logged in", home: withLogin(t, ".claude/.credentials.json"), wantOK: true, want: "Claude"},
		{name: "codex is logged in", home: withLogin(t, ".codex/auth.json"), wantOK: true, want: "ChatGPT"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := brainCheck(c.home, c.apiKey)
			if got.OK != c.wantOK {
				t.Errorf("OK = %v, want %v (detail %q)", got.OK, c.wantOK, got.Detail)
			}
			if !strings.Contains(got.Detail, c.want) {
				t.Errorf("detail = %q, want it to mention %q", got.Detail, c.want)
			}
			if !c.wantOK && got.Fix == "" {
				t.Error("a desk with no brain must say how to give it one")
			}
		})
	}
}

// ora doctor read the environment as the shell handed it over and nothing else, so a key sitting in the env file the first-run panel tells the user to write — the file the daemon itself reads — was invisible, and doctor told a user who had a brain that they had none.
func TestLoadEnvFiles_ReadsTheKeyTheDaemonReads(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("GEMINI_API_KEY", "restored-by-cleanup")
	os.Unsetenv("GEMINI_API_KEY") // godotenv never overwrites a variable that is already set, and an empty one counts as set

	dir := config.DataDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "env"), []byte("GEMINI_API_KEY=from-the-env-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	loadEnvFiles()

	if got := os.Getenv("GEMINI_API_KEY"); got != "from-the-env-file" {
		t.Fatalf("GEMINI_API_KEY = %q, want it read from %s", got, filepath.Join(dir, "env"))
	}
	if got := brainCheck(home, os.Getenv("GEMINI_API_KEY")); !got.OK || !strings.Contains(got.Detail, "Gemini") {
		t.Errorf("brain check = %+v, want it to find the Gemini key", got)
	}
}
