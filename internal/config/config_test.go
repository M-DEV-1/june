package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDataDir_ORADataDirOverrideWins verifies ORA_DATA_DIR, when set, is used verbatim — the override hook tests (and any future scripted/portable install) use to point ORA at a throwaway or explicit directory instead of a real XDG path.
func TestDataDir_ORADataDirOverrideWins(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ORA_DATA_DIR", dir)

	if got := DataDir(); got != dir {
		t.Errorf("DataDir() = %q, want override %q", got, dir)
	}
}

// TestDataDir_UsesXDGDataHome verifies that with no override set, DataDir resolves to $XDG_DATA_HOME/ora.
func TestDataDir_UsesXDGDataHome(t *testing.T) {
	t.Setenv("ORA_DATA_DIR", "")
	xdg := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdg)

	want := filepath.Join(xdg, "ora")
	if got := DataDir(); got != want {
		t.Errorf("DataDir() = %q, want %q", got, want)
	}
}

// TestDataDir_FallsBackToHomeLocalShare verifies that with neither override set, DataDir resolves to ~/.local/share/ora.
func TestDataDir_FallsBackToHomeLocalShare(t *testing.T) {
	t.Setenv("ORA_DATA_DIR", "")
	t.Setenv("XDG_DATA_HOME", "")
	home := t.TempDir()
	t.Setenv("HOME", home)

	want := filepath.Join(home, ".local", "share", "ora")
	if got := DataDir(); got != want {
		t.Errorf("DataDir() = %q, want %q", got, want)
	}
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

// TestDataDir_NoLegacyDir_NoMigration verifies DataDir doesn't error or create anything unexpected when there's no legacy ora-db to migrate — the common case for a fresh install.
func TestDataDir_NoLegacyDir_NoMigration(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("ORA_DATA_DIR", "")
	xdg := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdg)

	want := filepath.Join(xdg, "ora")
	if got := DataDir(); got != want {
		t.Errorf("DataDir() = %q, want %q", got, want)
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
