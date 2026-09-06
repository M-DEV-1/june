package recorder

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"ora/internal/db"
	oratext "ora/internal/text"
	"ora/internal/tracker"
)

// Segment is one span of speech from one side of the call, with its start and end measured from the beginning of the recording (not from the beginning of its own stream).
type Segment struct {
	Start, End time.Duration
	Speaker    string // speakerMe for the microphone, speakerCall for the system audio
	Text       string
}

// The two labels a transcript line can carry. speakerCall is deliberately neutral rather than "them": every remote voice is pooled into it here, and a later diarization pass is meant to split it into named speakers in place.
const (
	speakerMe   = "me"
	speakerCall = "call"
)

// Decoder thresholds passed to whisper on every run. Whisper decodes each window at a rising temperature and keeps the first result that passes these two tests, so tightening them makes it retry a bad window instead of printing what it invented on the first pass.
// entropyThreshold is the token-distribution entropy above which a decode counts as failed; whisper.cpp's own default is 2.40 and lowering it catches more of the flat, low-confidence decoding that produces looped sentences on near-silence.
// logProbThreshold is the mean per-token log probability below which a decode counts as failed; whisper.cpp defaults to -1.00, and raising it rejects text the model was never confident about.
// whisperMaxContext caps how much already-transcribed text whisper carries into the next window as its prompt. Conditioning on the previous window is what lets one invented sentence prime the next window to invent it again, and whisper.cpp's default of 224 tokens carries a whole loop forward. It cannot go to zero: the same setting bounds the priming prompt, and zero would throw that away too, so it is set a little above primingPromptBudget instead.
const (
	entropyThreshold  = "2.20"
	logProbThreshold  = "-0.70"
	whisperMaxContext = "160"
)

// transcribeWAV runs whisper over one WAV and returns its segments, labelled with speaker and shifted by offset — how much later this stream started than the recording as a whole.
// prompt is the initial text whisper conditions on, which biases spelling towards the words in it; pass "" for none.
// It repairs the WAV header first, so a file left behind by a crashed daemon still transcribes.
func transcribeWAV(ctx context.Context, bin, path, speaker, prompt string, offset time.Duration) ([]Segment, error) {
	if err := repairWAV(path); err != nil {
		return nil, fmt.Errorf("repair %s: %w", path, err)
	}
	// The model flags mean a real whisper.cpp run, which decodes on the GPU, where only one run fits at a time.
	gpu := whisperCPPArgs(bin)
	unprimed := []string{"-f", path, "-np", "-et", entropyThreshold, "-lpt", logProbThreshold, "-mc", whisperMaxContext, "-t", strconv.Itoa(whisperThreads(gpu != nil))}
	unprimed = append(unprimed, gpu...)
	// The prompt goes on the end and nowhere else, because dropping it again for the unprimed redo below is done by running the arguments without it. Appending anything after it took the model flags away from that redo instead.
	args := unprimed
	if prompt != "" {
		args = append(append([]string{}, unprimed...), "--prompt", prompt)
	}
	if gpu != nil {
		GPURun.Lock()
		defer GPURun.Unlock()
		// Under the lock, so the other stream cannot slip its decode in while this one is still waiting for the card.
		if err := waitForGPU(ctx); err != nil {
			return nil, err
		}
	}
	started := time.Now()
	out, errOut, err := run(ctx, bin, args)
	// A primed run can lock onto a non-speech marker and print it for the whole file: on 2026-09-02 17:32 the call side came back as 308 lines of "[ Silence ]" and one invented sentence, while the same file unprimed gave 42 real lines. The prompt is only a spelling aid, so when the run is that loop it is redone without one.
	if err == nil && prompt != "" && markerLooped(out) {
		slog.Warn("whisper looped on a silence marker under the priming prompt, transcribing again without it", "file", filepath.Base(path))
		out, errOut, err = run(ctx, bin, unprimed)
	}
	took := time.Since(started)
	// audio and rate say how long the meeting itself runs and how many times faster than real time this machine transcribes it, so a run's log line is enough to predict how long N hours of meetings will take to catch up on.
	audio := audioDuration(path)
	var rate float64
	if took > 0 {
		rate = audio.Seconds() / took.Seconds()
	}
	// Whisper's own account of the run used to be thrown away, which is why two meetings that failed outright looked like meetings nobody spoke in. Keep it wherever the run ends up.
	slog.Debug("whisper finished", "file", filepath.Base(path), "took", took, "audio", audio, "rate", rate, "stderr", strings.TrimSpace(errOut))
	if err != nil {
		return nil, fmt.Errorf("whisper %s: %w (%s)", filepath.Base(path), err, strings.TrimSpace(errOut))
	}
	return stripPromptEcho(parseSegments(out, speaker, offset), prompt), nil
}

