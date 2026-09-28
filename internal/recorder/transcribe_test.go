package recorder

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"june/internal/db"
)

// fakeWhisper writes a shell script that stands in for whisper-cli: it prints script to stdout, complaint to stderr, and exits 0. Returns its path.
func fakeWhisper(t *testing.T, stdout, stderr string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-whisper")
	body := "#!/bin/sh\ncat <<'EOF'\n" + stdout + "\nEOF\ncat <<'EOF' >&2\n" + stderr + "\nEOF\nexit 0\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake whisper: %v", err)
	}
	return path
}

// testWAV writes a small valid recording so repairWAV has something to open.
func testWAV(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mic.wav")
	w, err := newWAV(path)
	if err != nil {
		t.Fatalf("newWAV: %v", err)
	}
	if _, err := w.Write(make([]byte, 1600)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return path
}

// A silent clip is a real outcome, not a failure: whisper-cli exits nonzero when it cannot read or decode the audio, so an empty result with a clean exit is a meeting nobody spoke in. Stderr chatter must not cost us the transcript either: whisper prints progress and banners to stderr on every successful run too.
func TestTranscribeWAV_SilentClipIsNotAnError(t *testing.T) {
	cases := []struct {
		name     string
		stdout   string
		stderr   string
		wantLen  int
		wantText string
	}{
		{name: "a clean exit with nothing printed is silence, not an error"},
		{
			name:     "stderr noise on a successful run does not cost the transcript",
			stdout:   "[00:00:00.000 --> 00:00:01.000]   hello there",
			stderr:   "whisper_model_load: model size = 487.01 MB",
			wantLen:  1,
			wantText: "hello there",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bin := fakeWhisper(t, c.stdout, c.stderr)
			segs, err := transcribeWAV(context.Background(), bin, testWAV(t), speakerMe, "", 0)
			if err != nil {
				t.Fatalf("transcribeWAV: %v", err)
			}
			if len(segs) != c.wantLen {
				t.Fatalf("got %+v, want %d segments", segs, c.wantLen)
			}
			if c.wantLen == 1 && segs[0].Text != c.wantText {
				t.Errorf("got %+v, want text %q", segs, c.wantText)
			}
		})
	}
}

// whisper prints one line per segment as "[start --> end]   text". Everything else — banners, blank lines, a fractional timestamp of however many digits the build chose to print, a stream's own offset from the recording's start, and a bracketed non-speech marker — has to be read correctly or dropped outright.
func TestParseSegments(t *testing.T) {
	cases := []struct {
		name    string
		out     string
		speaker string
		offset  time.Duration
		check   func(t *testing.T, segs []Segment)
	}{
		{
			name: "one line per segment, banners and junk lines are not segments",
			out: `
[00:00:00.000 --> 00:00:07.640]   This is a LibriVox recording.
[00:00:07.640 --> 00:01:17.840]   For more information please visit librivox.org.
not a segment line
[01:02:03.500 --> 01:02:04.000]
`,
			speaker: speakerMe,
			check: func(t *testing.T, segs []Segment) {
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
			},
		},
		{
			// The fractional part of a whisper timestamp is however many digits the build chose to print, so its length sets its scale: ".06" is 60ms, not 6ms, and ".5" is half a second, not half a millisecond.
			name:    "the fractional part of a timestamp scales by its digit count",
			out:     "[00:00:00.06 --> 00:00:01.5]   hello",
			speaker: speakerMe,
			check: func(t *testing.T, segs []Segment) {
				if len(segs) != 1 {
					t.Fatalf("got %d segments, want 1", len(segs))
				}
				if segs[0].Start != 60*time.Millisecond {
					t.Errorf("start = %v, want 60ms", segs[0].Start)
				}
				if segs[0].End != 1500*time.Millisecond {
					t.Errorf("end = %v, want 1.5s", segs[0].End)
				}
			},
		},
		{
			// The two streams do not open at the same instant, so each stream's segments are shifted by how late that stream started relative to the recording as a whole.
			name:    "a stream offset shifts every segment in it",
			out:     "[00:00:01.000 --> 00:00:02.000]   hello",
			speaker: speakerCall,
			offset:  500 * time.Millisecond,
			check: func(t *testing.T, segs []Segment) {
				if len(segs) != 1 {
					t.Fatalf("got %d segments, want 1", len(segs))
				}
				if segs[0].Start != 1500*time.Millisecond {
					t.Errorf("start = %v, want 1.5s", segs[0].Start)
				}
				if segs[0].End != 2500*time.Millisecond {
					t.Errorf("end = %v, want 2.5s", segs[0].End)
				}
			},
		},
		{
			// whisper marks silence and non-speech sound as a bracketed pseudo-segment. One side of a call is silent most of the time, so those lines would otherwise be most of the transcript.
			name:    "non-speech markers are dropped",
			out:     "[00:00:00.000 --> 00:00:07.000]   [BLANK_AUDIO]\n[00:00:07.000 --> 00:00:08.000]   (upbeat music)\n[00:00:08.000 --> 00:00:09.000]   real words here",
			speaker: speakerMe,
			check: func(t *testing.T, segs []Segment) {
				if len(segs) != 1 || segs[0].Text != "real words here" {
					t.Errorf("expected only the spoken segment, got %+v", segs)
				}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.check(t, parseSegments(c.out, c.speaker, c.offset))
		})
	}
}

