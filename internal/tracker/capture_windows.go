//go:build windows

package tracker

import (
	"context"
	_ "embed"
	"errors"
	"os"
	"os/exec"
	"strings"
)

//go:embed uia.ps1
var uiaScript string

// captureScreen is a no-op — UIA reads live from the OS, no pixel capture needed.
func captureScreen() ([]byte, error) {
	return nil, nil
}

// grabScreen is not yet implemented on Windows — UIA text capture covers most
// cases; a BitBlt-based visual tier can be added later.
func grabScreen(_ context.Context) ([]byte, error) {
	return nil, errors.New("screenshot not implemented on windows")
}

// extractText reads structured text from the focused window via UI Automation.
// Returns empty string (not error) when the focused element is null or unreadable.
func extractText() (string, error) {
	// write the embedded script to a temp file so PowerShell can run it with -File
	// this avoids any path interpolation inside the script itself
	scriptFile, err := os.CreateTemp("", "ora-uia-*.ps1")
	if err != nil {
		return "", err
	}
	scriptPath := scriptFile.Name()
	defer os.Remove(scriptPath)

	if _, err := scriptFile.WriteString(uiaScript); err != nil {
		scriptFile.Close()
		return "", err
	}
	scriptFile.Close()

	// TODO: powershell.exe cold start is ~200-400ms per call. eventually swap to
	// a persistent host (stdin pipe, send paths, read stdout) if recapture
	// cadence tightens.
	cmd := exec.Command(
		"powershell.exe", "-NoProfile", "-NonInteractive",
		"-File", scriptPath,
	)
	out, err := cmd.Output()
	if err != nil {
		return "", nil
	}
	return strings.TrimSpace(string(out)), nil
}