// stripPromptEcho removes the priming prompt from the front of the transcript when whisper reads it back as speech instead of only conditioning on it. In the 2026-08-31 recording the first transcript line was "Participants: Rohit Verma, Claude Artifact." — the tail of the prompt, printed at 00:00:00 as though someone had said it, and then read by the minutes model as evidence about who was in the call.
// Whisper keeps only the last whisperMaxContext tokens of the prompt, so the echo can be any suffix of it. Sentences are stripped one at a time off the front of the first segment, and a segment left empty is dropped.
func stripPromptEcho(segs []Segment, prompt string) []Segment {
	if len(segs) == 0 || prompt == "" {
		return segs
	}
	// Whisper prints the echo as the opening of the stream, before anybody has said anything. Restricting the strip to that moment means a sentence the prompt happens to share with real speech is only ever at risk in the first seconds, rather than anywhere a segment starts.
	if segs[0].Start > promptEchoWindow {
		return segs
	}
	text := segs[0].Text
	for _, sentence := range sentenceSplit.FindAllString(prompt, -1) {
		sentence = strings.TrimSpace(sentence)
		if sentence == "" {
			continue
		}
		if trimmed := strings.TrimSpace(strings.TrimPrefix(text, sentence)); trimmed != text {
			text = trimmed
		}
	}
	if text == segs[0].Text {
		return segs
	}
	if text == "" {
		return segs[1:]
	}
	segs[0].Text = text
	return segs
}

// markerLooped reports whether whisper's output is mostly one non-speech marker repeated — more marker lines than lines with words — which is the decoder conditioning on its own "[ Silence ]" and never leaving it. Input: whisper's stdout. Output: true for the loop, false for normal output (a quiet stream still has words between its markers) and for empty output.
func markerLooped(out string) bool {
	markers, words := 0, 0
	for _, line := range strings.Split(out, "\n") {
		i := strings.Index(line, "]")
		if !strings.HasPrefix(line, "[") || i < 0 {
			continue
		}
		text := strings.TrimSpace(line[i+1:])
		if text == "" {
			continue
		}
		if oratext.NonSpeechLine.MatchString(text) {
			markers++
		} else {
			words++
		}
	}
	return markers > 0 && markers > words
}

// promptEchoWindow is how far into a stream the priming prompt may still be echoed. Whisper conditions on the prompt for its first decoding window and prints it, if at all, as the first thing it emits; past this the stream is speech.
const promptEchoWindow = 5 * time.Second

// whisperThreads is how many threads one whisper run may use. Input: whether this run decodes on the GPU. Output: the thread count, never less than one.
//
// A GPU run holds GPURun for the whole decode, so the two streams of a call run one after the other however they were started — that run has the machine to itself and takes whisper's own default of half the logical CPUs. On the CPU path the two runs really do overlap, and each takes a quarter so they add up to the same load rather than fighting over the same cores.
// An explicit $ORA_TRANSCRIBE_THREADS wins on either path, since the right number is a property of the machine.
func whisperThreads(gpu bool) int {
	if !gpu || os.Getenv("ORA_TRANSCRIBE_THREADS") != "" {
		return transcribeThreads()
	}
	if n := runtime.NumCPU() / 2; n > 0 {
		return n
	}
	return 1
}

// transcribeThreads is how many threads one background transcription run may use when it shares the machine with the other runs a meeting starts: a quarter of the logical CPUs, since whisper's own default is half and two runs at a quarter each add up to the same load. Never less than one. It is what the diarizer takes, and what a whisper run that decodes on the CPU takes.
// $ORA_TRANSCRIBE_THREADS overrides it. The right number is a property of the machine and not of the code: on a hybrid CPU the logical count is a poor guide to how many threads actually run at full speed, and the only way to know is to time a real recording both ways.
// ponytail: a quarter of the logical CPUs is a safe guess that never oversubscribes, not a tuned one. The knob is there so a machine that wants more can have it without a rebuild.
func transcribeThreads() int {
	if v := os.Getenv("ORA_TRANSCRIBE_THREADS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	if n := runtime.NumCPU() / 4; n > 0 {
		return n
	}
	return 1
}