// dropHallucinations has to tell a real repeat from a loop: a phrase said twice, whether across segments or inside one, is emphasis and survives; three or more identical segments in a row, or the same sentence repeated verbatim within one segment, is whisper stuck and collapses; and whisper's real loops drift, sliding the repeated phrase's word offset or even a number inside it between copies, so the drop has to catch those too.
func TestDropHallucinations(t *testing.T) {
	cases := []struct {
		name  string
		segs  []Segment
		check func(t *testing.T, got []Segment)
	}{
		{
			// Whisper loops on thin audio: the same invented sentence came back five times in a real recording, scattered between other lines rather than in one run.
			name: "a scattered verbatim repeat is dropped everywhere it occurs",
			segs: []Segment{
				{Start: 11 * time.Second, Speaker: speakerCall, Text: "I'm going to try to reach out to my dog."},
				{Start: 14 * time.Second, Speaker: speakerCall, Text: "Okay."},
				{Start: 15 * time.Second, Speaker: speakerCall, Text: "I'm going to try to reach out to my dog."},
				{Start: 18 * time.Second, Speaker: speakerCall, Text: "Okay."},
				{Start: 19 * time.Second, Speaker: speakerCall, Text: "I'm going to try to reach out to my dog."},
				{Start: 33 * time.Second, Speaker: speakerCall, Text: "Are you sharing something?"},
			},
			check: func(t *testing.T, got []Segment) {
				for _, s := range got {
					if s.Text == "I'm going to try to reach out to my dog." {
						t.Fatalf("the looped sentence survived: %+v", got)
					}
				}
			},
		},
		{
			name: "a phrase said twice across segments survives",
			segs: []Segment{
				{Start: 37 * time.Second, Speaker: speakerCall, Text: "It will be same for all of them in the same country."},
				{Start: 41 * time.Second, Speaker: speakerCall, Text: "It will be same for all of them in the same country."},
			},
			check: func(t *testing.T, got []Segment) {
				if len(got) != 2 {
					t.Errorf("got %d segments, want both kept: %+v", len(got), got)
				}
			},
		},
		{
			name: "three or more identical segments in a row collapse to one",
			segs: []Segment{
				{Start: 21 * time.Second, Speaker: speakerCall, Text: "Okay."},
				{Start: 22 * time.Second, Speaker: speakerCall, Text: "Okay."},
				{Start: 23 * time.Second, Speaker: speakerCall, Text: "Okay."},
				{Start: 24 * time.Second, Speaker: speakerCall, Text: "Okay."},
				{Start: 33 * time.Second, Speaker: speakerCall, Text: "Are you sharing something?"},
			},
			check: func(t *testing.T, got []Segment) {
				if len(got) != 2 || got[0].Text != "Okay." || got[1].Text != "Are you sharing something?" {
					t.Errorf("got %+v, want one Okay. and the real line", got)
				}
			},
		},
		{
			// Whisper also loops inside a single segment: one line of a real transcript was the word "Okay." thirteen times over.
			name: "a repeat inside one segment collapses without touching the words around it",
			segs: []Segment{{Speaker: speakerMe, Text: "read. Okay. Okay. Okay. Okay. Okay. Okay. Okay. Okay."}},
			check: func(t *testing.T, got []Segment) {
				if len(got) != 1 || got[0].Text != "read. Okay." {
					t.Errorf("got %+v, want \"read. Okay.\"", got)
				}
			},
		},
		{
			name: "a sentence said twice inside one segment is emphasis and stays",
			segs: []Segment{{Speaker: speakerMe, Text: "We have to do it. We have to do it."}},
			check: func(t *testing.T, got []Segment) {
				if got[0].Text != "We have to do it. We have to do it." {
					t.Errorf("got %q, want the pair kept", got[0].Text)
				}
			},
		},
		{
			// Whisper's real loops drift: each decode window slides a little further into the repeated phrase, so consecutive segments carry the same sentence at a different word offset and are never byte-identical.
			name: "a drifting loop is dropped even without an exact match",
			segs: []Segment{
				{Speaker: speakerCall, Text: "Now coming to the question which he asked."},
				{Speaker: speakerCall, Text: "In X, if you saw that video, there is something called as positive engagement and negative engagement."},
				{Speaker: speakerCall, Text: "That means that for example, you are not in a positive mood. You are in a positive mood. You are not in a positive mood. You are"},
				{Speaker: speakerCall, Text: "not in a positive mood. You are not in a positive mood. You are not in a positive mood. You are"},
				{Speaker: speakerCall, Text: "not in a positive mood. You are not in a positive mood. You are not in a positive mood. You are not in a positive mood. You are not in"},
				{Speaker: speakerCall, Text: "a positive mood. You are not in a positive mood. You are not in a positive mood. You are"},
			},
			check: func(t *testing.T, got []Segment) {
				for _, s := range got {
					if strings.Contains(s.Text, "positive mood") {
						t.Fatalf("the drifting loop survived: %+v", got)
					}
				}
				if len(got) == 0 || !strings.Contains(got[0].Text, "Now coming to the question") {
					t.Errorf("the real opening sentence should have survived, got %+v", got)
				}
			},
		},
		{
			// The same drift, with the loop's own wording sliding too: a real recording reworded "55%" to "56%" and back between copies. A window of words away from the changed one still has to match, so the loop goes even though no two copies agree on the number.
			name: "a drifting loop is dropped even when a number inside it changes between copies",
			segs: []Segment{
				{Speaker: speakerMe, Text: "so now you have used a total of 56% right, yes, okay, so now you have used a total"},
				{Speaker: speakerMe, Text: "of 56% right, so now you have used a total of 56% right, so now you have used a total"},
				{Speaker: speakerMe, Text: "of 55% right, so now you have used a total of 56% right, so now you have used a total of 55% right, so now you have used a total"},
			},
			check: func(t *testing.T, got []Segment) {
				for _, s := range got {
					if strings.Contains(s.Text, "used a total") {
						t.Fatalf("the drifting loop survived: %+v", got)
					}
				}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.check(t, dropHallucinations(c.segs))
		})
	}
}

