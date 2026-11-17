package recorder

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"june/internal/config"
	"june/internal/obs"
)

// The diarizer is a sherpa-onnx build installed the same way the whisper.cpp one is: the binary, its shared libraries and its two models together under <data dir>/sherpa. It splits the call side of a recording into voices; it never sees the microphone side, which is one known person by construction.
const (
	sherpaBinaryName   = "sherpa-onnx-offline-speaker-diarization"
	sherpaSegmentation = "segmentation-3.0.onnx"
	sherpaEmbedding    = "wespeaker_en_voxceleb_CAM++.onnx"
)

// defaultClusterThreshold is the cosine distance at which two stretches of speech stop being the same voice. It is only ever used when the number of people in the call is unknown, because as a way of getting the speaker count right it does not work: on the 2026-08-31 16:17 recording, a call with exactly one other person in it, thresholds of 0.85, 0.9 and 0.95 all returned four voices, while on a six-person standup the same range swung the answer from 35 down to 6. There is no value that transfers between two meetings, so tuning it is not a path to a correct answer, only to a differently wrong one.
// What does transfer is being told the number outright: given --clustering.num-clusters the diarizer returns exactly that many, so the count is right by construction rather than by calibration. June can read that count off the meeting's own window, which is why this constant is the fallback and not the mechanism.
const defaultClusterThreshold = 0.8

// minClusterSpeech is the least total speech a voice needs to count as a person when the diarizer had to estimate how many there are. A voice with less than this is almost always one person's voice split in two, or a cough or a laugh, and foldSmallClusters merges it into a neighbour.
const minClusterSpeech = 15 * time.Second

// clusterThreshold returns the configured threshold, or the default when none is set.
func clusterThreshold() string {
	t := config.LoadConfig().Transcribe.ClusterThreshold
	if t <= 0 {
		t = defaultClusterThreshold
	}
	return strconv.FormatFloat(t, 'f', -1, 64)
}

// SherpaBinary returns the path to the diarizer, or an error naming the exact path of each missing file. $JUNE_SHERPA overrides the location.
func SherpaBinary(dataDir string) (string, error) {
	bin := filepath.Join(dataDir, "sherpa", sherpaBinaryName)
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if p := os.Getenv("JUNE_SHERPA"); p != "" {
		bin = p
	}
	models := []string{filepath.Join(filepath.Dir(bin), sherpaSegmentation), filepath.Join(filepath.Dir(bin), sherpaEmbedding)}
	if info, err := os.Stat(bin); err != nil || info.IsDir() {
		return "", fmt.Errorf("no diarizer at %s, and it needs %s beside it", bin, strings.Join(models, " and "))
	}
	for _, m := range models {
		if _, err := os.Stat(m); err != nil {
			return "", fmt.Errorf("no %s at %s, beside the diarizer", filepath.Base(m), m)
		}
	}
	return bin, nil
}

// diarTurnLine matches one line of the diarizer's output, e.g. "0.518 -- 6.032 speaker_01".
var diarTurnLine = regexp.MustCompile(`([0-9.]+)\s*--\s*([0-9.]+)\s+speaker_([0-9]+)`)

// parseDiarTurns pulls the speaker turns out of the diarizer's stdout, ignoring the banner it prints around them.
func parseDiarTurns(out string) []diarTurn {
	var turns []diarTurn
	for _, line := range strings.Split(out, "\n") {
		m := diarTurnLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		start, err1 := strconv.ParseFloat(m[1], 64)
		end, err2 := strconv.ParseFloat(m[2], 64)
		speaker, err3 := strconv.Atoi(m[3])
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		turns = append(turns, diarTurn{
			Start:   time.Duration(start * float64(time.Second)),
			End:     time.Duration(end * float64(time.Second)),
			Speaker: speaker,
		})
	}
	return turns
}