// run executes one command and returns its stdout and stderr separately, so a tool that reports failure on stderr while exiting 0 can still be caught.
func run(ctx context.Context, name string, args []string) (stdout, stderr string, err error) {
	var out, errOut strings.Builder
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err = cmd.Run()
	return out.String(), errOut.String(), err
}

// runWithLibPath is run with libDir added to the shared-library search path, for a tool whose libraries sit beside it rather than on the system path. The sherpa-onnx build ships its own ONNX Runtime that way.
// libDir is prepended to whatever LD_LIBRARY_PATH the daemon inherited rather than replacing it: a machine running under Nix, Conda or a wrapped snap sets that variable for its own system libraries, and dropping it would leave the tool unable to link anything it does not ship itself.
func runWithLibPath(ctx context.Context, name string, args []string, libDir string) (stdout, stderr string, err error) {
	path := libDir
	if inherited := os.Getenv("LD_LIBRARY_PATH"); inherited != "" {
		path = libDir + string(os.PathListSeparator) + inherited
	}
	var out, errOut strings.Builder
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "LD_LIBRARY_PATH="+path)
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err = cmd.Run()
	return out.String(), errOut.String(), err
}

// segmentLine matches whisper's per-segment stdout line, e.g. "[00:00:07.640 --> 00:00:17.840]   text here".
var segmentLine = regexp.MustCompile(`^\[(\d+):(\d+):(\d+)\.(\d+) --> (\d+):(\d+):(\d+)\.(\d+)\]\s*(.*)$`)

// parseSegments pulls the timestamped segments out of whisper's stdout, dropping banner lines and segments with no text. Every timestamp is shifted by offset so segments from both streams share one clock.
func parseSegments(out, speaker string, offset time.Duration) []Segment {
	var segs []Segment
	for _, line := range strings.Split(out, "\n") {
		m := segmentLine.FindStringSubmatch(strings.TrimRight(line, "\r"))
		if m == nil {
			continue
		}
		text := strings.TrimSpace(m[9])
		if text == "" || oratext.NonSpeechLine.MatchString(text) {
			continue
		}
		segs = append(segs, Segment{
			Start:   hmsToDuration(m[1], m[2], m[3], m[4]) + offset,
			End:     hmsToDuration(m[5], m[6], m[7], m[8]) + offset,
			Speaker: speaker,
			Text:    text,
		})
	}
	return dropHallucinations(segs)
}

// Thresholds for the repeat rules below, all counted in one stream.
// minLoopRun is how many identical segments in a row it takes before the run is a loop rather than someone saying the same short thing twice.
// minScatteredRepeats is how many times one sentence has to appear across the whole stream before it is treated as invented.
// loopPhraseMinWords and loopPhraseMaxWords bound what counts as a loopable sentence: below the floor it is an acknowledgement people really do repeat all meeting ("Okay.", "Yeah."), above the ceiling nobody produces the same wording by chance and it is more likely a real repeated reading.
// loopShingleWords is the width of the word window suppressDriftingLoop matches on; see that function for why a loop needs this rather than an exact match.
const (
	minLoopRun          = 3
	minScatteredRepeats = 3
	loopPhraseMinWords  = 4
	loopPhraseMaxWords  = 12
	loopShingleWords    = 4
)

// dropHallucinations removes whisper's repetition artefacts from one stream's segments, which is most of what makes a transcript of a quiet call unreadable. It does four things, in order: it drops every copy of a sentence that appears minScatteredRepeats times or more across the stream, it collapses a run of minLoopRun or more identical consecutive segments down to the first, it collapses a sentence repeated minLoopRun times or more inside a single segment's text down to one copy, and it runs suppressDriftingLoop over what is left to catch the same loop when no two copies of it were byte-identical.
// The order matters: the scattered pass usually leaves the acknowledgements that were interleaved with the invented sentence sitting next to each other, and the run pass then collapses them.
func dropHallucinations(segs []Segment) []Segment {
	counts := make(map[string]int, len(segs))
	for _, s := range segs {
		counts[s.Text]++
	}

	var kept []Segment
	for _, s := range segs {
		if counts[s.Text] >= minScatteredRepeats && loopablePhrase(s.Text) {
			continue
		}
		s.Text = collapseRepeatedPhrases(s.Text)
		kept = append(kept, s)
	}

	var out []Segment
	for i := 0; i < len(kept); {
		j := i
		for j < len(kept) && kept[j].Text == kept[i].Text {
			j++
		}
		if j-i >= minLoopRun {
			out = append(out, kept[i])
		} else {
			out = append(out, kept[i:j]...)
		}
		i = j
	}
	return suppressDriftingLoop(out)
}