// The transcript is the two streams merged into one chronological conversation: consecutive segments from one stream are one turn printed as one paragraph at its start time, a run within the continuity gap stays one turn, an empty stream still yields the other side's transcript, and a long unbroken run still has to end somewhere rather than becoming one enormous paragraph with every timestamp thrown away.
func TestRenderTranscript(t *testing.T) {
	cases := []struct {
		name  string
		segs  []Segment
		check func(t *testing.T, got string)
	}{
		{
			name: "interleaves both sides by start time, merging each side's own consecutive segments",
			segs: append([]Segment{
				{Start: 0, End: time.Second, Speaker: speakerMe, Text: "morning all"},
				{Start: 10 * time.Second, End: 11 * time.Second, Speaker: speakerMe, Text: "sounds good"},
			}, []Segment{
				{Start: 2 * time.Second, End: 4 * time.Second, Speaker: speakerCall, Text: "morning"},
				{Start: 5 * time.Second, End: 6 * time.Second, Speaker: speakerCall, Text: "ship it friday"},
			}...),
			check: func(t *testing.T, got string) {
				want := "[00:00:00] [me] morning all\n\n" +
					"[00:00:02] [call] morning ship it friday\n\n" +
					"[00:00:10] [me] sounds good\n"
				if got != want {
					t.Errorf("transcript mismatch\n got:\n%s\nwant:\n%s", got, want)
				}
			},
		},
		{
			name: "an empty stream still yields the other side's transcript",
			segs: []Segment{{Start: time.Second, Speaker: speakerCall, Text: "anyone there?"}},
			check: func(t *testing.T, got string) {
				if got != "[00:00:01] [call] anyone there?\n" {
					t.Errorf("got %q", got)
				}
			},
		},
		{
			name: "consecutive segments from one speaker merge into one turn",
			segs: []Segment{
				{Start: 0, End: 2 * time.Second, Speaker: speakerMe, Text: "morning all"},
				{Start: 2 * time.Second, End: 4 * time.Second, Speaker: speakerMe, Text: "did you see the sheet"},
				{Start: 5 * time.Second, End: 6 * time.Second, Speaker: speakerCall, Text: "yes."},
				{Start: 7 * time.Second, End: 9 * time.Second, Speaker: speakerCall, Text: "ship it friday"},
				{Start: 10 * time.Second, End: 11 * time.Second, Speaker: speakerMe, Text: "sounds good"},
			},
			check: func(t *testing.T, got string) {
				want := "[00:00:00] [me] morning all did you see the sheet\n\n" +
					"[00:00:05] [call] yes. ship it friday\n\n" +
					"[00:00:10] [me] sounds good\n"
				if got != want {
					t.Errorf("transcript mismatch\n got:\n%s\nwant:\n%s", got, want)
				}
			},
		},
		{
			// Segments a couple of seconds apart with nothing between them are one person still talking, and splitting there is what shredded the transcript into fragments before turns existed.
			name: "a run within the continuity gap stays one turn",
			segs: []Segment{
				{Start: 0, End: 2 * time.Second, Speaker: speakerMe, Text: "morning all"},
				{Start: 2200 * time.Millisecond, End: 4 * time.Second, Speaker: speakerMe, Text: "did you see the sheet"},
			},
			check: func(t *testing.T, got string) {
				if got != "[00:00:00] [me] morning all did you see the sheet\n" {
					t.Errorf("got %q", got)
				}
			},
		},
		{
			// Ten minutes of back-to-back call audio, no pauses and no speaker markers, once rendered as a single 21,158-character turn with every timestamp thrown away and no boundary anywhere to attribute against. A turn has to end somewhere.
			name: "a long unbroken run breaks into more than one turn",
			segs: func() []Segment {
				var segs []Segment
				for i := 0; i < 120; i++ {
					at := time.Duration(i) * 5 * time.Second
					segs = append(segs, Segment{Start: at, End: at + 5*time.Second, Speaker: speakerCall, Text: "and then we looked at the numbers again."})
				}
				return segs
			}(),
			check: func(t *testing.T, got string) {
				if lines := strings.Count(got, "[00:"); lines < 2 {
					t.Fatalf("ten minutes of unbroken call audio rendered as %d turn(s); a turn must be bounded", lines)
				}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.check(t, renderTranscript(c.segs))
		})
	}
}