// diarizeWAV runs the diarizer over one WAV and returns the voice turns it found, on the file's own clock.
// speakers is how many voices the caller knows are in the call, from the meeting app's own window; the diarizer then returns exactly that many rather than estimating. Pass 0 when it is not known, and the distance threshold decides instead — which gets the count wrong more often than not.
// offset shifts the turns onto the recording's clock, the same way transcribeWAV shifts its segments.
func diarizeWAV(ctx context.Context, bin, path string, speakers int, offset time.Duration) ([]diarTurn, error) {
	ctx, span := obs.StartSpan(ctx, "Recorder.Sherpa")
	defer span.End()
	// A relative binary or library folder would be looked for under wd once the child starts there.
	if abs, err := filepath.Abs(bin); err == nil {
		bin = abs
	}
	dir := filepath.Dir(bin)
	// The models and the WAV go over the way whisper-cli's do (see childPaths): a data directory under a user name outside ASCII put a path the diarizer could not open into every run, and every remote voice stayed pooled under one label.
	wd, paths := childPaths([]string{filepath.Join(dir, sherpaSegmentation), filepath.Join(dir, sherpaEmbedding), path})
	args := []string{
		"--segmentation.pyannote-model=" + paths[0],
		"--embedding.model=" + paths[1],
		"--segmentation.num-threads=" + strconv.Itoa(transcribeThreads()),
		"--embedding.num-threads=" + strconv.Itoa(transcribeThreads()),
	}
	if speakers > 0 {
		args = append(args, "--clustering.num-clusters="+strconv.Itoa(speakers))
	} else {
		args = append(args, "--clustering.cluster-threshold="+clusterThreshold())
	}
	args = append(args, paths[2])
	out, errOut, err := runWithLibPath(ctx, wd, bin, args, filepath.Join(dir, "lib"))
	if err != nil {
		return nil, fmt.Errorf("diarize %s: %w (%s)", filepath.Base(path), err, strings.TrimSpace(errOut))
	}
	turns := parseDiarTurns(out)
	// A count the caller gave is exact by construction, so only an estimated one is cleaned up.
	if speakers <= 0 {
		turns = foldSmallClusters(turns)
	}
	for i := range turns {
		turns[i].Start += offset
		turns[i].End += offset
	}
	return turns, nil
}

// diarTurn is one span of the call's audio attributed to one clustered voice. Speaker is the cluster number the diarizer assigned, which is stable within a recording and means nothing across recordings — cluster 2 in this meeting is not cluster 2 in the next one.
type diarTurn struct {
	Start, End time.Duration
	Speaker    int
}

// foldSmallClusters gives every turn of a voice with under minClusterSpeech of speech in total to the voice whose nearest turn in time is closest, among the voices with at least that much. Turns that overlap in time count as zero apart. The turns keep their times and order, only their Speaker changes.
// When no voice reaches the minimum, as in a very short call, the turns are returned as they were, since there is nothing larger to fold into. The diarizer prints only turns, with no voice embeddings to measure distance with, so closeness in time is what decides.
func foldSmallClusters(turns []diarTurn) []diarTurn {
	total := map[int]time.Duration{}
	for _, t := range turns {
		total[t.Speaker] += t.End - t.Start
	}
	var large []diarTurn
	for _, t := range turns {
		if total[t.Speaker] >= minClusterSpeech {
			large = append(large, t)
		}
	}
	if len(large) == 0 {
		return turns
	}
	out := make([]diarTurn, len(turns))
	for i, t := range turns {
		out[i] = t
		if total[t.Speaker] >= minClusterSpeech {
			continue
		}
		nearest := time.Duration(math.MaxInt64)
		for _, l := range large {
			if gap := timeGap(t, l); gap < nearest {
				nearest, out[i].Speaker = gap, l.Speaker
			}
		}
	}
	return out
}

// timeGap is how long a stretch of silence lies between two turns, zero when they touch or overlap.
func timeGap(a, b diarTurn) time.Duration {
	return max(a.Start-b.End, b.Start-a.End, 0)
}

// callSpeaker is the label for one clustered voice on the call side, replacing the pooled speakerCall. Numbering starts at one because these are read by a human and by a model, neither of which expects a zeroth speaker.
func callSpeaker(cluster int) string {
	return fmt.Sprintf("%s:S%d", speakerCall, cluster+1)
}

// voiceSpan is one stretch of a call segment spoken by one voice.
type voiceSpan struct {
	start, end time.Duration
	speaker    int
}

func (v voiceSpan) length() time.Duration { return v.end - v.start }

// sentence is the words of one sentence from the call side, the times they were spoken between, and how long each voice spoke during them. A sentence that whisper's segment boundary cut in two is one sentence again here.
type sentence struct {
	start, end time.Duration
	words      []string
	heard      map[int]time.Duration
}

// finished reports whether the sentence ends the way a sentence does, rather than where a segment boundary cut it.
func (s sentence) finished() bool {
	return len(s.words) == 0 || endsSentence(s.words[len(s.words)-1])
}

// speaker is the label of the voice that spoke most of the sentence, the lower cluster number on a tie, or the pooled label when no turn touches it.
func (s sentence) speaker() string {
	best, bestTime := -1, time.Duration(0)
	for voice, d := range s.heard {
		if d > bestTime || (d == bestTime && voice < best) {
			best, bestTime = voice, d
		}
	}
	if best < 0 {
		return speakerCall
	}
	return callSpeaker(best)
}

