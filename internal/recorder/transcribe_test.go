package recorder

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ora/internal/db"
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

// A silent clip is a real outcome, not a failure: whisper-cli exits nonzero when it cannot read or decode the audio, so an empty result with a clean exit is a meeting nobody spoke in.
func TestTranscribeWAV_SilentClipIsNotAnError(t *testing.T) {
	bin := fakeWhisper(t, "", "")
	segs, err := transcribeWAV(context.Background(), bin, testWAV(t), speakerMe, "", 0)
	if err != nil {
		t.Fatalf("transcribeWAV: %v", err)
	}
	if len(segs) != 0 {
		t.Errorf("got %+v, want no segments", segs)
	}
}

// Stderr chatter must not cost us the transcript: whisper prints progress and banners to stderr on every successful run too.
func TestTranscribeWAV_KeepsSegmentsWhenStderrIsJustNoise(t *testing.T) {
	bin := fakeWhisper(t, "[00:00:00.000 --> 00:00:01.000]   hello there", "whisper_model_load: model size = 487.01 MB")
	segs, err := transcribeWAV(context.Background(), bin, testWAV(t), speakerMe, "", 0)
	if err != nil {
		t.Fatalf("transcribeWAV: %v", err)
	}
	if len(segs) != 1 || segs[0].Text != "hello there" {
		t.Errorf("got %+v, want the one spoken segment", segs)
	}
}

// whisper prints one line per segment as "[start --> end]   text". Everything else it prints (banners, blank lines) is not a segment.
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
		{Start: 2 * time.Second, End: 4 * time.Second, Speaker: speakerCall, Text: "morning"},
		{Start: 5 * time.Second, End: 6 * time.Second, Speaker: speakerCall, Text: "ship it friday"},
	}
	got := renderTranscript(append(mine, theirs...))
	want := "[00:00:00] [me] morning all\n\n" +
		"[00:00:02] [call] morning ship it friday\n\n" +
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

// Whisper loops on thin audio: the same invented sentence came back five times in the real 2026-08-28 recording, scattered between other lines rather than in one run. A sentence of ordinary length repeated verbatim that often is the decoder stuck, not something anyone said, so every copy goes.
func TestDropHallucinations_DropsAScatteredVerbatimRepeat(t *testing.T) {
	const dog = "I'm going to try to reach out to my dog."
	segs := []Segment{
		{Start: 11 * time.Second, Speaker: speakerCall, Text: dog},
		{Start: 14 * time.Second, Speaker: speakerCall, Text: "Okay."},
		{Start: 15 * time.Second, Speaker: speakerCall, Text: dog},
		{Start: 18 * time.Second, Speaker: speakerCall, Text: "Okay."},
		{Start: 19 * time.Second, Speaker: speakerCall, Text: dog},
		{Start: 33 * time.Second, Speaker: speakerCall, Text: "Are you sharing something?"},
	}
	for _, s := range dropHallucinations(segs) {
		if s.Text == dog {
			t.Fatalf("the looped sentence survived: %+v", dropHallucinations(segs))
		}
	}
}

// A phrase people really do repeat must survive. Twice is emphasis, not a loop.
func TestDropHallucinations_KeepsAPhraseSaidTwice(t *testing.T) {
	const line = "It will be same for all of them in the same country."
	segs := []Segment{
		{Start: 37 * time.Second, Speaker: speakerCall, Text: line},
		{Start: 41 * time.Second, Speaker: speakerCall, Text: line},
	}
	if got := dropHallucinations(segs); len(got) != 2 {
		t.Errorf("got %d segments, want both kept: %+v", len(got), got)
	}
}

// Runs of a bare acknowledgement are whisper filling silence. Three or more in a row from one stream collapse to one; two stay as they are.
func TestDropHallucinations_CollapsesARunOfIdenticalSegments(t *testing.T) {
	segs := []Segment{
		{Start: 21 * time.Second, Speaker: speakerCall, Text: "Okay."},
		{Start: 22 * time.Second, Speaker: speakerCall, Text: "Okay."},
		{Start: 23 * time.Second, Speaker: speakerCall, Text: "Okay."},
		{Start: 24 * time.Second, Speaker: speakerCall, Text: "Okay."},
		{Start: 33 * time.Second, Speaker: speakerCall, Text: "Are you sharing something?"},
	}
	got := dropHallucinations(segs)
	if len(got) != 2 || got[0].Text != "Okay." || got[1].Text != "Are you sharing something?" {
		t.Errorf("got %+v, want one Okay. and the real line", got)
	}
}