// The priming prompt is built out of what June already saw on screen during the meeting: it carries the domain's own acronyms, proper nouns and participant names within budget and readable as sentences, leaves out browser chrome, produces nothing at all when there is no context, orders names after terms so truncation can't reach them, takes participants only from the meeting's own window, and includes the people the personal-context store already knows.
func TestPrimingPrompt(t *testing.T) {
	cases := []struct {
		name  string
		eps   []db.Episode
		known []string
		check func(t *testing.T, got string)
	}{
		{
			name: "carries the window's acronyms and names, within budget, as sentences",
			eps: []db.Episode{
				{Title: "Excalidraw Whiteboard - Brave", ScreenText: "Vexil Quorin: ok sure ping me. Zemna Braxen: I also found this ORVEC Risk Scoring formula and the KDNI numbers. ORVEC again. KDNI again."},
				{Title: "Acme Basics - Brightpath Reporting Platform - Brave", ScreenText: "Brightpath Studio Regional Coverage Assessment VRDS"},
			},
			check: func(t *testing.T, got string) {
				for _, want := range []string{"ORVEC", "KDNI", "VRDS", "Vexil Quorin", "Brightpath Studio"} {
					if !strings.Contains(got, want) {
						t.Errorf("priming prompt is missing %q:\n%s", want, got)
					}
				}
				if len(got) > primingPromptBudget {
					t.Errorf("priming prompt is %d chars, over the %d-char budget", len(got), primingPromptBudget)
				}
				// Whisper continues the prompt's register as well as its words: primed with raw lowercase screen text it returns the whole transcript lowercase and unpunctuated.
				if !strings.HasPrefix(got, "Meeting notes.") || !strings.HasSuffix(got, ".") {
					t.Errorf("priming prompt must read as sentences, got:\n%s", got)
				}
			},
		},
		{
			name: "leaves out browser chrome",
			eps:  []db.Episode{{Title: "GitHub - Brave", ScreenText: "Type / to search Pull requests Add file Code Insights Settings Search Search Ctrl K"}},
			check: func(t *testing.T, got string) {
				for _, junk := range []string{"Ctrl K", "Search Search", "Add file"} {
					if strings.Contains(got, junk) {
						t.Errorf("priming prompt carries chrome %q:\n%s", junk, got)
					}
				}
			},
		},
		{
			name: "nothing on screen means no prompt at all",
			check: func(t *testing.T, got string) {
				if got != "" {
					t.Errorf("got %q, want no prompt", got)
				}
			},
		},
		{
			// Whisper keeps only the last whisperMaxContext tokens of this prompt, so participant names matter more than terms for getting the transcript right and have to come after them, where truncation can't reach them.
			name: "names come after terms so they survive truncation",
			eps:  []db.Episode{{Title: "Meet - abc-defg-hij - Brave", ScreenText: "Vexil Quorin: ok sure ping me. ORVEC Risk Scoring KDNI numbers Brightpath Studio"}},
			check: func(t *testing.T, got string) {
				termsAt, participantsAt := strings.Index(got, "Terms:"), strings.Index(got, "Participants:")
				if termsAt == -1 || participantsAt == -1 {
					t.Fatalf("expected both a Terms and a Participants section, got:\n%s", got)
				}
				if participantsAt < termsAt {
					t.Errorf("participants must come after terms, got:\n%s", got)
				}
				if !strings.HasSuffix(got, "Vexil Quorin.") {
					t.Errorf("the participant name should be the last thing in the prompt, got:\n%s", got)
				}
			},
		},
		{
			// Only the window the call is actually running in names the people on the call: a WhatsApp tab open at the same time must not contribute its chat senders as participants.
			name: "participants come only from the meeting window",
			eps: []db.Episode{
				{App: "Brave Browser", Title: "WhatsApp - Brave", ScreenText: "Yalven Pravik: Kal shaam ko bhej dunga"},
				{App: "Brave Browser", Title: "Meet - abc-defg-hij - Brave", ScreenText: "Ravix Dolmen: sharing my screen now"},
			},
			check: func(t *testing.T, got string) {
				_, participants, _ := strings.Cut(got, "Participants:")
				if strings.Contains(participants, "Yalven") {
					t.Errorf("a WhatsApp sender was named as a meeting participant: %q", participants)
				}
				if !strings.Contains(participants, "Ravix Dolmen") {
					t.Errorf("the meeting window's own chat sender was dropped: %q", participants)
				}
			},
		},
		{
			// The names the store already knows are the cheapest priming there is, so they go into the prompt after the screen's own terms.
			name:  "known people from personal context are included",
			known: []string{"Vexil Quorin", "Sorrek"},
			check: func(t *testing.T, got string) {
				for _, want := range []string{"Vexil Quorin", "Sorrek"} {
					if !strings.Contains(got, want) {
						t.Errorf("prompt %q lacks known person %q", got, want)
					}
				}
				if len(got) > primingPromptBudget {
					t.Errorf("prompt is %d chars, over the %d budget", len(got), primingPromptBudget)
				}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.check(t, primingPromptFor(c.eps, c.known))
		})
	}
}

