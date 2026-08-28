package recorder

import (
	"context"
	"errors"
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
	"syscall"
	"time"

	"ora/internal/db"
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

// whisperCandidates are the whisperfile names looked for under <data dir>/whisper, best first. These are single-file executables from Mozilla's whisperfile release with the weights baked in, so there is no separate model file to point at.
// The multilingual builds come first because the meetings being recorded are Hindi-English code-switched, and an .en build hears that as nothing at all — a 16-minute call came back with no speech in it. The multilingual models detect the language themselves and transcribe it as spoken, so no language flag is passed and --translate is never used.
var whisperCandidates = []string{
	"whisper-medium.llamafile",
	"whisper-small.llamafile",
	"whisper-medium.en.llamafile",
	"whisper-small.en.llamafile",
	"whisper-tiny.en.llamafile",
}

// whisperBinary returns the path to the whisper executable to run: $ORA_WHISPER if set, otherwise the best whisperCandidates entry present under dataDir/whisper.
func whisperBinary(dataDir string) (string, error) {
	if p := os.Getenv("ORA_WHISPER"); p != "" {
		return p, nil
	}
	dir := filepath.Join(dataDir, "whisper")
	for _, name := range whisperCandidates {
		p := filepath.Join(dir, name)
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("no whisperfile found in %s (expected one of %s) and $ORA_WHISPER is unset", dir, strings.Join(whisperCandidates, ", "))
}

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
	args := []string{"-f", path, "-np", "-et", entropyThreshold, "-lpt", logProbThreshold, "-mc", whisperMaxContext, "-t", strconv.Itoa(transcribeThreads())}
	if prompt != "" {
		args = append(args, "--prompt", prompt)
	}
	// A whisper.cpp build takes two flags a whisperfile does not, and decodes on the GPU, where only one run fits at a time.
	if extra := whisperCPPArgs(bin); extra != nil {
		args = append(args, extra...)
		gpuRun.Lock()
		defer gpuRun.Unlock()
	}
	started := time.Now()
	out, errOut, err := run(ctx, bin, args)
	if errors.Is(err, syscall.ENOEXEC) {
		// A whisperfile is an APE binary: the kernel refuses it without a binfmt_misc registration, but its header doubles as a shell script that loads the real executable. A plain ELF whisper build takes the direct path above instead.
		started = time.Now()
		out, errOut, err = run(ctx, "/bin/sh", append([]string{bin}, args...))
	}
	took := time.Since(started)
	// Whisper's own account of the run used to be thrown away, which is why two meetings that failed outright looked like meetings nobody spoke in. Keep it wherever the run ends up.
	slog.Debug("whisper finished", "file", filepath.Base(path), "took", took, "stderr", strings.TrimSpace(errOut))
	if err != nil {
		return nil, fmt.Errorf("whisper %s: %w (%s)", filepath.Base(path), err, strings.TrimSpace(errOut))
	}
	// whisperfile exits 0 whether or not it managed to decode the audio, so a file it could not read is indistinguishable from a meeting nobody spoke in unless its stderr is read.
	if strings.Contains(errOut, "failed to read") {
		return nil, fmt.Errorf("whisper could not read %s: %s", filepath.Base(path), strings.TrimSpace(lineContaining(errOut, "failed to read")))
	}

	segs := parseSegments(out, speaker, offset)
	// Whisper runs at a few times real time, so a long recording that came back with nothing in a moment was never listened to: whatever went wrong, it exited 0 without saying so. Treating that as a silent meeting is what kept the audio's real contents hidden.
	if audio := wavDuration(path); len(segs) == 0 && audio >= minAudioForSpeedCheck && took < audio/whisperSlowestPlausibleSpeed {
		slog.Warn("whisper produced nothing far too quickly to have transcribed the audio", "file", filepath.Base(path), "audio", audio, "took", took, "stderr", strings.TrimSpace(errOut))
		return nil, fmt.Errorf("whisper returned no speech for %s after %s of %s of audio, which is too fast to be a transcription: %s", filepath.Base(path), took.Round(time.Millisecond), audio.Round(time.Second), strings.TrimSpace(errOut))
	}
	return segs, nil
}

// whisperSlowestPlausibleSpeed is the slowest ratio of audio length to run time that still counts as a real transcription. small on this CPU runs around four times real time, so a run twenty times faster than the audio it was given did not decode it.
const whisperSlowestPlausibleSpeed = 20

// transcribeThreads is how many threads one transcription run may use. The two sides of a call are transcribed at the same time, so this is deliberately half of what a single run would take: whisper's own default is half the logical CPUs, and two runs at a quarter each add up to the same load rather than fighting over the same cores. Never less than one.
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

// minAudioForSpeedCheck is the shortest recording the speed check applies to. A few seconds of audio genuinely does transcribe in a blink, and a fixed process startup cost swamps the ratio there.
const minAudioForSpeedCheck = 30 * time.Second

// wavDuration returns how much 16 kHz mono 16-bit audio a WAV file holds, from its size. Returns zero if the file cannot be measured.
func wavDuration(path string) time.Duration {
	info, err := os.Stat(path)
	if err != nil || info.Size() <= wavHeaderSize {
		return 0
	}
	frames := (info.Size() - wavHeaderSize) / 2
	return time.Duration(frames) * time.Second / sampleRate
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

// lineContaining returns the first line of s that contains want, so an error message can quote the complaint rather than the whole model-loading banner.
func lineContaining(s, want string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, want) {
			return line
		}
	}
	return s
}

