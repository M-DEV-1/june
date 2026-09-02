package ui

import "time"

// probably the only folder which needed these constants cuz its just too much
const (
	MinViewportHeight     = 5
	GutterWidth           = 10
	DefaultWidth          = 80
	MaxTextareaHeight     = 6
	MinWaveWidth          = 20
	DefaultListWidth      = 60
	DefaultListHeight     = 8
	DefaultViewportHeight = 20
)

// DaemonPollInterval is how often the TUI re-checks the daemon's /status endpoint, so a crash/restart while the TUI is open still shows up in the header pill.
const DaemonPollInterval = 12 * time.Second