// Whisper also loops inside a single segment: one line of the real transcript was the word "Okay." thirteen times over. The repeat is collapsed without touching the words around it.
func TestDropHallucinations_CollapsesARepeatInsideOneSegment(t *testing.T) {
	segs := []Segment{{Speaker: speakerMe, Text: "read. Okay. Okay. Okay. Okay. Okay. Okay. Okay. Okay."}}
	got := dropHallucinations(segs)
	if len(got) != 1 || got[0].Text != "read. Okay." {
		t.Errorf("got %+v, want \"read. Okay.\"", got)
	}
}

// A sentence said twice inside one segment is emphasis and stays.
func TestDropHallucinations_KeepsADoubleInsideOneSegment(t *testing.T) {
	segs := []Segment{{Speaker: speakerMe, Text: "We have to do it. We have to do it."}}
	if got := dropHallucinations(segs); got[0].Text != "We have to do it. We have to do it." {
		t.Errorf("got %q, want the pair kept", got[0].Text)
	}
}

// Whisper's real loops drift: each decode window slides a little further into the repeated phrase, so consecutive segments carry the same sentence at a different word offset and are never byte-identical. This is verbatim (retimed) from the 2026-09-01 22:47 recording, where it ran for the full 15 minutes and none of the exact-match rules above caught a single copy of it.
func TestDropHallucinations_DropsADriftingLoop(t *testing.T) {
	segs := []Segment{
		{Speaker: speakerCall, Text: "Now coming to the question which he asked."},
		{Speaker: speakerCall, Text: "In X, if you saw that video, there is something called as positive engagement and negative engagement."},
		{Speaker: speakerCall, Text: "That means that for example, you are not in a positive mood. You are in a positive mood. You are not in a positive mood. You are"},
		{Speaker: speakerCall, Text: "not in a positive mood. You are not in a positive mood. You are not in a positive mood. You are"},
		{Speaker: speakerCall, Text: "not in a positive mood. You are not in a positive mood. You are not in a positive mood. You are not in a positive mood. You are not in"},
		{Speaker: speakerCall, Text: "a positive mood. You are not in a positive mood. You are not in a positive mood. You are"},
	}
	got := dropHallucinations(segs)
	for _, s := range got {
		if strings.Contains(s.Text, "positive mood") {
			t.Fatalf("the drifting loop survived: %+v", got)
		}
	}
	if len(got) == 0 || !strings.Contains(got[0].Text, "Now coming to the question") {
		t.Errorf("the real opening sentence should have survived, got %+v", got)
	}
}

// The same drift, with the loop's own wording sliding too: on the 2026-09-03 23:44 recording the loop reworded "55%" to "56%" and back between copies. A window of words away from the changed one still matches, so the loop goes even though no two copies agree on the number.
func TestDropHallucinations_DropsADriftingLoopWithWordSubstitution(t *testing.T) {
	segs := []Segment{
		{Speaker: speakerMe, Text: "so now you have used a total of 56% right, yes, okay, so now you have used a total"},
		{Speaker: speakerMe, Text: "of 56% right, so now you have used a total of 56% right, so now you have used a total"},
		{Speaker: speakerMe, Text: "of 55% right, so now you have used a total of 56% right, so now you have used a total of 55% right, so now you have used a total"},
	}
	got := dropHallucinations(segs)
	for _, s := range got {
		if strings.Contains(s.Text, "used a total") {
			t.Fatalf("the drifting loop survived: %+v", got)
		}
	}
}

