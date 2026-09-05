package config

import "testing"

// TestActRunsKeptDefaultsWhenUnset covers the three states of the setting: a config written before it existed (zero) gets the default, a number the user wrote is used as written, and a negative number turns the count cap off.
func TestActRunsKeptDefaultsWhenUnset(t *testing.T) {
	cases := []struct {
		name string
		set  int
		want int
	}{
		{"unset falls back to the default", 0, DefaultActRunKeep},
		{"a number the user set is used as written", 50, 50},
		{"a negative number means keep every run", -1, -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := OraConfig{ActRunKeep: c.set}
			if got := cfg.ActRunsKept(); got != c.want {
				t.Errorf("ActRunsKept() = %d, want %d", got, c.want)
			}
		})
	}
}

// TestActRunKeepReadsFromTheConfigFile writes the key into a config file and reads it back through LoadConfig, so the setting is one the user can actually edit rather than a field only tests set.
func TestActRunKeepReadsFromTheConfigFile(t *testing.T) {
	t.Setenv("ORA_DATA_DIR", t.TempDir())
	cfg := LoadConfig()
	cfg.ActRunKeep = 123
	if err := SaveConfig(cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	if got := LoadConfig().ActRunsKept(); got != 123 {
		t.Errorf("after saving act_run_keep = 123, LoadConfig().ActRunsKept() = %d", got)
	}
}

// TestActRunKeepDefaultIsGenerousEnoughForTheNightlyStage pins the default above the 200 newest runs the dreaming loop's procedures stage reads (internal/dream.procedureRunCap), since pruning inside that window would take runs the nightly notes have not been written from yet.
func TestActRunKeepDefaultIsGenerousEnoughForTheNightlyStage(t *testing.T) {
	const procedureRunCap = 200
	if DefaultActRunKeep <= procedureRunCap {
		t.Errorf("DefaultActRunKeep = %d, which is not above the %d runs the nightly procedures stage reads", DefaultActRunKeep, procedureRunCap)
	}
}

// TestFailedActRunsKeptDaysDefaultsWhenUnset covers the three states of the failed-run grace, the same way ActRunsKept is covered above: a config written before the setting existed (zero) gets the default, a number the user wrote is used as written, and a negative number turns the grace off so a failed run is capped by count like any other.
func TestFailedActRunsKeptDaysDefaultsWhenUnset(t *testing.T) {
	cases := []struct {
		name string
		set  int
		want int
	}{
		{"unset falls back to the default", 0, DefaultActRunFailedKeepDays},
		{"a number the user set is used as written", 7, 7},
		{"a negative number means no grace at all", -1, -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := OraConfig{ActRunFailedKeepDays: c.set}
			if got := cfg.FailedActRunsKeptDays(); got != c.want {
				t.Errorf("FailedActRunsKeptDays() = %d, want %d", got, c.want)
			}
		})
	}
}

// TestActRunFailedKeepDaysReadsFromTheConfigFile writes the key into a config file and reads it back through LoadConfig, so the grace is one the user can edit rather than a number only tests set — which is the whole point of moving it out of internal/db.
func TestActRunFailedKeepDaysReadsFromTheConfigFile(t *testing.T) {
	t.Setenv("ORA_DATA_DIR", t.TempDir())
	cfg := LoadConfig()
	cfg.ActRunFailedKeepDays = 9
	if err := SaveConfig(cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	if got := LoadConfig().FailedActRunsKeptDays(); got != 9 {
		t.Errorf("after saving act_run_failed_keep_days = 9, LoadConfig().FailedActRunsKeptDays() = %d", got)
	}
}