// segmentLine matches whisperfile's per-segment stdout line, e.g. "[00:00:07.640 --> 00:00:17.840]   text here".
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
		if text == "" || nonSpeech.MatchString(text) {
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
const (
	minLoopRun          = 3
	minScatteredRepeats = 3
	loopPhraseMinWords  = 4
	loopPhraseMaxWords  = 12
)

// dropHallucinations removes whisper's repetition artefacts from one stream's segments, which is most of what makes a transcript of a quiet call unreadable. It does three things, in order, and nothing else: it drops every copy of a sentence that appears minScatteredRepeats times or more across the stream, it collapses a run of minLoopRun or more identical consecutive segments down to the first, and it collapses a sentence repeated minLoopRun times or more inside a single segment's text down to one copy.
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

// nonSpeech matches whisper's markers for a stretch with no words in it — "[BLANK_AUDIO]", "(upbeat music)", "[SOUND]". A silent stream is otherwise nothing but these, and the microphone side of a call is silent most of the time.
var nonSpeech = regexp.MustCompile(`^[\[(][^)\]]*[)\]]$`)

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
		for j < len(segs) && segs[j].Speaker == segs[i].Speaker {
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

// primingPromptBudget caps the priming prompt, in characters. Whisper keeps only the last whisperMaxContext tokens of the text it is primed with, and English averages a little over three characters per token, so a prompt longer than this has its front silently cut off — which is exactly where the participant names sit.
const primingPromptBudget = 500

// acronymPattern matches an all-capitals token — INFORM, GRDI, ASRS, ESG. These are exactly the words speech recognition mangles ("ND game" for INFORM) and exactly the words a meeting's own screens are full of.
var acronymPattern = regexp.MustCompile(`\b[A-Z][A-Z0-9]{1,5}\b`)

// properNounPattern matches a run of two or more capitalised words — "Climate Risk Studio", "Acme Essentials", "Priya Shah". One capitalised word on its own is almost always a sentence start, so the run has to be at least two.
var properNounPattern = regexp.MustCompile(`\b[A-Z][a-z]+(?: [A-Z][a-z0-9]+)+\b`)

// chatSenderPattern matches a name written the way a chat window writes it: at the start of its own line, immediately before a colon, as in "Priya Shah: ok sure ping me". The line anchor is what keeps it off the labels an app puts mid-sentence ("Industry Division: Health Care"), which look identical without it.
var chatSenderPattern = regexp.MustCompile(`(?m)^\s*([A-Z][a-z]+(?: [A-Z][a-z]+| [A-Z]{2,4})+):`)

// promptChrome is the capitalised furniture every browser and app window carries regardless of what the meeting is about. A candidate term is thrown away if any of its words is in here, which is what keeps "Insights Settings Private Branches" and "Datepicker All" out of a prompt that is supposed to be about climate risk scoring.
// ponytail: a hand-written word list, extended when a new app's furniture shows up in a prompt. The principled version would rank a term by how unusual it is for this user's screens rather than by a fixed list, which needs a corpus of episodes from outside the meeting window to compare against.
var promptChrome = map[string]bool{
	"about": true, "actions": true, "activity": true, "add": true, "all": true, "branches": true,
	"browser": true, "chrome": true, "close": true, "code": true, "commit": true, "commits": true,
	"contribute": true, "created": true, "custom": true, "datepicker": true, "edit": true,
	"file": true, "filter": true, "folders": true, "guided": true, "home": true, "insights": true,
	"into": true, "issues": true, "latest": true, "menu": true, "more": true, "no": true,
	"open": true, "packages": true, "private": true, "projects": true, "pull": true,
	"readme": true, "requests": true, "resources": true, "search": true, "security": true,
	"selected": true, "settings": true, "share": true, "sort": true, "tab": true, "tags": true,
	"tour": true, "turn": true, "view": true, "wiki": true,
}

// primingPrompt builds the text whisper is primed with, out of what the desktop tracker recorded on screen while the meeting ran. Priming biases whisper's spelling towards the words in the prompt, so feeding it the meeting's own acronyms and proper nouns is what turns "ND game and GRDI" into "INFORM and GDIS".
// The prompt is deliberately written as capitalised, punctuated English. Whisper continues the prompt's register as well as its vocabulary: primed with a raw lowercase chat log it returns the whole transcript lowercase and unpunctuated, which is worse to read and worse to summarise from.
// Input: the episodes captured during the recording window. Output: one line of text, capped at primingPromptBudget characters, or "" when there was nothing on screen to learn from.
func primingPrompt(eps []db.Episode) string {
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

	for _, e := range eps {
		for _, text := range []string{e.Title, e.UserActivity, e.ScreenText, e.VisibleText} {
			for _, m := range acronymPattern.FindAllString(text, -1) {
				add(m)
			}
			for _, m := range properNounPattern.FindAllString(text, -1) {
				add(m)
			}
			// A name written immediately before a colon is how a chat window labels who typed something, which is the one place on screen that names the people in the meeting rather than the software.
			for _, m := range chatSenderPattern.FindAllStringSubmatch(text, -1) {
				if name := m[1]; !seenSender[name] {
					seenSender[name] = true
					senders = append(senders, name)
				}
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

	var b strings.Builder
	b.WriteString("Meeting notes.")
	// The names are bounded by the same budget as everything else: a screen full of chat history can name more people than the prompt has room for, and a prompt whose front is cut off loses the framing sentence that keeps whisper writing in sentences.
	named := 0
	for _, name := range senders {
		if b.Len()+len(name)+18 > primingPromptBudget {
			break
		}
		if named == 0 {
			b.WriteString(" Participants: " + name)
		} else {
			b.WriteString(", " + name)
		}
		named++
	}
	if named > 0 {
		b.WriteString(".")
	}
	if len(terms) == 0 {
		if named == 0 {
			return ""
		}
		return b.String()
	}
	b.WriteString(" Terms:")
	written := 0
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
		return strings.TrimSuffix(b.String(), " Terms:")
	}
	b.WriteString(".")
	return b.String()
}

// hasChromeWord reports whether any word of a candidate term is app furniture rather than something the meeting is about.
func hasChromeWord(term string) bool {
	for _, w := range strings.Fields(term) {
		if promptChrome[strings.ToLower(w)] {
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