// A line-per-segment transcript shreds both speakers into fragments. Consecutive segments from one stream are one turn, printed as one paragraph timestamped at its start.
func TestRenderTranscript_MergesConsecutiveSegmentsIntoOneTurn(t *testing.T) {
	segs := []Segment{
		{Start: 0, End: 2 * time.Second, Speaker: speakerMe, Text: "morning all"},
		{Start: 2 * time.Second, End: 4 * time.Second, Speaker: speakerMe, Text: "did you see the sheet"},
		{Start: 5 * time.Second, End: 6 * time.Second, Speaker: speakerCall, Text: "yes."},
		{Start: 7 * time.Second, End: 9 * time.Second, Speaker: speakerCall, Text: "ship it friday"},
		{Start: 10 * time.Second, End: 11 * time.Second, Speaker: speakerMe, Text: "sounds good"},
	}
	want := "[00:00:00] [me] morning all did you see the sheet\n\n" +
		"[00:00:05] [call] yes. ship it friday\n\n" +
		"[00:00:10] [me] sounds good\n"
	if got := renderTranscript(segs); got != want {
		t.Errorf("transcript mismatch\n got:\n%s\nwant:\n%s", got, want)
	}
}

// The priming prompt is built out of what Ora already saw on screen during the meeting, so whisper spells the domain's own words: the acronyms and the proper nouns, plus the people whose names were on screen.
func TestPrimingPrompt_CarriesTheWindowsAcronymsAndNames(t *testing.T) {
	eps := []db.Episode{
		{Title: "Excalidraw Whiteboard - Brave", ScreenText: "Priya Shah: ok sure ping me. Alex Rivera: I also found this INFORM Risk Scoring formula and the GRDI numbers. INFORM again. GRDI again."},
		{Title: "Acme Essentials - Climate Reporting Platform - Brave", ScreenText: "Climate Risk Studio Double Materiality Assessment ASRS"},
	}
	got := primingPrompt(eps)
	for _, want := range []string{"INFORM", "GRDI", "ASRS", "Priya Shah", "Climate Risk Studio"} {
		if !strings.Contains(got, want) {
			t.Errorf("priming prompt is missing %q:\n%s", want, got)
		}
	}
	if len(got) > primingPromptBudget {
		t.Errorf("priming prompt is %d chars, over the %d-char budget", len(got), primingPromptBudget)
	}
	// Whisper continues the prompt's register as well as its words: primed with raw lowercase screen text it returns the whole transcript lowercase and unpunctuated, so the prompt has to read as capitalised, punctuated sentences.
	if !strings.HasPrefix(got, "Meeting notes.") || !strings.HasSuffix(got, ".") {
		t.Errorf("priming prompt must read as sentences, got:\n%s", got)
	}
}

// Browser and app chrome is on every screen Ora captures and is not what the meeting is about, so it must not crowd the real terms out of the prompt.
func TestPrimingPrompt_LeavesOutBrowserChrome(t *testing.T) {
	eps := []db.Episode{{Title: "GitHub - Brave", ScreenText: "Type / to search Pull requests Add file Code Insights Settings Search Search Ctrl K"}}
	got := primingPrompt(eps)
	for _, junk := range []string{"Ctrl K", "Search Search", "Add file"} {
		if strings.Contains(got, junk) {
			t.Errorf("priming prompt carries chrome %q:\n%s", junk, got)
		}
	}
}

// Nothing on screen means no prompt at all, rather than a prompt made of nothing that whisper would try to continue.
func TestPrimingPrompt_EmptyWhenThereIsNoContext(t *testing.T) {
	if got := primingPrompt(nil); got != "" {
		t.Errorf("got %q, want no prompt", got)
	}
}