// Diarization gives the call side its turns as spans on the same clock as the transcript, so a segment belongs to whichever voice it shares the most time with. A segment the diarizer never covered keeps the pooled label rather than being guessed at.
func TestAssignSpeakers_TakesTheMostOverlappedCluster(t *testing.T) {
	segs := []Segment{
		{Start: 0, End: 4 * time.Second, Speaker: speakerCall, Text: "mostly the first voice"},
		{Start: 20 * time.Second, End: 22 * time.Second, Speaker: speakerCall, Text: "nobody was placed here"},
		{Start: 0, End: 4 * time.Second, Speaker: speakerMe, Text: "my own microphone"},
	}
	turns := []diarTurn{
		{Start: 0, End: 3 * time.Second, Speaker: 0},
		{Start: 3 * time.Second, End: 6 * time.Second, Speaker: 1},
	}
	got := assignSpeakers(segs, turns)
	if got[0].Speaker != "call:S1" {
		t.Errorf("segment overlapping cluster 0 for 3s and cluster 1 for 1s got %q", got[0].Speaker)
	}
	if got[1].Speaker != speakerCall {
		t.Errorf("an uncovered segment got %q, want the pooled label", got[1].Speaker)
	}
	if got[2].Speaker != speakerMe {
		t.Errorf("the microphone side was relabelled to %q", got[2].Speaker)
	}
}