// suppressDriftingLoop catches the loop the exact-match rules above miss: whisper's self-conditioning does not repeat a sentence byte for byte, it slides a decode window a little further into it each time and sometimes swaps or drops a word ("55%" for "56%", "in a positive mood" for "not in a positive mood"), so no two copies of a real hallucination loop are identical. Two 2026-09 recordings show both kinds of drift, one after the other, for most of a 15-minute and a 90-minute call.
// It pools every remaining segment's words into one stream and counts each run of loopShingleWords consecutive words (lower-cased, punctuation trimmed). A word covered by a run that recurs minScatteredRepeats times or more anywhere in the stream is dropped outright — a repeat that far apart is the decoder, not someone repeating themselves that many times verbatim — and a segment left with no surviving words is dropped here, not passed back empty for the caller to filter.
// Input: segments already past the three exact-match rules. Output: the same segments with looped words removed and empty ones gone.
func suppressDriftingLoop(segs []Segment) []Segment {
	type word struct {
		segIdx int
		raw    string
		key    string
	}
	var words []word
	for i, s := range segs {
		for _, w := range strings.Fields(s.Text) {
			words = append(words, word{i, w, strings.ToLower(strings.Trim(w, ".,!?;:\"'()"))})
		}
	}
	if len(words) < loopShingleWords {
		return segs
	}

	shingle := func(i int) string {
		keys := make([]string, loopShingleWords)
		for j := range keys {
			keys[j] = words[i+j].key
		}
		return strings.Join(keys, " ")
	}
	counts := make(map[string]int, len(words))
	for i := 0; i+loopShingleWords <= len(words); i++ {
		counts[shingle(i)]++
	}

	drop := make([]bool, len(words))
	for i := 0; i+loopShingleWords <= len(words); i++ {
		if counts[shingle(i)] >= minScatteredRepeats {
			for j := i; j < i+loopShingleWords; j++ {
				drop[j] = true
			}
		}
	}

	bySeg := make(map[int][]string, len(segs))
	for i, w := range words {
		if drop[i] {
			continue
		}
		bySeg[w.segIdx] = append(bySeg[w.segIdx], w.raw)
	}

	var out []Segment
	for i, s := range segs {
		surviving, ok := bySeg[i]
		if !ok {
			continue
		}
		s.Text = strings.Join(surviving, " ")
		out = append(out, s)
	}
	return out
}

// loopablePhrase reports whether text is the shape of a sentence whisper loops on: long enough that repeating it verbatim is not something a person does, short enough to be one of the stock sentences the model falls back on when it hears nothing.
func loopablePhrase(text string) bool {
	n := len(strings.Fields(text))
	return n >= loopPhraseMinWords && n <= loopPhraseMaxWords
}

// sentenceSplit cuts a segment's text after each sentence-ending punctuation mark, keeping the mark with the sentence it ends.
var sentenceSplit = regexp.MustCompile(`[^.!?]*[.!?]+\s*|[^.!?]+$`)

// collapseRepeatedPhrases cuts a segment's text into sentences and collapses any run of minLoopRun or more identical ones down to a single copy, leaving everything around it alone. This is the same loop as the segment-level one, one level down: whisper puts "Okay." thirteen times into one segment as readily as it puts it into thirteen segments.
func collapseRepeatedPhrases(text string) string {
	parts := sentenceSplit.FindAllString(text, -1)
	if len(parts) < minLoopRun {
		return text
	}
	var kept []string
	for i := 0; i < len(parts); {
		j := i
		for j < len(parts) && strings.TrimSpace(parts[j]) == strings.TrimSpace(parts[i]) {
			j++
		}
		if j-i >= minLoopRun {
			kept = append(kept, parts[i])
		} else {
			kept = append(kept, parts[i:j]...)
		}
		i = j
	}
	// A run of fewer than minLoopRun copies is left alone, so only text that actually shrank is rewritten and a segment nobody looped comes back byte for byte.
	if len(kept) == len(parts) {
		return text
	}
	return strings.TrimSpace(strings.Join(kept, ""))
}

