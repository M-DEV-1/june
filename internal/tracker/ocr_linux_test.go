//go:build linux

package tracker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Tesseract prints a form feed after each page, blank lines between blocks, and short runs of symbols for icons and borders. Only lines with words in them are kept, and the result is capped like accessibility text.
func TestCleanOCR(t *testing.T) {
	raw := "Chapter 4: Capture pipelines\n\n  | ==  \n~ i\nfox jumps over 42 dogs  \n\f"
	want := "Chapter 4: Capture pipelines\nfox jumps over 42 dogs"
	if got := cleanOCR(raw); got != want {
		t.Fatalf("cleanOCR = %q, want %q", got, want)
	}
	if got := cleanOCR(strings.Repeat("word ", maxTextLen)); len([]rune(got)) > maxTextLen {
		t.Fatalf("cleanOCR kept %d runes, want at most %d", len([]rune(got)), maxTextLen)
	}
}

// fakeTesseract puts a program named tesseract first on PATH, ahead of any real one. Input: the body of the shell script to run in its place.
func fakeTesseract(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tesseract"), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":/usr/bin:/bin")
}

// The OCR function pipes the picture to tesseract's stdin, reads the answer from its stdout, and leaves no process behind: here the fake program echoes how many bytes it was sent and the thread limit it was started with.
func TestNewOCR_RunsTesseractOnStdin(t *testing.T) {
	fakeTesseract(t, `echo "read $(wc -c) bytes with $OMP_THREAD_LIMIT thread"`)
	ocr := newOCR()
	if ocr == nil {
		t.Fatal("newOCR() = nil with tesseract on PATH")
	}
	if got, want := ocr(context.Background(), []byte("12345")), "read 5 bytes with 1 thread"; got != want {
		t.Fatalf("ocr = %q, want %q", got, want)
	}
}

// A tesseract that hangs is killed when the capture's bound passes, and one that fails reads as no text.
func TestNewOCR_HangAndFailure(t *testing.T) {
	fakeTesseract(t, "exec sleep 30")
	ocr := newOCR()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if got := ocr(ctx, []byte("png")); got != "" || time.Since(start) > 5*time.Second {
		t.Fatalf("hung tesseract: got %q after %v, want nothing at the bound", got, time.Since(start))
	}

	fakeTesseract(t, "echo 'half an answer'; exit 1")
	if got := newOCR()(context.Background(), []byte("png")); got != "" {
		t.Fatalf("failing tesseract: got %q, want nothing", got)
	}
}

// Without tesseract installed there is no OCR function at all, so the capture never takes a screenshot on its account.
func TestNewOCR_NilWithoutTesseract(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if newOCR() != nil {
		t.Fatal("newOCR() is not nil with no tesseract on PATH")
	}
}
