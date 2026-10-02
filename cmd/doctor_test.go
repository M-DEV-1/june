package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

// june doctor read the environment as the shell handed it over and nothing else, so a key sitting in the env file the first-run panel tells the user to write, the file the daemon itself reads, was invisible.
func TestLoadEnvFiles_ReadsTheKeyTheDaemonReads(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("JUNE_DATA_DIR", dir)
	t.Setenv("GEMINI_API_KEY", "restored-by-cleanup")
	os.Unsetenv("GEMINI_API_KEY") // godotenv never overwrites a variable that is already set, and an empty one counts as set

	if err := os.WriteFile(filepath.Join(dir, "env"), []byte("GEMINI_API_KEY=from-the-env-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	loadEnvFiles()

	if got := os.Getenv("GEMINI_API_KEY"); got != "from-the-env-file" {
		t.Fatalf("GEMINI_API_KEY = %q, want it read from %s", got, filepath.Join(dir, "env"))
	}
}