// hmsToDuration turns one whisper timestamp into a duration. frac is the fractional-seconds digits exactly as printed, so its length is its scale: "64" means 640ms, "640" means 640ms, and anything past three digits is finer than this cares about and is cut.
func hmsToDuration(h, m, s, frac string) time.Duration {
	n := func(v string) int { i, _ := strconv.Atoi(v); return i }
	if len(frac) > 3 {
		frac = frac[:3]
	}
	ms := n(frac)
	for i := len(frac); i < 3; i++ {
		ms *= 10
	}
	return time.Duration(n(h))*time.Hour + time.Duration(n(m))*time.Minute + time.Duration(n(s))*time.Second + time.Duration(ms)*time.Millisecond
}

// renderTranscript sorts segments from both streams into one chronological conversation and prints one paragraph per turn: "[HH:MM:SS] [me] everything they said before the other side spoke". Whisper cuts speech into a segment every few seconds, so a line per segment shreds both speakers into fragments that read as if they were interrupting each other constantly; a turn is every run of consecutive segments from the same stream, timestamped at the first of them.
func renderTranscript(segs []Segment) string {
	sort.SliceStable(segs, func(i, j int) bool { return segs[i].Start < segs[j].Start })
	var b strings.Builder
	for i := 0; i < len(segs); {
		j := i
		var turn []string
		for j < len(segs) && segs[j].Speaker == segs[i].Speaker && (j == i || !breaksTurn(segs[i:j], segs[j])) {
			turn = append(turn, segs[j].Text)
			j++
		}
		total := int(segs[i].Start.Seconds())
		fmt.Fprintf(&b, "[%02d:%02d:%02d] [%s] %s\n", total/3600, total/60%60, total%60, segs[i].Speaker, strings.Join(turn, " "))
		if j < len(segs) {
			b.WriteString("\n")
		}
		i = j
	}
	return b.String()
}

// How long a turn may run before it is broken regardless of what the audio does.
// turnSilence is the gap between two segments that counts as a pause rather than someone drawing breath. Whisper cuts on its own token timing, so consecutive segments of one person talking sit a few hundred milliseconds apart.
// maxTurn bounds a turn that neither a pause nor a speaker marker ever ends, which is the normal case on a call where people talk over each other. About as long as one person speaks uninterrupted in a standup; past that the paragraph is more likely several people than one.
const (
	turnSilence = 2 * time.Second
	maxTurn     = time.Minute
)

// breaksTurn reports whether next starts a new turn instead of continuing the one already made of turn, which is always at least one segment and always from the same speaker as next. renderTranscript prints one paragraph per turn, so this is the only thing deciding where a transcript gets its timestamps and its boundaries.
// Input: turn, the segments gathered so far, oldest first; next, the segment being considered. Output: true to end the turn before next.
func breaksTurn(turn []Segment, next Segment) bool {
	// Whisper's own speaker-change marker. It is a hint rather than a fact — it appeared seventeen times in the first five minutes of the 2026-08-31 standup and never again — but when it does appear it is the only voice-change signal the transcript carries, so it is trusted.
	if strings.HasPrefix(strings.TrimSpace(next.Text), ">>") {
		return true
	}
	// A pause. Whisper's silence segments are dropped in parseSegments, so a hole between one segment's end and the next one's start is exactly where nobody was speaking.
	if next.Start-turn[len(turn)-1].End >= turnSilence {
		return true
	}
	// The backstop, and the one that actually matters on a busy call: six people talking over each other leave no silences and no markers, so without a ceiling the whole meeting stays one paragraph.
	return next.Start-turn[0].Start >= maxTurn
}

// primingPromptBudget caps the priming prompt, in characters. Whisper keeps only the last whisperMaxContext tokens of the text it is primed with, and English averages a little over three characters per token, so a prompt longer than this has its front silently cut off — which is exactly where the participant names sit.
const primingPromptBudget = 500

// acronymPattern matches an all-capitals token — INFORM, GRDI, ASRS, ESG. These are exactly the words speech recognition mangles ("ND game" for INFORM) and exactly the words a meeting's own screens are full of.
var acronymPattern = regexp.MustCompile(`\b[A-Z][A-Z0-9]{1,5}\b`)