// assignSpeakers relabels the call side with the voice clusters that spoke it, leaving the microphone side alone — that stream is one known person by construction and there is nothing to work out.
// The label is chosen per sentence, not per whisper segment. Whisper cuts on its own timing, into segments of up to half a minute that can hold several people and can end halfway through a sentence, and giving such a segment one label put one person's words under another's name. A sentence cut by a segment boundary is put back together first, so a person never changes in the middle of one.
// A sentence that overlaps no turn at all keeps the pooled speakerCall label: the diarizer discards speech shorter than its own floor, and a stretch it never covered is one it has no opinion about, which is different from one it placed.
// Input: the transcript's segments and the diarizer's turns, both on the recording's clock. Output: the segments in time order, the call side as one segment per sentence and named per cluster.
func assignSpeakers(segs []Segment, turns []diarTurn) []Segment {
	var out []Segment
	var pending []sentence
	flush := func() {
		for _, s := range pending {
			out = append(out, Segment{Start: s.start, End: s.end, Speaker: s.speaker(), Text: strings.Join(s.words, " ")})
		}
		pending = nil
	}
	for _, seg := range segs {
		if seg.Speaker != speakerCall {
			flush()
			out = append(out, seg)
			continue
		}
		for i, s := range sentencesIn(seg, turns) {
			if n := len(pending); i == 0 && n > 0 && !pending[n-1].finished() {
				pending[n-1].end = s.end
				pending[n-1].words = append(pending[n-1].words, s.words...)
				for voice, d := range s.heard {
					pending[n-1].heard[voice] += d
				}
				continue
			}
			pending = append(pending, s)
		}
	}
	flush()
	return out
}

// sentencesIn cuts one call segment into its sentences. Whisper prints no word times, so the words are spread evenly over the time somebody was speaking in the segment, which leaves out the silence its timestamps stretch across, and each sentence is credited with the speech of each voice that falls in its share.
// A segment no turn touches comes back as a single sentence with nobody heard in it.
func sentencesIn(seg Segment, turns []diarTurn) []sentence {
	words := strings.Fields(seg.Text)
	spans := voiceSpans(seg, turns)
	var total time.Duration
	for _, sp := range spans {
		total += sp.length()
	}
	if len(words) == 0 || total == 0 {
		return []sentence{{start: seg.Start, end: seg.End, words: words, heard: map[int]time.Duration{}}}
	}
	bounds := append(append([]int{0}, sentenceEnds(words)...), len(words))
	var out []sentence
	for k := 0; k+1 < len(bounds); k++ {
		first, last := bounds[k], bounds[k+1]
		from := time.Duration(float64(total) * float64(first) / float64(len(words)))
		to := time.Duration(float64(total) * float64(last) / float64(len(words)))
		s := sentence{start: wallTime(spans, from), end: wallTime(spans, to), words: words[first:last], heard: map[int]time.Duration{}}
		var offset time.Duration
		for _, sp := range spans {
			if d := min(to, offset+sp.length()) - max(from, offset); d > 0 {
				s.heard[sp.speaker] += d
			}
			offset += sp.length()
		}
		out = append(out, s)
	}
	// The sentences together cover what the whole segment did.
	out[0].start = seg.Start
	out[len(out)-1].end = seg.End
	return out
}

// voiceSpans lists who spoke inside a segment, in time order and without overlap: the turns cut to the segment's times, the earlier turn keeping any time two of them share.
func voiceSpans(seg Segment, turns []diarTurn) []voiceSpan {
	var pieces []voiceSpan
	for _, t := range turns {
		if p := (voiceSpan{max(t.Start, seg.Start), min(t.End, seg.End), t.Speaker}); p.length() > 0 {
			pieces = append(pieces, p)
		}
	}
	sort.SliceStable(pieces, func(i, j int) bool { return pieces[i].start < pieces[j].start })
	var spans []voiceSpan
	var at time.Duration
	for _, p := range pieces {
		p.start = max(p.start, at)
		if p.length() > 0 {
			spans = append(spans, p)
			at = p.end
		}
	}
	return spans
}

// wallTime turns an amount of speaking time into the clock time it is reached at, counting only the time inside the spans. offset must not be negative.
func wallTime(spans []voiceSpan, offset time.Duration) time.Duration {
	for _, sp := range spans {
		if offset <= sp.length() {
			return sp.start + offset
		}
		offset -= sp.length()
	}
	return spans[len(spans)-1].end
}

// sentenceEnds returns every position j from 1 to len(words)-1 where words[j-1] ends a sentence, so words[:j] is a whole number of sentences.
func sentenceEnds(words []string) []int {
	var ends []int
	for j := 1; j < len(words); j++ {
		if endsSentence(words[j-1]) {
			ends = append(ends, j)
		}
	}
	return ends
}

// endsSentence reports whether a word finishes a sentence: a full stop, question mark, exclamation mark or danda, possibly followed by a closing quote or bracket.
func endsSentence(word string) bool {
	w := []rune(strings.TrimRight(word, "\"')”’"))
	return len(w) > 0 && strings.ContainsRune(".?!…।", w[len(w)-1])
}
