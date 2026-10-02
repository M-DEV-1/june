//go:build !windows

package cmd

import (
	"os"
	"os/exec"
	"path/filepath"

	"june/internal/util"
)

// audioChecks reports the PipeWire pieces recording and call detection go through: the pulse server and pw-dump. Input: $XDG_RUNTIME_DIR. Output: one check each.
func audioChecks(runtimeDir string) []doctorCheck {
	var out []doctorCheck
	// Recording goes through PipeWire's pulse server, found the way github.com/jfreymuth/pulse finds it: $PULSE_SERVER when set, the socket under $XDG_RUNTIME_DIR otherwise.
	if server := os.Getenv("PULSE_SERVER"); server != "" {
		out = append(out, doctorCheck{Name: "audio server", Detail: "PULSE_SERVER is " + server, OK: true})
	} else if sock := filepath.Join(runtimeDir, "pulse", "native"); !util.Exists(sock) {
		out = append(out, doctorCheck{Name: "audio server", Detail: "no pulse socket at " + sock + "; meeting recording and voice input are off", Fix: "install and start PipeWire with its pulse server (pipewire-pulse)"})
	} else {
		out = append(out, doctorCheck{Name: "audio server", Detail: "pulse socket at " + sock, OK: true})
	}
	if bin, err := exec.LookPath("pw-dump"); err != nil {
		out = append(out, doctorCheck{Name: "call detection", Detail: "no pw-dump on PATH (" + os.Getenv("PATH") + "); noticing a call and offering to record it is off", Fix: "install PipeWire's command-line tools (pipewire-bin on Debian and Ubuntu)"})
	} else {
		out = append(out, doctorCheck{Name: "call detection", Detail: "pw-dump at " + bin, OK: true})
	}
	return out
}