// properNounPattern matches a run of two or more capitalised words — "Climate Risk Studio", "Acme Essentials", "Priya Shah". One capitalised word on its own is almost always a sentence start, so the run has to be at least two.
var properNounPattern = regexp.MustCompile(`\b[A-Z][a-z]+(?: [A-Z][a-z0-9]+)+\b`)

// chatSenderPattern matches a name written the way a chat window writes it: at the start of its own line, immediately before a colon, as in "Priya Shah: ok sure ping me". The line anchor is what keeps it off the labels an app puts mid-sentence ("Industry Division: Health Care"), which look identical without it.
var chatSenderPattern = regexp.MustCompile(`(?m)^\s*([A-Z][a-z]+(?: [A-Z][a-z]+| [A-Z]{2,4})+):`)

// chromeWords is the capitalised furniture every browser, app window and meeting-call toolbar carries regardless of what the meeting is about. A candidate term or roster name is thrown away if any of its words is in here, which is what keeps "Insights Settings Private Branches" and "Datepicker All" out of a prompt about climate risk scoring, and "Mute", "Share Screen" or "Raise Hand" out of a participant list.
// ponytail: a hand-written word list, extended when a new app's furniture shows up in a prompt or a roster. The principled version would rank a word by how unusual it is for this user's screens rather than by a fixed list, which needs a corpus of episodes from outside the meeting window to compare against.
var chromeWords = map[string]bool{
	"about": true, "actions": true, "activity": true, "add": true, "admit": true, "all": true,
	"apps": true, "audio": true, "background": true, "branches": true, "breakout": true,
	"browser": true, "call": true, "camera": true, "captions": true, "chat": true, "chrome": true,
	"close": true, "code": true, "cohost": true, "commit": true, "commits": true, "contribute": true,
	"created": true, "custom": true, "datepicker": true, "edit": true, "effects": true, "end": true,
	"file": true, "filter": true, "folders": true, "gallery": true, "grid": true, "guest": true,
	"guided": true, "hand": true, "home": true, "host": true, "insights": true, "into": true,
	"invite": true, "issues": true, "latest": true, "layout": true, "leave": true, "lobby": true,
	"menu": true, "meeting": true, "microphone": true, "more": true, "mute": true, "no": true, "now": true,
	"off": true, "on": true, "open": true, "options": true, "organiser": true, "organizer": true,
	"packages": true, "participants": true, "pin": true, "polls": true, "present": true,
	"presenting": true, "private": true, "projects": true, "pull": true, "raise": true,
	"readme": true, "reactions": true, "record": true, "recording": true, "requests": true,
	"resources": true, "room": true, "rooms": true, "screen": true, "search": true, "security": true,
	"selected": true, "settings": true, "share": true, "sort": true, "speaker": true, "spotlight": true,
	"start": true, "stop": true, "tab": true, "tags": true, "tour": true, "turn": true, "unmute": true,
	"video": true, "view": true, "waiting": true, "whiteboard": true, "wiki": true, "you": true,
}

// isMeetingWindow reports whether an episode was captured from the window of a call rather than from whatever else was on screen. Everything that claims to name a participant is checked against this first: a screen during a meeting is mostly not the meeting. The tracker owns the test, since it is the same one it uses to decide which window to capture on its own clock.
func isMeetingWindow(app, title string) bool {
	return tracker.IsMeetingWindow(app, title)
}

// personNamesFromContext turns the store's person subjects into names whisper can be primed with. Input: the personal-context entries. Output: one display name per person subject, hyphens to spaces and each word capitalised, in store order; the identity entry and preference entries are skipped.
func personNamesFromContext(entries []db.PersonalEntry) []string {
	var names []string
	for _, e := range entries {
		if !db.IsPersonSubject(e.Subject) {
			continue
		}
		names = append(names, db.PersonSubjectName(e.Subject))
	}
	return names
}

// primingPromptFor is primingPrompt with the people the store already knows added after the screen's own terms, within the same budget. Whisper spells a name the way it is primed to: on 2026-09-03 a call primed only with "Terms: API." wrote "Ashar" for Sneha, and that guess became a person in memory. People go last because the prompt is cut from the front when it is too long.
func primingPromptFor(eps []db.Episode, people []string) string {
	base := primingPromptBody(eps)
	if len(people) == 0 {
		return base
	}
	var b strings.Builder
	b.WriteString(strings.TrimSuffix(base, "."))
	written := 0
	for _, name := range people {
		if b.Len()+len(name)+len(" People: ")+2 > primingPromptBudget {
			break
		}
		if written == 0 {
			b.WriteString(". People: ")
		} else {
			b.WriteString(", ")
		}
		b.WriteString(name)
		written++
	}
	b.WriteString(".")
	return b.String()
}