// Whisper keeps only the last whisperMaxContext tokens of this prompt, so whatever sits at the front is what gets cut when the prompt runs long. Participant names matter more than terms for getting the transcript right, so terms have to come first and the names last, where truncation can't reach them.
func TestPrimingPrompt_NamesComeAfterTermsSoTheySurviveTruncation(t *testing.T) {
	eps := []db.Episode{
		{Title: "Meet - abc-defg-hij - Brave", ScreenText: "Priya Shah: ok sure ping me. INFORM Risk Scoring GRDI numbers Climate Risk Studio"},
	}
	got := primingPrompt(eps)
	termsAt, participantsAt := strings.Index(got, "Terms:"), strings.Index(got, "Participants:")
	if termsAt == -1 || participantsAt == -1 {
		t.Fatalf("expected both a Terms and a Participants section, got:\n%s", got)
	}
	if participantsAt < termsAt {
		t.Errorf("participants must come after terms, got:\n%s", got)
	}
	if !strings.HasSuffix(got, "Priya Shah.") {
		t.Errorf("the participant name should be the last thing in the prompt, got:\n%s", got)
	}
}

// Knowing how many times faster than real time this machine transcribes is what lets the user predict how long N hours of meetings take to catch up on, so every run logs the audio length and the rate alongside the wall time it already logged.
func TestTranscribeWAV_LogsAudioDurationAndRate(t *testing.T) {
	logs := captureLogs(t)
	bin := fakeWhisper(t, "", "")

	path := filepath.Join(t.TempDir(), "mic.wav")
	w, err := newWAV(path)
	if err != nil {
		t.Fatalf("newWAV: %v", err)
	}
	// One second of 16 kHz mono 16-bit audio.
	if _, err := w.Write(make([]byte, sampleRate*2)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if _, err := transcribeWAV(context.Background(), bin, path, speakerMe, "", 0); err != nil {
		t.Fatalf("transcribeWAV: %v", err)
	}

	got := logs.String()
	if !strings.Contains(got, "whisper finished") {
		t.Fatalf("expected the whisper-finished log line, got:\n%s", got)
	}
	if !strings.Contains(got, "audio=1s") {
		t.Errorf("expected the log to carry the audio's own length (1s), got:\n%s", got)
	}
	if !strings.Contains(got, "rate=") {
		t.Errorf("expected the log to carry the audio-to-wall-time rate, got:\n%s", got)
	}
}

// The anti-hallucination thresholds and the priming prompt only do anything if they actually reach the whisper process, and nothing else in the pipeline would notice if they stopped being passed.
func TestTranscribeWAV_PassesThresholdsAndPrompt(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	bin := filepath.Join(dir, "echo-args")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho \"$@\" > "+argsFile+"\n"), 0o755); err != nil {
		t.Fatalf("write fake whisper: %v", err)
	}
	if _, err := transcribeWAV(context.Background(), bin, testWAV(t), speakerMe, "GDIS, INFORM", 0); err != nil {
		t.Fatalf("transcribeWAV: %v", err)
	}
	got, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read back the arguments whisper was given: %v", err)
	}
	for _, want := range []string{"-et " + entropyThreshold, "-lpt " + logProbThreshold, "-mc " + whisperMaxContext, "--prompt GDIS, INFORM"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("whisper was called without %q: %s", want, got)
		}
	}
}

// The 2026-08-31 standup rendered as a single [call] turn of 21,158 characters covering the last thirty minutes of a thirty-nine minute meeting: whisper produced hundreds of timestamped segments and the turn merge threw every one of their timestamps away. Six people spoke in that block and the minutes model had no boundary anywhere to attribute against. A turn has to end somewhere.
func TestRenderTranscript_BreaksALongUnbrokenRunIntoTurns(t *testing.T) {
	// Ten minutes of back-to-back call audio, no pauses and no speaker markers — the shape that produced the 21k paragraph.
	var segs []Segment
	for i := 0; i < 120; i++ {
		at := time.Duration(i) * 5 * time.Second
		segs = append(segs, Segment{Start: at, End: at + 5*time.Second, Speaker: speakerCall, Text: "and then we looked at the numbers again."})
	}
	lines := strings.Count(renderTranscript(segs), "[00:")
	if lines < 2 {
		t.Fatalf("ten minutes of unbroken call audio rendered as %d turn(s); a turn must be bounded", lines)
	}
}

