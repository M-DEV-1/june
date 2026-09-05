package config

import (
	"os"
	"path/filepath"
	"testing"
)

// DataDir resolves in a fixed order: the ORA_DATA_DIR override verbatim (what the tests and any scripted/portable install use), then $XDG_DATA_HOME/ora, then ~/.local/share/ora.
func TestDataDir_ResolutionOrder(t *testing.T) {
	t.Run("ORA_DATA_DIR override wins", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("ORA_DATA_DIR", dir)
		if got := DataDir(); got != dir {
			t.Errorf("DataDir() = %q, want override %q", got, dir)
		}
	})

	t.Run("XDG_DATA_HOME", func(t *testing.T) {
		t.Setenv("ORA_DATA_DIR", "")
		xdg := t.TempDir()
		t.Setenv("XDG_DATA_HOME", xdg)
		if got, want := DataDir(), filepath.Join(xdg, "ora"); got != want {
			t.Errorf("DataDir() = %q, want %q", got, want)
		}
	})

	t.Run("home fallback", func(t *testing.T) {
		t.Setenv("ORA_DATA_DIR", "")
		t.Setenv("XDG_DATA_HOME", "")
		home := t.TempDir()
		t.Setenv("HOME", home)
		if got, want := DataDir(), filepath.Join(home, ".local", "share", "ora"); got != want {
			t.Errorf("DataDir() = %q, want %q", got, want)
		}
	})
}

// TestDataDir_MigratesLegacyOraDb verifies the one-time migration: when the new XDG-based directory doesn't exist yet but a legacy "ora-db" directory exists in the current working directory (the old cwd-relative convention), DataDir moves its contents into the new directory instead of leaving them orphaned.
func TestDataDir_MigratesLegacyOraDb(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)

	legacy := filepath.Join(cwd, "ora-db")
	if err := os.MkdirAll(legacy, 0755); err != nil {
		t.Fatalf("MkdirAll legacy dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "db"), []byte("legacy-data"), 0644); err != nil {
		t.Fatalf("WriteFile legacy db: %v", err)
	}

	t.Setenv("ORA_DATA_DIR", "")
	xdg := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdg)

	got := DataDir()
	want := filepath.Join(xdg, "ora")
	if got != want {
		t.Fatalf("DataDir() = %q, want %q", got, want)
	}

	data, err := os.ReadFile(filepath.Join(want, "db"))
	if err != nil {
		t.Fatalf("expected legacy db file to be migrated into the new data dir: %v", err)
	}
	if string(data) != "legacy-data" {
		t.Errorf("migrated db file content = %q, want %q", string(data), "legacy-data")
	}

	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("expected the legacy ora-db directory to be gone after migration, stat err = %v", err)
	}
}

// TestConfigPath_UnderDataDir verifies ConfigPath resolves inside DataDir rather than the old cwd-relative "ora-db".
func TestConfigPath_UnderDataDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ORA_DATA_DIR", dir)

	want := filepath.Join(dir, "ora-config.json")
	if got := ConfigPath(); got != want {
		t.Errorf("ConfigPath() = %q, want %q", got, want)
	}
}

// TestDataDir_MigratesWhenTargetExistsButHasNoDatabase verifies the migration still runs when the target directory already exists but holds no database.
// This is the ordinary case on a real upgrade, not an edge case: InitTelemetry does MkdirAll(DataDir()) for the log file and SaveConfig does the same for the config, so by the time anything looks for a legacy directory the target usually exists already. Keying the decision on "does the directory exist" instead of "does it hold a database" silently skips the move and strands every existing memory in the old location.
func TestDataDir_MigratesWhenTargetExistsButHasNoDatabase(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)

	legacy := filepath.Join(cwd, "ora-db")
	if err := os.MkdirAll(legacy, 0755); err != nil {
		t.Fatalf("MkdirAll legacy dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "db"), []byte("legacy-data"), 0644); err != nil {
		t.Fatalf("WriteFile legacy db: %v", err)
	}

	t.Setenv("ORA_DATA_DIR", "")
	xdg := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdg)

	// Something got there first and created the target for its log file, exactly as InitTelemetry does.
	target := filepath.Join(xdg, "ora")
	if err := os.MkdirAll(target, 0755); err != nil {
		t.Fatalf("MkdirAll target: %v", err)
	}
	if err := os.WriteFile(filepath.Join(target, "ora.log"), []byte("some log line"), 0644); err != nil {
		t.Fatalf("WriteFile target log: %v", err)
	}

	DataDir()

	data, err := os.ReadFile(filepath.Join(target, "db"))
	if err != nil {
		t.Fatalf("expected the legacy db to be migrated even though the target dir already existed: %v", err)
	}
	if string(data) != "legacy-data" {
		t.Errorf("migrated db content = %q, want %q", string(data), "legacy-data")
	}
	if _, err := os.Stat(filepath.Join(target, "ora.log")); err != nil {
		t.Errorf("migration destroyed the pre-existing log file in the target: %v", err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("expected the legacy directory to be gone after migration, stat err = %v", err)
	}
}

