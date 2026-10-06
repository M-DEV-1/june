package ipc

import (
	"log/slog"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
)

// OpenCommand starts the platform's command to open url in the system browser, the same three-way switch internal/agent/tools.go uses for its open_url tool: xdg-open on Linux, open on macOS, rundll32's FileProtocolHandler on Windows. Input: the url. Output: an error if the command could not be started. The child is waited on in the background so it does not stay a zombie once it has exited.
func OpenCommand(rawURL string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL)
	case "darwin":
		cmd = exec.Command("open", rawURL)
	default:
		cmd = exec.Command("xdg-open", rawURL)
	}
	letOpenedComeForward()
	if err := cmd.Start(); err != nil {
		return err
	}
	// Reaped on its own goroutine: xdg-open exits in milliseconds, and a child nobody waits on stays a zombie in the daemon's process table for the life of the daemon.
	go func() { _ = cmd.Wait() }()
	return nil
}

// Open handles POST /open: it hands url to run, the system browser opener, for a reply link the desktop window's WebKitGTK webview cannot reliably send to the browser itself with window.open. Input: JSON body {"url": string}; run is OpenCommand in production and a fake in tests, so a test never launches a real command. Output: 204 once run has started the command; 400 for a body that will not decode or a url whose scheme is not http or https — rejected before run is ever called, so a reply cannot smuggle a javascript: or file: link into a shell command; 500 if run returned an error.
func Open(run func(string) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			URL string `json:"url"`
		}
		if !DecodeJSON(w, r, &req) {
			return
		}
		parsed, err := url.Parse(req.URL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			http.Error(w, "url must be http or https", http.StatusBadRequest)
			return
		}
		if err := run(req.URL); err != nil {
			slog.Error("open: could not open the url", "url", req.URL, "error", err)
			http.Error(w, "could not open that link", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