// Segments a couple of seconds apart with nothing between them are one person still talking, and splitting there is what shredded the transcript into fragments before turns existed.
func TestRenderTranscript_KeepsAContinuousRunTogether(t *testing.T) {
	segs := []Segment{
		{Start: 0, End: 2 * time.Second, Speaker: speakerMe, Text: "morning all"},
		{Start: 2200 * time.Millisecond, End: 4 * time.Second, Speaker: speakerMe, Text: "did you see the sheet"},
	}
	if got := renderTranscript(segs); got != "[00:00:00] [me] morning all did you see the sheet\n" {
		t.Errorf("got %q", got)
	}
}

// The 2026-08-31 standup: a WhatsApp tab was open five minutes in, and its chat senders went into the priming prompt as the meeting's participants. Only the window the call is actually running in names the people on the call.
func TestPrimingPrompt_TakesParticipantsOnlyFromTheMeetingWindow(t *testing.T) {
	eps := []db.Episode{
		{App: "Brave Browser", Title: "WhatsApp - Brave", ScreenText: "Rohit Verma: Abhi renew hua"},
		{App: "Brave Browser", Title: "Meet - abc-defg-hij - Brave", ScreenText: "Vikram Goel: sharing my screen now"},
	}
	_, participants, _ := strings.Cut(primingPrompt(eps), "Participants:")
	if strings.Contains(participants, "Rohit") {
		t.Errorf("a WhatsApp sender was named as a meeting participant: %q", participants)
	}
	if !strings.Contains(participants, "Vikram Goel") {
		t.Errorf("the meeting window's own chat sender was dropped: %q", participants)
	}
}

// Whisper reads its priming prompt back as speech: the 2026-08-31 transcript opened with "Participants: Rohit Verma, Claude Artifact." timestamped at 00:00:00, which the minutes model then treated as something a person said.
func TestStripPromptEcho_DropsTheEchoedPrompt(t *testing.T) {
	const prompt = "Meeting notes. Terms: INFORM, GRDI. Participants: Vikram Goel."
	segs := []Segment{
		{Start: 0, End: time.Second, Speaker: speakerCall, Text: "Participants: Vikram Goel. Good afternoon."},
		{Start: 2 * time.Second, End: 3 * time.Second, Speaker: speakerCall, Text: "is my screen visible?"},
	}
	got := stripPromptEcho(segs, prompt)
	if got[0].Text != "Good afternoon." {
		t.Errorf("got %q, want the echo stripped and the speech kept", got[0].Text)
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

// On the 2026-09-02 17:32 call the priming prompt made whisper decode the whole 21-minute call side as 308 lines of "[ Silence ]" plus one invented sentence, while the same file without the prompt gave 42 real lines. A run whose output is mostly one repeated non-speech marker is that loop, and the cure is to run again without the prompt.
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

// Whisper spells a name the way it is primed to. On 2026-09-03 a call with Sneha was primed with "Meeting notes. Terms: API." because the screen had nothing, and the transcript said "Ashar", which then became a person in memory. The names the store already knows are the cheapest priming there is, so they go into the prompt after the screen's own terms.
func TestPrimingPromptFor_IncludesKnownPeople(t *testing.T) {
	got := primingPromptFor(nil, []string{"Priya Shah", "Sneha"})
	for _, want := range []string{"Priya Shah", "Sneha"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt %q lacks known person %q", got, want)
		}
	}
	if len(got) > primingPromptBudget {
		t.Errorf("prompt is %d chars, over the %d budget", len(got), primingPromptBudget)
	}
	if primingPrompt(nil) != primingPromptFor(nil, nil) {
		t.Error("primingPrompt must stay the no-people form of primingPromptFor")
	}
}

// personNamesFromContext turns the store's hyphenated person subjects into the names whisper should hear: "priya-shah" is "Priya Shah". Subjects that are not people (identity, preferences-*) are left out.
func TestPersonNamesFromContext(t *testing.T) {
	entries := []db.PersonalEntry{{Subject: "identity", Content: "x"}, {Subject: "priya-shah", Content: "x"}, {Subject: "preferences-notes", Content: "x"}, {Subject: "krish-littlebird", Content: "x"}}
	got := personNamesFromContext(entries)
	want := []string{"Priya Shah", "Krish Littlebird"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v, want %v", got, want)
	}
}