func primingPromptBody(eps []db.Episode) string {
	counts := map[string]int{}
	first := map[string]string{} // lower-cased term to the spelling it was first seen with
	var senders []string
	seenSender := map[string]bool{}
	add := func(term string) {
		key := strings.ToLower(term)
		if hasRepeatedWord(term) || hasChromeWord(term) {
			return
		}
		if _, ok := first[key]; !ok {
			first[key] = term
		}
		counts[key]++
	}

	// A name written immediately before a colon is how a chat window labels who typed something. Only the meeting's own window counts: on 2026-08-31 a WhatsApp tab open during a standup put "Rohit Verma" and "Claude Artifact" into the prompt as the meeting's participants, and whisper printed them back as the first line of the transcript.
	for _, e := range eps {
		inMeeting := isMeetingWindow(e.App, e.Title)
		for _, text := range []string{e.Title, e.UserActivity, e.ScreenText, e.VisibleText} {
			if !inMeeting {
				continue
			}
			for _, m := range chatSenderPattern.FindAllStringSubmatch(text, -1) {
				name := m[1]
				if !seenSender[name] {
					seenSender[name] = true
					senders = append(senders, name)
				}
			}
		}
	}

	for _, e := range eps {
		for _, text := range []string{e.Title, e.UserActivity, e.ScreenText, e.VisibleText} {
			for _, m := range acronymPattern.FindAllString(text, -1) {
				add(m)
			}
			for _, m := range properNounPattern.FindAllString(text, -1) {
				add(m)
			}
		}
	}

	terms := make([]string, 0, len(counts))
	for key := range counts {
		terms = append(terms, key)
	}
	// Most-mentioned first, and alphabetically within a count, so the same meeting always produces the same prompt.
	sort.Slice(terms, func(i, j int) bool {
		if counts[terms[i]] != counts[terms[j]] {
			return counts[terms[i]] > counts[terms[j]]
		}
		return terms[i] < terms[j]
	})

	// Terms come before participants and participants come last: whisper keeps only the last whisperMaxContext tokens of this prompt, so whatever sits at the front is what gets cut when the prompt runs long. Names matter more than terms for getting the transcript right, so they go where the truncation can't reach them.
	var b strings.Builder
	b.WriteString("Meeting notes.")

	written := 0
	if len(terms) > 0 {
		b.WriteString(" Terms:")
		for _, key := range terms {
			term := first[key]
			if seenSender[term] {
				continue
			}
			// Three characters are left over for the separator and the closing full stop, which is what keeps whisper writing in sentences rather than copying a bare word list.
			if b.Len()+len(term)+3 > primingPromptBudget {
				break
			}
			if written > 0 {
				b.WriteString(",")
			}
			b.WriteString(" " + term)
			written++
		}
		if written == 0 {
			s := strings.TrimSuffix(b.String(), " Terms:")
			b.Reset()
			b.WriteString(s)
		} else {
			b.WriteString(".")
		}
	}

	if len(senders) == 0 {
		if written == 0 {
			return ""
		}
		return b.String()
	}
	b.WriteString(" Participants:")
	named := 0
	for _, name := range senders {
		// Same three characters of slack as the terms loop, for the separator and the closing full stop.
		if b.Len()+len(name)+3 > primingPromptBudget {
			break
		}
		if named > 0 {
			b.WriteString(",")
		}
		b.WriteString(" " + name)
		named++
	}
	if named == 0 {
		return strings.TrimSuffix(b.String(), " Participants:")
	}
	b.WriteString(".")
	return b.String()
}

// hasChromeWord reports whether any word of a candidate term is app furniture rather than something the meeting is about.
func hasChromeWord(term string) bool {
	for _, w := range strings.Fields(term) {
		if chromeWords[strings.ToLower(w)] {
			return true
		}
	}
	return false
}

// hasRepeatedWord reports whether a phrase says the same word twice in a row, which is what a screen reader produces out of a repeated label ("Search Search", "More More Home") and never out of a real name or term.
func hasRepeatedWord(term string) bool {
	words := strings.Fields(term)
	for i := 1; i < len(words); i++ {
		if words[i] == words[i-1] {
			return true
		}
	}
	return false
}