// TestEmbedConfigDefaults verifies a config file with no "embed" key at all (every install before the local embedder existed) still comes back with the local port and idle timeout filled in, so the daemon never spawns llama-server on port 0 or reaps it instantly.
func TestEmbedConfigDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ORA_DATA_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "ora-config.json"), []byte(`{"voice":"Iapetus"}`), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := LoadConfig()
	if cfg.Embed.Port != DefaultEmbedPort {
		t.Errorf("Embed.Port = %d, want %d", cfg.Embed.Port, DefaultEmbedPort)
	}
	if cfg.Embed.IdleTimeout != DefaultEmbedIdleTimeout {
		t.Errorf("Embed.IdleTimeout = %v, want %v", cfg.Embed.IdleTimeout, DefaultEmbedIdleTimeout)
	}
	if cfg.Embed.LocalEnabled() {
		t.Error("Embed.LocalEnabled() should be false with no llama_server/model_path configured")
	}
}

// TestEmbedConfigLocalEnabled verifies that setting both the binary and the model path is what switches the daemon onto the local embedder, and that an explicit port and idle timeout survive the load.
func TestEmbedConfigLocalEnabled(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ORA_DATA_DIR", dir)
	body := `{"embed":{"llama_server":"/opt/llama-server","model_path":"/opt/gemma.gguf","port":6943,"idle_timeout_ms":60000}}`
	if err := os.WriteFile(filepath.Join(dir, "ora-config.json"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := LoadConfig()
	if !cfg.Embed.LocalEnabled() {
		t.Fatal("Embed.LocalEnabled() = false, want true with both llama_server and model_path set")
	}
	if cfg.Embed.Port != 6943 {
		t.Errorf("Embed.Port = %d, want 6943", cfg.Embed.Port)
	}
	if cfg.Embed.IdleTimeout != 60000 {
		t.Errorf("Embed.IdleTimeout = %v, want 60000", cfg.Embed.IdleTimeout)
	}
	if got, want := cfg.Embed.BaseURL(), "http://127.0.0.1:6943"; got != want {
		t.Errorf("Embed.BaseURL() = %q, want %q", got, want)
	}
}

// TestEmbedConfigHalfConfiguredStaysOnGemini verifies that a binary with no model (or the reverse) does not switch the daemon over — a half-written config must not take embeddings down.
func TestEmbedConfigHalfConfiguredStaysOnGemini(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ORA_DATA_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "ora-config.json"), []byte(`{"embed":{"llama_server":"/opt/llama-server"}}`), 0644); err != nil {
		t.Fatal(err)
	}

	if LoadConfig().Embed.LocalEnabled() {
		t.Error("Embed.LocalEnabled() = true with no model_path, want false")
	}
}

// TestEmbedConfigSimilarityFloorDefault verifies the local embedder gets its own cosine floor when the config does not name one, and that an explicit value in the file wins.
func TestEmbedConfigSimilarityFloorDefault(t *testing.T) {
	if got := (EmbedConfig{}).Floor(); got != DefaultLocalSimilarityFloor {
		t.Errorf("Floor() = %v with nothing configured, want %v", got, DefaultLocalSimilarityFloor)
	}
	if got := (EmbedConfig{SimilarityFloor: 0.5}).Floor(); got != 0.5 {
		t.Errorf("Floor() = %v with an explicit 0.5, want 0.5", got)
	}
}

// A config file with no brain block leaves the zero value in place, which brain.FromConfig reads as the Gemini API — the behaviour ORA had before the block existed. A file that does carry one is read verbatim.
func TestLoadConfig_Brain(t *testing.T) {
	t.Run("absent block is the Gemini default", func(t *testing.T) {
		t.Setenv("ORA_DATA_DIR", t.TempDir())
		if err := os.WriteFile(ConfigPath(), []byte(`{"voice":"Kore"}`), 0644); err != nil {
			t.Fatal(err)
		}
		if got := LoadConfig().Brain; got != (BrainConfig{}) {
			t.Errorf("Brain = %+v, want the zero value", got)
		}
	})

	t.Run("a named provider is read back", func(t *testing.T) {
		t.Setenv("ORA_DATA_DIR", t.TempDir())
		if err := os.WriteFile(ConfigPath(), []byte(`{"brain":{"provider":"claude-cli","binary":"/usr/bin/claude","timeout_seconds":120}}`), 0644); err != nil {
			t.Fatal(err)
		}
		got := LoadConfig().Brain
		want := BrainConfig{Provider: BrainClaudeCLI, Binary: "/usr/bin/claude", TimeoutSeconds: 120}
		if got != want {
			t.Errorf("Brain = %+v, want %+v", got, want)
		}
	})
}

