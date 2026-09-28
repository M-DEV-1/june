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

// A clean machine has none of the pieces Ora runs locally, and doctor used to name only the Silero model, so the first sign of a missing whisper-cli was a meeting that never turned into minutes. Every piece is reported missing with the exact path it was looked for at and the feature that is off without it, and reported present once it is there.
func TestLocalPieceChecks_NamesEveryMissingPieceAndWhereItGoes(t *testing.T) {
	dataDir, runtimeDir, pathDir := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("ORA_WHISPER_CPP", "")
	t.Setenv("ORA_SHERPA", "")
	t.Setenv("PULSE_SERVER", "")
	t.Setenv("PATH", pathDir)
	whisper := filepath.Join(dataDir, "whispercpp")
	sherpa := filepath.Join(dataDir, "sherpa")
	want := map[string][]string{
		"meeting transcription": {filepath.Join(whisper, "whisper-cli"), filepath.Join(whisper, "ggml-medium.bin")},
		"voice activity model":  {filepath.Join(whisper, "ggml-silero-v6.2.0.bin")},
		"speaker diarization":   {filepath.Join(sherpa, "sherpa-onnx-offline-speaker-diarization"), filepath.Join(sherpa, "segmentation-3.0.onnx"), filepath.Join(sherpa, "wespeaker_en_voxceleb_CAM++.onnx")},
		"local memory search":   {config.ConfigPath()},
		"audio server":          {filepath.Join(runtimeDir, "pulse", "native")},
		"call detection":        {"pw-dump", pathDir},
	}

	checks := localPieceChecks(dataDir, runtimeDir, config.EmbedConfig{})
	if len(checks) != len(want) {
		t.Fatalf("got %d checks, want one for each of %d pieces: %+v", len(checks), len(want), checks)
	}
	for _, c := range checks {
		paths, ok := want[c.Name]
		if !ok {
			t.Errorf("unexpected check %q", c.Name)
			continue
		}
		if c.OK || c.Fix == "" {
			t.Errorf("%s = %+v, want a failure with a fix on an empty data dir", c.Name, c)
		}
		if !strings.Contains(c.Detail, " off") {
			t.Errorf("%s detail %q does not say which feature is off", c.Name, c.Detail)
		}
		for _, p := range paths {
			if !strings.Contains(c.Detail, p) {
				t.Errorf("%s detail %q does not name %s", c.Name, c.Detail, p)
			}
		}
	}

	put := func(path string, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), mode); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range append(want["meeting transcription"], want["voice activity model"][0], want["speaker diarization"][0], want["speaker diarization"][1], want["speaker diarization"][2], want["audio server"][0]) {
		put(p, 0o755)
	}
	put(filepath.Join(pathDir, "pw-dump"), 0o755)
	server, model := filepath.Join(dataDir, "llama.cpp", "llama-server"), filepath.Join(dataDir, "models", "embeddinggemma.gguf")
	put(server, 0o755)
	put(model, 0o644)

	for _, c := range localPieceChecks(dataDir, runtimeDir, config.EmbedConfig{LlamaServer: server, ModelPath: model}) {
		if !c.OK {
			t.Errorf("%s = %+v, want OK once its pieces are installed", c.Name, c)
		}
	}
}

// A saved restore_token from a one-monitor grant keeps restoring that same one-monitor grant forever, and a click on the second monitor of a multi-monitor desk fails with "coordinate (x, y) is outside the WxH monitor" — it happened twice in one run log. internal/input/portal.go already knows this and warns to stderr, where nobody sees it; doctor says it in the report and names the exact file to delete.
func TestPointerCheck_NamesTheTokenFileAndTheDesksMonitorCount(t *testing.T) {
	dataDir := t.TempDir()
	tokenPath := filepath.Join(dataDir, "portal-input-token")

	if got := pointerCheck(tokenPath, 2); got.Fix == "" {
		t.Errorf("pointerCheck = %+v, want a fix when no consent is saved yet", got)
	} else if !strings.Contains(got.Detail, "no saved consent") {
		t.Errorf("Detail = %q, want it to say no consent is saved", got.Detail)
	}

	if err := os.WriteFile(tokenPath, []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := pointerCheck(tokenPath, 2)
	if !got.OK {
		t.Errorf("pointerCheck = %+v, want OK once consent is saved", got)
	}
	if !strings.Contains(got.Detail, "2 monitor") {
		t.Errorf("Detail = %q, want the desk's monitor count", got.Detail)
	}
	if !strings.Contains(got.Detail, tokenPath) {
		t.Errorf("Detail = %q, want the exact token path to delete for a re-grant", got.Detail)
	}

	if got := pointerCheck(tokenPath, -1); !strings.Contains(got.Detail, "could not be read") {
		t.Errorf("Detail = %q, want it to say the monitor count could not be read", got.Detail)
	}
}