// A run whose output is mostly one repeated non-speech marker is the priming-prompt silence loop and must be redone without the prompt; a quiet stream with real words between its markers, or empty output, is not that loop.
func TestMarkerLooped(t *testing.T) {
	looped := "[00:00:00.000 --> 00:00:30.000]   [ Silence ]\n[00:00:30.000 --> 00:01:00.000]   [ Silence ]\n[00:01:00.000 --> 00:01:30.000]   [ Silence ]\n[00:01:30.000 --> 00:01:32.000]   Me, I am frustrated.\n"
	if !markerLooped(looped) {
		t.Error("three marker lines against one sentence is the silence loop and must be caught")
	}
	fine := "[00:00:00.000 --> 00:00:18.000]   [no audio]\n[00:00:18.000 --> 00:00:20.000]   Yes.\n[00:00:22.000 --> 00:00:24.000]   Year in order.\n"
	if markerLooped(fine) {
		t.Error("a quiet stream with real words between its markers is not a loop")
	}
	if markerLooped("") {
		t.Error("empty output is not a loop")
	}
}

// fakeWhisperLogging is fakeWhisper with a model file beside it, so whisperCPPArgs treats it as a real whisper.cpp build, and with every invocation's arguments appended to a log file. Input: what the script should print to stdout. Output: the script's path and the path of the argument log, one line per run.
func fakeWhisperLogging(t *testing.T, stdout string) (bin, argLog string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "fake-whisper")
	argLog = filepath.Join(dir, "args.log")
	body := "#!/bin/sh\necho \"$@\" >> " + argLog + "\ncat <<'EOF'\n" + stdout + "\nEOF\nexit 0\n"
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake whisper: %v", err)
	}
	// whisperCPPArgs only adds the model flags when the model sits beside the binary, so the file has to be there for this to be the GPU path. Nothing reads its contents.
	if err := os.WriteFile(filepath.Join(dir, whisperCPPModelName), []byte("not a model"), 0o644); err != nil {
		t.Fatalf("write fake model: %v", err)
	}
	return bin, argLog
}

