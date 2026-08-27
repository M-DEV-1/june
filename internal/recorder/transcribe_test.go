package recorder

import (
	"strings"
	"testing"
	"time"
)

// whisperfile prints one line per segment as "[start --> end]   text". Everything else it prints (banners, blank lines) is not a segment.
func TestParseSegments(t *testing.T) {
	out := `
[00:00:00.000 --> 00:00:07.640]   This is a LibriVox recording.
[00:00:07.640 --> 00:01:17.840]   For more information please visit librivox.org.
not a segment line
[01:02:03.500 --> 01:02:04.000]
`
	segs := parseSegments(out, speakerMe, 0)
	if len(segs) != 2 {
		t.Fatalf("got %d segments, want 2 (blank text and junk lines dropped): %+v", len(segs), segs)
	}
	if segs[0].Start != 0 || segs[0].End != 7640*time.Millisecond {
		t.Errorf("segment 0 span = %v..%v, want 0..7.64s", segs[0].Start, segs[0].End)
	}
	if segs[0].Text != "This is a LibriVox recording." {
		t.Errorf("segment 0 text = %q", segs[0].Text)
	}
	if segs[1].Start != 7640*time.Millisecond || segs[1].End != 77840*time.Millisecond {
		t.Errorf("segment 1 span = %v..%v, want 7.64s..1m17.84s", segs[1].Start, segs[1].End)
	}
	if segs[0].Speaker != speakerMe {
		t.Errorf("speaker = %q, want %q", segs[0].Speaker, speakerMe)
	}
}

// The fractional part of a whisper timestamp is however many digits the build chose to print, so its length sets its scale: ".06" is 60ms, not 6ms, and ".5" is half a second, not half a millisecond.
func TestParseSegments_FractionalSecondsScaleByDigitCount(t *testing.T) {
	segs := parseSegments("[00:00:00.06 --> 00:00:01.5]   hello", speakerMe, 0)
	if len(segs) != 1 {
		t.Fatalf("got %d segments, want 1", len(segs))
	}
	if segs[0].Start != 60*time.Millisecond {
		t.Errorf("start = %v, want 60ms", segs[0].Start)
	}
	if segs[0].End != 1500*time.Millisecond {
		t.Errorf("end = %v, want 1.5s", segs[0].End)
	}
}

// The two streams do not open at the same instant, so each stream's segments are shifted by how late that stream started relative to the recording as a whole.
func TestParseSegments_AppliesStreamOffset(t *testing.T) {
	segs := parseSegments("[00:00:01.000 --> 00:00:02.000]   hello", speakerCall, 500*time.Millisecond)
	if len(segs) != 1 {
		t.Fatalf("got %d segments, want 1", len(segs))
	}
	if segs[0].Start != 1500*time.Millisecond {
		t.Errorf("start = %v, want 1.5s", segs[0].Start)
	}
	if segs[0].End != 2500*time.Millisecond {
		t.Errorf("end = %v, want 2.5s", segs[0].End)
	}
}

// The transcript is the two streams merged into one chronological conversation, each line marked with who spoke.
func TestRenderTranscript_InterleavesByStartTime(t *testing.T) {
	mine := []Segment{
		{Start: 0, End: time.Second, Speaker: speakerMe, Text: "morning all"},
		{Start: 10 * time.Second, End: 11 * time.Second, Speaker: speakerMe, Text: "sounds good"},
	}
	theirs := []Segment{
		{Start: 2 * time.Second, End: 3 * time.Second, Speaker: speakerCall, Text: "morning"},
		{Start: 5 * time.Second, End: 6 * time.Second, Speaker: speakerCall, Text: "ship it friday"},
	}
	got := renderTranscript(append(mine, theirs...))
	want := "[00:00:00] [me] morning all\n" +
		"[00:00:02] [call] morning\n" +
		"[00:00:05] [call] ship it friday\n" +
		"[00:00:10] [me] sounds good\n"
	if got != want {
		t.Errorf("transcript mismatch\n got:\n%s\nwant:\n%s", got, want)
	}
}

// An empty stream (nobody unmuted their mic, say) still yields the other side's transcript rather than an error.
func TestRenderTranscript_OneSideEmpty(t *testing.T) {
	got := renderTranscript([]Segment{{Start: time.Second, Speaker: speakerCall, Text: "anyone there?"}})
	if got != "[00:00:01] [call] anyone there?\n" {
		t.Errorf("got %q", got)
	}
}

// whisper marks silence and non-speech sound as a bracketed pseudo-segment. One side of a call is silent most of the time, so those lines would otherwise be most of the transcript.
func TestParseSegments_DropsNonSpeechMarkers(t *testing.T) {
	out := `[00:00:00.000 --> 00:00:07.000]   [BLANK_AUDIO]
[00:00:07.000 --> 00:00:08.000]   (upbeat music)
[00:00:08.000 --> 00:00:09.000]   real words here`
	segs := parseSegments(out, speakerMe, 0)
	if len(segs) != 1 || segs[0].Text != "real words here" {
		t.Errorf("expected only the spoken segment, got %+v", segs)
	}
}

// The system stream is labelled [call], never [them]: it pools several voices, and a later diarization pass is meant to replace that placeholder with names in place.
func TestSpeakerLabels(t *testing.T) {
	if speakerCall == "them" {
		t.Fatal("the system-audio label must not be \"them\"")
	}
	got := renderTranscript([]Segment{{Speaker: speakerCall, Text: "hi"}})
	if strings.Contains(got, "[them]") {
		t.Errorf("transcript still uses [them]: %q", got)
	}
}
