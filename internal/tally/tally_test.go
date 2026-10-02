package tally

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeRecorder captures every RecordUsage call for assertions, and can be told to fail.
type fakeRecorder struct {
	calls   []recordedCall
	failErr error
}

type recordedCall struct {
	provider string
	ok       bool
	ms       time.Duration
}

func (f *fakeRecorder) RecordUsage(provider string, ok bool, ms time.Duration, promptChars, replyChars int) error {
	f.calls = append(f.calls, recordedCall{provider, ok, ms})
	return f.failErr
}

// TestWrap_SuccessAndFailure_TableDriven asserts Wrap records exactly one call per invocation, with ok/failed set from whether the wrapped Brain returned an error, and passes the reply/error through unchanged, even when the Recorder itself fails.
func TestWrap_SuccessAndFailure_TableDriven(t *testing.T) {
	cases := []struct {
		name    string
		brainOK bool
		recErr  error
		wantOK  bool
	}{
		{"success is recorded ok=true", true, nil, true},
		{"failure is recorded ok=false", false, nil, false},
		{"a failing recorder never fails the call", true, errors.New("disk full"), true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &fakeRecorder{failErr: tc.recErr}
			var brainErr error
			if !tc.brainOK {
				brainErr = errors.New("brain down")
			}
			wrapped := Wrap("claude-cli", func(ctx context.Context, prompt string) (string, error) {
				return "reply", brainErr
			}, rec)

			reply, err := wrapped(context.Background(), "prompt")

			if reply != "reply" {
				t.Errorf("reply = %q, want passthrough %q", reply, "reply")
			}
			if !errors.Is(err, brainErr) && err != brainErr {
				t.Errorf("err = %v, want passthrough %v", err, brainErr)
			}
			if len(rec.calls) != 1 {
				t.Fatalf("recorded %d calls, want 1", len(rec.calls))
			}
			got := rec.calls[0]
			if got.provider != "claude-cli" {
				t.Errorf("provider = %q, want %q", got.provider, "claude-cli")
			}
			if got.ok != tc.wantOK {
				t.Errorf("ok = %v, want %v", got.ok, tc.wantOK)
			}
			if got.ms < 0 {
				t.Errorf("ms = %v, want >= 0", got.ms)
			}
		})
	}
}