// A config file with no transcribe block leaves the zero value in place, which the recorder reads as whisper — the behaviour ORA had before the block existed. A file that does carry one is read verbatim.
func TestLoadConfig_Transcribe(t *testing.T) {
	t.Run("absent block is the whisper default", func(t *testing.T) {
		t.Setenv("ORA_DATA_DIR", t.TempDir())
		if err := os.WriteFile(ConfigPath(), []byte(`{"voice":"Kore"}`), 0644); err != nil {
			t.Fatal(err)
		}
		if got := LoadConfig().Transcribe; got != (TranscribeConfig{}) {
			t.Errorf("Transcribe = %+v, want the zero value", got)
		}
	})

	t.Run("the gpu device is read back", func(t *testing.T) {
		t.Setenv("ORA_DATA_DIR", t.TempDir())
		if err := os.WriteFile(ConfigPath(), []byte(`{"transcribe":{"gpu_device":1}}`), 0644); err != nil {
			t.Fatal(err)
		}
		if got := LoadConfig().Transcribe.GPUDevice; got != 1 {
			t.Errorf("GPUDevice = %d, want 1", got)
		}
	})
}

// Hours resolves the proactive schedule: zero fields (a config written before the block existed) become the defaults, and a negative hour passes through as the disable signal.
func TestProactiveConfig_HoursDefaultsAndDisable(t *testing.T) {
	brief, close := ProactiveConfig{}.Hours()
	if brief != DefaultBriefHour || close != DefaultCloseHour {
		t.Errorf("zero config Hours() = %d, %d; want defaults %d, %d", brief, close, DefaultBriefHour, DefaultCloseHour)
	}
	brief, close = ProactiveConfig{BriefHour: -1, CloseHour: 7}.Hours()
	if brief != -1 || close != 7 {
		t.Errorf("Hours() = %d, %d; want -1 (disabled) and 7 (explicit)", brief, close)
	}
}

// DreamHour and DreamPort resolve the dreaming schedule: zero fields (a config written before the block existed) become the defaults, and a negative hour passes through as the disable signal.
func TestDreamConfig_DefaultsAndDisable(t *testing.T) {
	if got := (DreamConfig{}).DreamHour(); got != DefaultDreamHour {
		t.Errorf("zero DreamHour() = %d, want %d", got, DefaultDreamHour)
	}
	if got := (DreamConfig{Hour: -1}).DreamHour(); got != -1 {
		t.Errorf("DreamHour() = %d, want -1 (disabled)", got)
	}
	if got := (DreamConfig{Hour: 1}).DreamHour(); got != 1 {
		t.Errorf("DreamHour() = %d, want 1", got)
	}
	if got := (DreamConfig{}).DreamPort(); got != DefaultDreamPort {
		t.Errorf("zero DreamPort() = %d, want %d", got, DefaultDreamPort)
	}
}

// TestBackgroundModel_DefaultsToFlashLite checks that every unattended job runs on the cheap high-allowance model unless the config pins one, so no background job can quietly spend the 20-request day that gemini-3.5-flash gets on the free tier.
func TestBackgroundModel_DefaultsToFlashLite(t *testing.T) {
	SetBackgroundModels(nil)
	for _, job := range BackgroundJobs() {
		if got := BackgroundModel(job); got != DefaultBackgroundModel {
			t.Errorf("BackgroundModel(%q) = %q, want the default %q", job, got, DefaultBackgroundModel)
		}
	}
}

// TestBackgroundModel_ConfigPinsAJob checks that a model named in the config's background_models map wins for that job and leaves every other job on the default.
func TestBackgroundModel_ConfigPinsAJob(t *testing.T) {
	SetBackgroundModels(map[string]string{JobMeetingMinutes: "gemini-3.5-flash"})
	t.Cleanup(func() { SetBackgroundModels(nil) })

	if got := BackgroundModel(JobMeetingMinutes); got != "gemini-3.5-flash" {
		t.Errorf("pinned job = %q, want %q", got, "gemini-3.5-flash")
	}
	if got := BackgroundModel(JobWorkingState); got != DefaultBackgroundModel {
		t.Errorf("unpinned job = %q, want the default %q", got, DefaultBackgroundModel)
	}
}