// runArgs returns the arguments of each run the fake whisper logged, in order.
func runArgs(t *testing.T, argLog string) []string {
	t.Helper()
	b, err := os.ReadFile(argLog)
	if err != nil {
		t.Fatalf("read the fake whisper's argument log: %v", err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

// The priming prompt is a spelling aid, and whisper sometimes reads it back as the first thing said in the meeting, which the minutes model then takes as evidence about who was in the call. This is the test that stripPromptEcho is actually wired into the run rather than merely defined.
func TestTranscribeWAV_DropsThePromptWhisperReadBackAsSpeech(t *testing.T) {
	prompt := "A meeting recording. Participants: Yalven Pravik, Claude Artifact."
	bin := fakeWhisper(t, "[00:00:00.000 --> 00:00:02.000]   Participants: Yalven Pravik, Claude Artifact.\n[00:00:03.000 --> 00:00:05.000]   shall we ship on friday", "")

	segs, err := transcribeWAV(context.Background(), bin, testWAV(t), speakerCall, prompt, 0)
	if err != nil {
		t.Fatalf("transcribeWAV: %v", err)
	}
	if len(segs) != 1 || segs[0].Text != "shall we ship on friday" {
		t.Fatalf("got %+v, want only the spoken segment: the echoed prompt is still in the transcript", segs)
	}
}

// A run that loops on a silence marker is redone without the prompt, and the redo has to keep everything else the first run had — above all the model flags, without which whisper-cli looks for a model that is not there and the whole meeting fails.
func TestTranscribeWAV_TheUnprimedRetryKeepsTheModelFlags(t *testing.T) {
	looped := "[00:00:00.000 --> 00:00:01.000]   [ Silence ]\n[00:00:01.000 --> 00:00:02.000]   [ Silence ]\n[00:00:02.000 --> 00:00:03.000]   hello"
	bin, argLog := fakeWhisperLogging(t, looped)

	if _, err := transcribeWAV(context.Background(), bin, testWAV(t), speakerMe, "Participants: Vexil Quorin.", 0); err != nil {
		t.Fatalf("transcribeWAV: %v", err)
	}

	runs := runArgs(t, argLog)
	if len(runs) != 2 {
		t.Fatalf("whisper ran %d times, want 2: the primed run and the unprimed redo\n%v", len(runs), runs)
	}
	if !strings.Contains(runs[0], "--prompt") {
		t.Errorf("the first run carried no prompt: %s", runs[0])
	}
	if strings.Contains(runs[1], "--prompt") {
		t.Errorf("the redo still carried the prompt it was redone to drop: %s", runs[1])
	}
	for i, args := range runs {
		if !strings.Contains(args, "-m ") {
			t.Errorf("run %d lost the model flags, so whisper has no model to load: %s", i, args)
		}
	}
}

// A whisper run that decodes on the GPU holds GPURun for the whole decode, so the two streams of a call run one after the other and each has the machine to itself. Splitting the cores four ways there leaves three quarters of them idle for the length of every meeting. Only the CPU path really overlaps, and the thread count on either path is a property of the machine, so an explicit override wins on both.
func TestWhisperThreads_AGPURunGetsMoreThanACPURunBecauseItIsAlone(t *testing.T) {
	cpu, gpu := whisperThreads(false), whisperThreads(true)
	if cpu < 1 || gpu < 1 {
		t.Fatalf("whisperThreads = %d on the CPU and %d on the GPU, want at least one thread either way", cpu, gpu)
	}
	if runtime.NumCPU() >= 4 && gpu <= cpu {
		t.Errorf("whisperThreads = %d on the GPU and %d on the CPU of %d cores, want the serialised GPU run to get more", gpu, cpu, runtime.NumCPU())
	}
	if max := runtime.NumCPU()/2 + 1; gpu > max {
		t.Errorf("a GPU run asked for %d threads on %d cores, want no more than %d", gpu, runtime.NumCPU(), max)
	}

	t.Setenv("JUNE_TRANSCRIBE_THREADS", "6")
	if cpu, gpu := whisperThreads(false), whisperThreads(true); cpu != 6 || gpu != 6 {
		t.Errorf("whisperThreads = %d on the CPU and %d on the GPU, want the configured 6 either way", cpu, gpu)
	}
}

// fakeWhisperDyingOnTheGPUFirst writes a stand-in for whisper-cli that dies the way the real one died on 2026-09-06 — the Vulkan allocation failure on stderr and then SIGSEGV — on its first run, and transcribes normally on every run after that. Returns its path and the path of the log each run appends its arguments to.
func fakeWhisperDyingOnTheGPUFirst(t *testing.T, stdout string) (bin, argLog string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "fake-whisper")
	argLog = filepath.Join(dir, "args.log")
	body := "#!/bin/sh\n" +
		"echo \"$@\" >> " + argLog + "\n" +
		"if [ ! -f " + dir + "/ran ]; then\n" +
		"  touch " + dir + "/ran\n" +
		"  echo 'ggml_vulkan: Device memory allocation of size 462323712 failed.' >&2\n" +
		"  echo 'ggml_vulkan: vk::Device::allocateMemory: ErrorOutOfDeviceMemory' >&2\n" +
		"  kill -SEGV $$\n" +
		"fi\n" +
		"cat <<'EOF'\n" + stdout + "\nEOF\nexit 0\n"
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake whisper: %v", err)
	}
	// whisperCPPArgs only adds the model flags when the model sits beside the binary, so the file has to be there for this to be the GPU path.
	if err := os.WriteFile(filepath.Join(dir, whisperCPPModelName), []byte("not a model"), 0o644); err != nil {
		t.Fatalf("write fake model: %v", err)
	}
	return bin, argLog
}

// The card is shared with the embedding server and a shadow model, so a decode can find no memory on it and die mid-run. The same audio decodes on the CPU, so the run is redone there instead of the words being thrown away.
func TestTranscribeWAV_RetriesOnTheCPUWhenTheGPURunDies(t *testing.T) {
	bin, argLog := fakeWhisperDyingOnTheGPUFirst(t, "[00:00:00.000 --> 00:00:01.000]   shall we ship on friday")

	segs, err := transcribeWAV(context.Background(), bin, testWAV(t), speakerMe, "", 0)
	if err != nil {
		t.Fatalf("transcribeWAV: %v", err)
	}
	if len(segs) != 1 || segs[0].Text != "shall we ship on friday" {
		t.Fatalf("got %+v, want the segment the CPU retry transcribed", segs)
	}
	runs := runArgs(t, argLog)
	if len(runs) != 2 {
		t.Fatalf("whisper ran %d times, want 2: the GPU run that died and the CPU retry\n%v", len(runs), runs)
	}
	if strings.Contains(runs[0], "-ng") {
		t.Errorf("the first run was already on the CPU: %s", runs[0])
	}
	if !strings.Contains(runs[1], "-ng") {
		t.Errorf("the retry did not turn the GPU off, so it fails for the same reason: %s", runs[1])
	}
	if !strings.Contains(runs[1], "-m ") {
		t.Errorf("the retry lost the model flags, so whisper has no model to load: %s", runs[1])
	}
}

// A retry that fails too is still a failure the user has to be told about, rather than an empty transcript that reads as a meeting nobody spoke in.
func TestTranscribeWAV_ReportsTheErrorWhenTheCPURetryFailsToo(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-whisper")
	body := "#!/bin/sh\necho 'ggml_vulkan: vk::Device::allocateMemory: ErrorOutOfDeviceMemory' >&2\nexit 3\n"
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake whisper: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, whisperCPPModelName), []byte("not a model"), 0o644); err != nil {
		t.Fatalf("write fake model: %v", err)
	}

	if _, err := transcribeWAV(context.Background(), bin, testWAV(t), speakerMe, "", 0); err == nil {
		t.Fatal("transcribeWAV returned no error, want the failure of both attempts")
	}
}
