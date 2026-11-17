//go:build linux

package tracker

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"unicode"
)

// minOCRLineLetters is how many letters or digits a line of OCR output needs to be kept. Icons, borders and video frames come out of tesseract as short runs of symbols, and none of them is text worth storing.
const minOCRLineLetters = 3

// newOCR returns a function that reads the text off a PNG with the tesseract program, or nil when tesseract is not installed. Nothing stays resident: each call starts tesseract, pipes the picture to its stdin, and the process exits when it has answered.
// It runs on one thread because tesseract's own threading made a 1920x1080 read take 8 s instead of 3.5 s here, most of the difference being time the threads spent waiting on each other.
func newOCR() func(ctx context.Context, png []byte) string {
	path, err := exec.LookPath("tesseract")
	if err != nil {
		return nil
	}
	return func(ctx context.Context, png []byte) string {
		cmd := exec.CommandContext(ctx, path, "stdin", "stdout")
		cmd.Env = append(os.Environ(), "OMP_THREAD_LIMIT=1")
		cmd.Stdin = bytes.NewReader(png)
		out, err := cmd.Output()
		if err != nil {
			slog.Debug("tesseract could not read the screenshot", "error", err)
			return ""
		}
		return cleanOCR(string(out))
	}
}

// cleanOCR keeps the lines of OCR output that hold words. Input: what tesseract printed. Output: those lines, trimmed and joined with newlines, capped like accessibility text.
func cleanOCR(raw string) string {
	var kept []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if wordRunes(line) >= minOCRLineLetters {
			kept = append(kept, line)
		}
	}
	return trimText(strings.Join(kept, "\n"))
}

// wordRunes counts the letters and digits in s.
func wordRunes(s string) int {
	n := 0
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			n++
		}
	}
	return n
}