// TestBackgroundModel_UnknownJobStillAnswers checks that a job name with no entry and no default of its own falls back to the default model rather than returning an empty model name that would fail the call.
func TestBackgroundModel_UnknownJobStillAnswers(t *testing.T) {
	SetBackgroundModels(nil)
	if got := BackgroundModel("not-a-job"); got != DefaultBackgroundModel {
		t.Errorf("unknown job = %q, want the default %q", got, DefaultBackgroundModel)
	}
}

// TestBackgroundJobs_CoversEveryUnattendedJob checks that the job list the user configures by name includes each background duty that reaches a metered model.
func TestBackgroundJobs_CoversEveryUnattendedJob(t *testing.T) {
	want := []string{
		JobWorkingState,
		JobEpisodeSummary,
		JobEpisodicCompaction,
		JobNoteConsolidation,
		JobPersonalContext,
		JobMeetingMinutes,
		JobDream,
	}
	got := BackgroundJobs()
	for _, w := range want {
		found := false
		for _, g := range got {
			if g == w {
				found = true
			}
		}
		if !found {
			t.Errorf("BackgroundJobs() is missing %q", w)
		}
	}
}

// TestBackgroundBrainConfig_PinsGeminiOnlyWhenUnset checks that a background duty on the Gemini API gets the cheap high-allowance model, that a model the user pinned is left alone, and that a CLI provider is untouched because its Model field is a CLI alias rather than a Gemini model name.
func TestBackgroundBrainConfig_PinsGeminiOnlyWhenUnset(t *testing.T) {
	SetBackgroundModels(map[string]string{JobMeetingMinutes: "gemini-3.5-flash-lite"})
	t.Cleanup(func() { SetBackgroundModels(nil) })

	cases := []struct {
		name      string
		in        BrainConfig
		wantModel string
	}{
		{"an empty provider is the gemini api and gets the background model", BrainConfig{}, "gemini-3.5-flash-lite"},
		{"the named gemini api provider gets it too", BrainConfig{Provider: BrainGeminiAPI}, "gemini-3.5-flash-lite"},
		{"a model the user pinned wins", BrainConfig{Provider: BrainGeminiAPI, Model: "gemini-3.6-flash"}, "gemini-3.6-flash"},
		{"a claude-cli alias is left alone", BrainConfig{Provider: BrainClaudeCLI, Model: "sonnet"}, "sonnet"},
		{"a claude-cli with no alias stays empty rather than getting a gemini model name", BrainConfig{Provider: BrainClaudeCLI}, ""},
		{"codex is left alone", BrainConfig{Provider: BrainCodex, Model: "gpt-5.5"}, "gpt-5.5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := BackgroundBrainConfig(tc.in, JobMeetingMinutes).Model; got != tc.wantModel {
				t.Errorf("Model = %q, want %q", got, tc.wantModel)
			}
		})
	}
}

// TestDailyTokenBudgetFor_DefaultsToOff checks that a provider named with no budget, and a config that never set the map at all, both come back as 0 — off — rather than some other default, since a user who never asked for a budget should never see a warning.
func TestDailyTokenBudgetFor_DefaultsToOff(t *testing.T) {
	var zero OraConfig
	if got := zero.DailyTokenBudgetFor("codex"); got != 0 {
		t.Errorf("a config with no budgets at all = %d, want 0", got)
	}
	cfg := OraConfig{DailyTokenBudget: map[string]int{"codex": 500000}}
	if got := cfg.DailyTokenBudgetFor("gemini"); got != 0 {
		t.Errorf("a provider named with no budget = %d, want 0", got)
	}
}

// TestDailyTokenBudgetFor_ReturnsTheSetBudget checks the budget set for one provider is returned unchanged and does not leak into another provider's.
func TestDailyTokenBudgetFor_ReturnsTheSetBudget(t *testing.T) {
	cfg := OraConfig{DailyTokenBudget: map[string]int{"codex": 500000, "gemini": 1000000}}
	if got := cfg.DailyTokenBudgetFor("codex"); got != 500000 {
		t.Errorf("codex budget = %d, want 500000", got)
	}
	if got := cfg.DailyTokenBudgetFor("gemini"); got != 1000000 {
		t.Errorf("gemini budget = %d, want 1000000", got)
	}
}
