// Package recorder records a meeting from the two sides of the machine's audio — the microphone and the default sink's monitor — then transcribes both locally with whisper and turns the interleaved transcript into meeting minutes.
package recorder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"june/internal/audio"
	"june/internal/brain"
	"june/internal/config"
	"june/internal/db"
	"june/internal/memory"
	"june/internal/util"
)

// noteKind is the notes.kind written for a meeting, so minutes are distinguishable from the memory compiler's facts.
const noteKind = "meeting"

// transcribeTimeout bounds one whisper run. small.en runs several times faster than real time on CPU, so two hours covers a very long meeting with room to spare.
const transcribeTimeout = 2 * time.Hour

// episodeLimit is how many raw episode rows a meeting's window may fetch. It is deliberately generous, because the rows are deduplicated after they arrive and most of them are repeats: a 39-minute meeting produced 27 rows that collapsed to 7 distinct screens. Capping the fetch tightly spends the budget on duplicates and then throws away the end of the meeting — at the old value of 200, a four-hour call had its last three hours dropped before the model saw any of it, and the minutes read as though it had ended at lunchtime.
// A four-hour meeting at the tracker's poll rate is a few hundred rows even before deduplication, and each one's text is capped again by screenTextBudget, so the fetch is bounded well below anything a prompt would notice.
const episodeLimit = 3000

// timelineEntries is how many distinct screens the prompt may carry, applied after deduplication rather than before it. When a meeting has more than this, the entries are sampled evenly across its whole length instead of truncated, because the end of a meeting is where the decisions are.
const timelineEntries = 120

// Store is the slice of *db.Store the recorder needs: the desktop timeline captured while the meeting ran, the personal context that says who the [me] speaker is (and which a finished meeting can add a person to), somewhere to file the minutes, and — for meeting prep — every note filed so far to search for one about the people or meeting on screen right now.
type Store interface {
	EpisodesInWindow(ctx context.Context, since, until time.Time, limit int) ([]db.Episode, error)
	// DistinctTitles backs the judgement of which words in a meeting's window title identify the meeting and which are the browser's own furniture.
	DistinctTitles(ctx context.Context, limit int) ([]string, error)
	PersonalContext(ctx context.Context) ([]db.PersonalEntry, error)
	SetPersonalContext(ctx context.Context, subject, content string) error
	LogNote(ctx context.Context, content, kind string) (int64, error)
	UpdateNote(ctx context.Context, id int64, content string) error
	GetNotes(ctx context.Context) ([]db.Note, error)
	// AddActionItems files the things people agreed to do in this meeting as their own rows, skipping any already on file.
	AddActionItems(ctx context.Context, items []memory.ActionItem) (int, error)
	// CloseDoneActionItems closes the open tasks this meeting's own minutes say are finished.
	CloseDoneActionItems(ctx context.Context, since time.Time) (int, error)
	// CreateConversation and AddTurn file a meeting prep as a conversation of June's own, so the whole brief can be read and answered in the window rather than cut off on a card.
	CreateConversation(ctx context.Context, title, brain string) (int64, error)
	AddTurn(ctx context.Context, conversationID int64, role, text, kind string, evidence json.RawMessage, tools []string) (int64, error)
}

// Recorder owns at most one meeting recording at a time. Start and StopAndProcess are what the tray calls; everything after the stop runs in the background.
type Recorder struct {
	// mainBrain is the brain minutes are written with, installed by the daemon through SetBrain so a meeting write-up spends from the same shared Gemini daily quota as everything else the daemon meters; nil means defaultBrain builds its own routed one from config, per meeting.
	mainBrain brain.Brain
	// mainBrainMu guards mainBrain, which the daemon may install while a meeting is already being written up.
	mainBrainMu sync.RWMutex
	// stateObservers are called after a recording starts or stops. The tray menu registers its redraw here, because the label reading "Start meeting recording" is wrong the moment anything other than the tray itself starts one — and since June began offering to record when it notices a call, that is the common case rather than a corner of it. The daemon registers the "recording" announce the window draws from here too, which is why it is a list: a single slot let the tray's registration replace the daemon's, and the window never heard a recording start.
	stateObservers []func()
	// observersMu guards stateObservers, which the tray adds to from its own goroutine while the meeting watcher may already be starting a recording. It is not mu, because the observers themselves call Active, which takes mu.
	observersMu sync.Mutex

	dataDir string
	store   Store
	apiKey  string

	mu   sync.Mutex
	live *session

	// inFlight holds the recording directories being processed right now, so the stop path and the retry loop can never transcribe the same recording twice.
	inFlight map[string]bool

	// swept is closed once the startup sweep for unfinished recordings has finished. Only tests wait on it.
	swept chan struct{}

	// silenceAfter is how long the system stream may stay silent before the user is warned. Tests shorten it.
	silenceAfter time.Duration

	// retryEvery is how often a deferred recording is checked on. Tests do not rely on it, driving pickup directly instead.
	retryEvery time.Duration

	// prepTimeout bounds the whole meeting-prep flow that Start fires off, the brain call included. Tests shorten it to check that a slow prep is dropped rather than delivered late.
	prepTimeout time.Duration

	// Seams, all set by New and replaced in tests: opening the sound streams, running whisper, finding the whisper binary, calling Gemini, posting a desktop notification, and asking whether the machine is plugged in.
	capture     func(mic, system io.Writer) (capturer, time.Time, time.Time, error)
	whisper     func(ctx context.Context, bin, path, speaker, prompt string, offset time.Duration) ([]Segment, error)
	findWhisper func(dataDir string) (string, error)
	diarize     func(ctx context.Context, bin, path string, speakers int, offset time.Duration) ([]diarTurn, error)
	findSherpa  func(dataDir string) (string, error)
	minutes     func(ctx context.Context, prompt string) (string, error)
	notify      func(title, body string)
	// notifyAt posts a notice that opens somewhere in the window: the meeting prep, which opens the conversation it was filed as, and the "Meeting summary ready" notice, which opens the meeting in Meetings by the id its minutes were filed under. Input: title, body, and the place and row id the card's Open goes to, both "" for a notice that opens nothing in particular.
	notifyAt func(title, body, place, id string)
	onAC     func() bool
}

// New returns a Recorder that writes under dataDir/recordings, reads desktop context from and files minutes into store, and summarises with the Gemini API key apiKey.
// ctx is the daemon's own context, and it bounds every background job the recorder starts here: the startup sweep, the retry loop, and the whisper run either of them starts. Under context.Background() a decode outlived the daemon that started it, kept the GPU, and was still running when the next daemon swept the same recording and started a second decode beside it.
func New(ctx context.Context, dataDir string, store Store, apiKey string) *Recorder {
	r := &Recorder{dataDir: dataDir, store: store, apiKey: apiKey}
	r.capture = func(mic, system io.Writer) (capturer, time.Time, time.Time, error) {
		c, err := audio.StartMeetingCapture(mic, system)
		if err != nil {
			return nil, time.Time{}, time.Time{}, err
		}
		return c, c.MicStart, c.SystemStart, nil
	}
	r.whisper = transcribeWAV
	r.findWhisper = WhisperCPPBinary
	r.diarize = diarizeWAV
	r.findSherpa = SherpaBinary
	r.minutes = r.defaultBrain
	r.notify = notifySend
	r.notifyAt = notifySendAt
	r.onAC = OnACPower
	r.silenceAfter = defaultSilenceAfter
	r.retryEvery = defaultRetryEvery
	r.prepTimeout = defaultPrepTimeout
	r.swept = make(chan struct{})
	go func() {
		defer close(r.swept)
		r.pickup(ctx)
		// The duration backfill corrects notes filed before the duration marker existed, so it has nothing to find once it has been through them: it runs here, at startup, rather than on every sweep.
		r.backfillOnce(ctx)
	}()
	go r.retryDeferred(ctx)
	return r
}

// defaultRetryEvery is how often the recorder looks for a recording it deferred, which is short enough that plugging in gets the transcript within a few minutes and long enough to cost nothing.
const defaultRetryEvery = 2 * time.Minute

// pickup finishes every recording that still needs it: one abandoned by a crash or by quitting the tray mid-recording, one whose transcription was deferred because the machine was on battery, and one that has its transcript but no minutes — which is how the user asks for the minutes again, by deleting minutes.md.
// It is best-effort — a directory that fails is logged and the next one still gets its turn — and it skips the recording that is running right now, whose WAVs are still being written and which on disk is indistinguishable from an abandoned one.
func (r *Recorder) pickup(ctx context.Context) {
	root := filepath.Join(r.dataDir, "recordings")
	entries, err := os.ReadDir(root)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("could not scan for unfinished meeting recordings", "dir", root, "error", err)
		}
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		s, ok := unfinished(dir)
		// liveDir is read per directory rather than once up front: a recording starting while this loop runs holds the lock from before it creates its directory until after it publishes it, so by the time this call returns the answer for that directory is settled.
		if !ok || dir == r.liveDir() {
			continue
		}
		// Regenerating minutes from an existing transcript is cheap and may run on battery; running whisper is not, and StopAndProcess already defers that until the machine is plugged in. The sweep has to honour the same deferral, or a recording that just ended on battery gets transcribed on the very next tick.
		if !s.fromTranscript && !r.onAC() {
			continue
		}
		// A recording kept because transcripts were not installed waits here, quietly, until they are: this loop runs every retryEvery, so the first sweep after the install finishes writes it up, and trying before then would only log the same missing install every two minutes.
		if !s.fromTranscript && !r.TranscriptionReady() {
			continue
		}
		// Peeking at the claim set (rather than claiming here) is what keeps this log from firing for a directory whose whisper run from a previous tick is still going: without it, a long meeting logged "finishing" every retryEvery for as long as it took to transcribe, though process() below was always going to no-op.
		if r.claimedElsewhere(dir) {
			continue
		}
		slog.Info("finishing an unfinished meeting recording", "dir", dir, "from the existing transcript", s.fromTranscript)
		if err := r.process(ctx, s); err != nil {
			slog.Error("could not finish an unfinished meeting recording", "dir", dir, "error", err)
		}
	}
}

// backfillOnce runs the duration backfill over every recording on disk. Input: the daemon's context. Output: none — a recordings directory that cannot be read means there is nothing to correct.
func (r *Recorder) backfillOnce(ctx context.Context) {
	root := filepath.Join(r.dataDir, "recordings")
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	r.backfillDurations(ctx, root, entries)
}

// backfillDurations corrects every already-filed meeting note that predates the duration marker (see meetingDurationLine): it was filed by a version of fileMinutes that never wrote one, so GET /meetings has nothing to read and reports it as a zero-length meeting. The one thing left on disk that still says how long such a meeting ran is the size of its mic.wav, still there because keepAudio keeps it — the directory's own name gives the start, and the audio's length gives the stop. A recording whose audio has since been removed is left exactly as it is: nothing else on file says how long that meeting took, so it keeps reporting zero rather than a guess. Input: the store's own context, the recordings directory, and the entries already read from it. Output: none — every note this corrects is logged, and a note or a directory this cannot make sense of is skipped rather than fatal, the same as everywhere else in this sweep.
func (r *Recorder) backfillDurations(ctx context.Context, root string, entries []os.DirEntry) {
	notes, err := r.store.GetNotes(ctx)
	if err != nil {
		slog.Warn("could not read notes to backfill meeting durations", "error", err)
		return
	}
	byID := make(map[int64]db.Note, len(notes))
	for _, n := range notes {
		byID[n.ID] = n
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		raw, err := os.ReadFile(filepath.Join(dir, noteIDFile))
		if err != nil {
			continue
		}
		id, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
		if err != nil {
			continue
		}
		note, ok := byID[id]
		if !ok || note.Kind != noteKind || strings.Contains(note.Content, meetingDurationPrefix) {
			continue
		}
		started, err := dirStart(e.Name())
		if err != nil {
			continue
		}
		d := audioDuration(filepath.Join(dir, "mic.wav"))
		if d <= 0 {
			continue
		}
		if err := r.store.UpdateNote(ctx, id, withMeetingDuration(note.Content, started, started.Add(d))); err != nil {
			slog.Warn("could not backfill a meeting note's duration", "dir", dir, "note_id", id, "error", err)
			continue
		}
		slog.Info("backfilled a meeting note's duration from its recording's audio", "dir", dir, "note_id", id, "duration", d)
	}
}

// liveDir returns the directory of the recording running right now, or "" when nothing is recording.
func (r *Recorder) liveDir() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.live == nil {
		return ""
	}
	return r.live.dir
}

// Quiescent reports whether the recorder has no live session and nothing in flight — the overnight dreaming loop's licence to take the machine, since a whisper decode or a minutes call must never be raced for the GPU or the brain.
func (r *Recorder) Quiescent() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.live == nil && len(r.inFlight) == 0
}

// claim reserves a recording directory for processing, reporting false if something else already has it. release gives it back.
func (r *Recorder) claim(dir string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inFlight[dir] {
		return false
	}
	if r.inFlight == nil {
		r.inFlight = make(map[string]bool)
	}
	r.inFlight[dir] = true
	return true
}

func (r *Recorder) release(dir string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.inFlight, dir)
}

// claimedElsewhere reports whether dir is already being processed by another in-flight call, without claiming it. It exists only so the sweep can decide whether to log before it hands a directory to process(), which does the real, race-free claim.
func (r *Recorder) claimedElsewhere(dir string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inFlight[dir]
}

// retryDeferred runs pickup every retryEvery for as long as the process lives, so a recording deferred to save the battery is transcribed within a few minutes of the charger going in.
func (r *Recorder) retryDeferred(ctx context.Context) {
	tick := time.NewTicker(r.retryEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if r.onAC() {
				r.pickup(ctx)
			}
		}
	}
}

// Active reports whether a recording is running, which is what the tray menu label keys off.
// StopForShutdown closes a recording that is running so the daemon can exit without abandoning it, and does nothing when none is.
//
// Input: none. Output: the directory the recording was written to, empty when nothing was running, and an error only when a running recording could not be closed.
//
// Transcription is deliberately not run here. Whisper takes minutes and pins every core, and a process being asked to exit — by a logout, a reboot, or a package upgrade — does not have minutes. The audio is closed properly and the sweep on the next start transcribes it, which is the same path that recovers a recording after a crash.
func (r *Recorder) StopForShutdown() (string, error) {
	if !r.Active() {
		return "", nil
	}
	s, err := r.stop()
	if err != nil {
		return "", err
	}
	r.stateChanged()
	slog.Info("closed a running meeting recording for shutdown, it will be transcribed on the next start", "dir", s.dir)
	return s.dir, nil
}

// AddStateObserver registers a function to call whenever a recording starts or stops, beside any already registered. It is called on the caller's goroutine, outside the recorder's locks, so a slow observer delays the caller rather than blocking anything asking whether a recording is running.
func (r *Recorder) AddStateObserver(fn func()) {
	r.observersMu.Lock()
	defer r.observersMu.Unlock()
	r.stateObservers = append(r.stateObservers, fn)
}

// stateChanged tells every observer that a recording started or stopped.
func (r *Recorder) stateChanged() {
	r.observersMu.Lock()
	observers := slices.Clone(r.stateObservers)
	r.observersMu.Unlock()
	for _, fn := range observers {
		fn()
	}
}

func (r *Recorder) Active() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.live != nil
}

// LiveSegment is one line of transcript already known while a meeting is still being recorded. Nothing in this package produces one today — see the doc comment on LiveSnapshot.
type LiveSegment struct {
	At      time.Time
	Speaker string
	Text    string
}

// LiveSnapshot is what a meeting recording looks like while it is still running, for a caller — another agent, or the desktop window — that wants to know what is happening before StopAndProcess has produced minutes.
type LiveSnapshot struct {
	StartedAt time.Time
	// Window is the meeting window's title, read the same way prep.go names it for the "before you join" notification. Empty when nothing on screen has named it yet.
	Window string
	// Participants are the names read off the meeting window so far, using the same heuristic prep.go uses before the call starts. It can only grow as more of the call appears on screen.
	Participants []string
	// SegmentsSoFar is always empty today: transcription is one pass at stop (see transcriptFor), not incremental, so there is nothing said in the meeting yet to hand over.
	SegmentsSoFar []LiveSegment
	// TranscribedThrough is the zero time until SegmentsSoFar carries real transcription.
	TranscribedThrough time.Time
	// Note explains why SegmentsSoFar is empty, since an empty list alone reads as "nobody has said anything" rather than "nothing is transcribed yet."
	Note string
}

// noIncrementalTranscriptionNote is LiveSnapshot's Note while whisper only ever runs once, after the recording stops.
const noIncrementalTranscriptionNote = "transcribed at the end: this build runs whisper once, after the recording stops, so nothing said so far is available yet"

// LiveSnapshot reports what is known about the meeting recording running right now. Input: a context, used only for the screen-context read. Output: the snapshot and true, or a zero LiveSnapshot and false when nothing is being recorded.
func (r *Recorder) LiveSnapshot(ctx context.Context) (LiveSnapshot, bool) {
	r.mu.Lock()
	live := r.live
	r.mu.Unlock()
	if live == nil {
		return LiveSnapshot{}, false
	}

	snap := LiveSnapshot{
		StartedAt:     live.startedAt,
		SegmentsSoFar: []LiveSegment{},
		Note:          noIncrementalTranscriptionNote,
	}
	eps, err := r.store.EpisodesInWindow(ctx, live.startedAt, time.Now(), episodeLimit)
	if err != nil {
		slog.Debug("live meeting snapshot: could not read screen context", "error", err)
		snap.Participants = []string{}
		return snap, true
	}
	var known []string
	if entries, err := r.store.PersonalContext(ctx); err == nil {
		known = personNamesFromContext(entries)
	}
	snap.Window = meetingTitle(eps)
	snap.Participants = meetingParticipants(eps, known)
	if snap.Participants == nil {
		snap.Participants = []string{}
	}
	return snap, true
}

// Start opens both audio streams and begins writing mic.wav and system.wav under a fresh timestamped directory.
// It refuses with ErrTranscriptionMissing, and points the user at Local features, when meeting transcripts are not installed: a recording made then is an hour of audio June can do nothing with, and it used to end in a "Meeting transcription failed" card carrying whisper's install paths.
func (r *Recorder) Start() error {
	if !r.TranscriptionReady() {
		r.notifyAt(setupTitle, "June records and summarises calls once meeting transcripts are set up. "+setupHint, setupPlace, setupID)
		return ErrTranscriptionMissing
	}
	if err := r.open(); err != nil {
		return err
	}
	r.stateChanged()
	// Notifying happens outside the lock, as it does on the stop path: notify shells out to notify-send, and holding the recorder's lock across a process spawn stalls anything asking whether a recording is running.
	r.notify("Recording meeting", "June is recording. Stop it from the tray when the call ends.")
	// The moment recording starts is the moment June knows a call is happening, so it is also the moment to look for what matters from the last time these people met. It runs in its own goroutine and is best-effort throughout: Start must return the instant capture is open, and a slow or empty prep must never hold that up. r.live already guards against Start running twice for one meeting, so this fires at most once per recording the same way the "Recording meeting" notice does.
	go r.prepMeeting()
	return nil
}

// open makes the recording directory, opens both audio streams and publishes the live session. Input: none. Output: an error if any of that fails, with the half-made recording directory removed again.
func (r *Recorder) open() (err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.live != nil {
		return errors.New("a meeting recording is already running")
	}

	dir, err := makeRecordingDir(filepath.Join(r.dataDir, "recordings"))
	if err != nil {
		return fmt.Errorf("create recording dir: %w", err)
	}
	// A recording that never got its streams open leaves a directory of stub WAVs, which on disk is indistinguishable from a recording a crash abandoned — so the sweep would pick it up and try to transcribe silence on every tick.
	defer func() {
		if err != nil {
			if rmErr := os.RemoveAll(dir); rmErr != nil {
				slog.Warn("could not remove the directory of a recording that failed to start", "dir", dir, "error", rmErr)
			}
		}
	}()

	mic, err := newWAV(filepath.Join(dir, "mic.wav"))
	if err != nil {
		return fmt.Errorf("create mic.wav: %w", err)
	}
	sys, err := newWAV(filepath.Join(dir, "system.wav"))
	if err != nil {
		mic.Close()
		return fmt.Errorf("create system.wav: %w", err)
	}

	micWatch, sysWatch := newSilenceWatch(mic), newSilenceWatch(sys)
	cap, micStart, sysStart, err := r.capture(micWatch, sysWatch)
	if err != nil {
		mic.Close()
		sys.Close()
		return fmt.Errorf("start capture: %w", err)
	}

	// The earlier of the two stream starts is the recording's zero, so segments from the two transcripts land on one clock.
	zero := micStart
	if sysStart.Before(zero) {
		zero = sysStart
	}
	r.live = &session{
		dir:       dir,
		startedAt: zero,
		cap:       cap,
		mic:       mic,
		sys:       sys,
		micOffset: micStart.Sub(zero),
		sysOffset: sysStart.Sub(zero),
		done:      make(chan struct{}),
	}
	// A capture that can say whether a stream fell over (the real one can, the test fakes need not) is asked on every tick.
	dropped := func() bool { return false }
	if d, ok := cap.(interface{ Dropped() bool }); ok {
		dropped = d.Dropped
	}
	go r.watchSilence(micWatch, sysWatch, r.silenceAfter, dropped, r.live.done)
	return nil
}

// StopAndProcess halts capture and runs transcription and summarising in the background, notifying the user at each stage. Returns the recording directory.
func (r *Recorder) StopAndProcess(ctx context.Context) (string, error) {
	s, err := r.stop()
	if err != nil {
		return "", err
	}
	r.stateChanged()
	// Whisper pins every core for minutes. On battery that empties the laptop and the CPU is throttled while it runs, so the recording is left exactly as it is — no marker, nothing to say it is finished — and the retry loop picks it up once the charger is back in.
	if !r.onAC() {
		slog.Info("deferring meeting transcription until the machine is on mains power", "dir", s.dir)
		r.notify("Recording saved", "June will transcribe it once you plug in.")
		return s.dir, nil
	}
	if !r.TranscriptionReady() {
		slog.Info("keeping a meeting recording until meeting transcripts are set up", "dir", s.dir)
		r.notifyFailure(ErrTranscriptionMissing)
		return s.dir, nil
	}
	r.notify("Transcribing meeting", "June is transcribing the recording in the background.")
	go func() {
		if err := r.process(ctx, s); err != nil {
			slog.Error("meeting post-processing failed", "dir", s.dir, "error", err)
			r.notifyFailure(err)
		}
	}()
	return s.dir, nil
}

// ErrTranscriptionMissing is what Start returns, and what a recording's processing fails with, when no whisper.cpp build and model are installed. It is its own error so the failure is told apart from a transcription that ran and failed: it is not counted as a failed attempt, and the user is sent to set transcripts up rather than shown whisper's paths.
var ErrTranscriptionMissing = errors.New("meeting transcripts are not set up")

// errNoSpeech and errSummarise mark the two ways processing fails that the user is told about in their own words; anything else gets the general sentence in failureText.
var (
	errNoSpeech  = errors.New("transcription found no speech")
	errSummarise = errors.New("summarise meeting")
)

// The notice that sends the user to set meeting transcripts up opens Settings at the Local features card, where the transcribe feature is installed.
const (
	setupTitle = "Set up meeting transcripts"
	setupHint  = "Open Settings → Local features to add meeting transcripts."
	setupPlace = "settings"
	setupID    = "features"
)

// TranscriptionReady reports whether a recording made now could be transcribed: the whisper.cpp build and its model are where WhisperCPPBinary looks. The tray and the meeting watcher offer to record only when it is true.
func (r *Recorder) TranscriptionReady() bool {
	_, err := r.findWhisper(r.dataDir)
	return err == nil
}

// notifyFailure tells the user a meeting could not be written up, in a sentence rather than the error itself: the error names whisper's paths and the brain's raw reply, which belong in the log and say nothing a person can act on. The audio is always kept, so every sentence says so.
func (r *Recorder) notifyFailure(err error) {
	if errors.Is(err, ErrTranscriptionMissing) {
		r.notifyAt("Recording saved", "Meeting transcripts aren't set up, so June kept the audio and will write it up once they are. "+setupHint, setupPlace, setupID)
		return
	}
	r.notify("Meeting not written up", failureText(err))
}

// failureText is the one sentence a failed meeting write-up is reported in. Input: the error processing returned. Output: a sentence with no paths and no provider output in it.
func failureText(err error) string {
	switch {
	case errors.Is(err, errNoSpeech):
		return "June heard no speech in the recording, so there are no minutes. The audio is kept."
	case errors.Is(err, errSummarise):
		return "June transcribed the meeting but could not write the minutes. It will try again later."
	}
	return "June could not transcribe the meeting. The recording is kept and June will try again later."
}

// stop halts capture and closes both WAV files, returning the finished session.
func (r *Recorder) stop() (*session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.live
	if s == nil {
		return nil, errors.New("no meeting recording is running")
	}
	r.live = nil

	if s.done != nil {
		close(s.done)
	}
	s.cap.Stop()
	s.stoppedAt = time.Now()
	if err := s.mic.Close(); err != nil {
		slog.Warn("closing mic.wav", "error", err)
	}
	if err := s.sys.Close(); err != nil {
		slog.Warn("closing system.wav", "error", err)
	}
	return s, nil
}

// process turns a recording into minutes: it gets the transcript — by running whisper over both streams, or by reading the one already on disk — then writes minutes.md, files the minutes as a note and updates personal context.
func (r *Recorder) process(ctx context.Context, s *session) (err error) {
	// Two paths can reach the same recording — the stop that made it and the retry loop looking for deferred ones — and transcribing it twice would race two whisper runs onto the same files.
	if !r.claim(s.dir) {
		return nil
	}
	defer r.release(s.dir)
	// How this attempt ended is recorded next to the recording, because on disk a recording that failed to summarise looks exactly like one that was never summarised at all. The attempt that reaches the cap also files a note, so a recording nobody is going to retry says so in the meetings list rather than only in the log.
	// The context is captured here because the timeout one below replaces it, and by the time this runs that one is cancelled.
	outer := ctx
	defer func() {
		// Transcripts not being installed is not this recording failing, and counting it would spend the recording's retries while the user has not yet installed them; the sweep waits for the install instead (see pickup).
		if errors.Is(err, ErrTranscriptionMissing) {
			return
		}
		if attempts := markOutcome(s.dir, err); err != nil && attempts >= maxProcessAttempts {
			r.fileGivenUp(outer, s, err)
		}
	}()

	ctx, cancel := context.WithTimeout(ctx, transcribeTimeout)
	defer cancel()

	transcript, err := r.transcriptFor(ctx, s)
	if err != nil {
		return err
	}

	text, err := r.minutes(ctx, r.buildPrompt(ctx, transcript, s.startedAt, s.stoppedAt))
	if err != nil {
		return fmt.Errorf("%w: %w", errSummarise, err)
	}
	text = trimPreamble(text)
	// A blank reply is not a summary of anything: writing it as minutes.md would mark the meeting done and lose it silently. Treating it as an error instead sends it through the same failure marker and hour-long backoff as a summariser that returned an error outright.
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("%w: the brain returned empty minutes", errSummarise)
	}
	if err := os.WriteFile(filepath.Join(s.dir, "minutes.md"), []byte(text), 0o644); err != nil {
		return fmt.Errorf("write minutes: %w", err)
	}
	// Whether this meeting has been filed before is read before fileMinutes files it, since that is the call that writes the note id.
	filed := util.Exists(filepath.Join(s.dir, noteIDFile))
	noteID := r.fileMinutes(ctx, s.dir, text, s.startedAt, s.stoppedAt)

	// The meeting may have taught June something durable about a person the user works with. This is the only path that writes personal context without the user saying it outright, so the model is held to a strict bar (see personalUpdateInstruction) and every write it makes is logged.
	// It runs once per meeting, on the run that first files the minutes: re-summarising a meeting already filed would only re-propose the writes it made the first time. The evidence is the note id on disk rather than whether the transcript came off disk, because a meeting whose minutes failed the first time is retried from its own transcript — and gating on that skipped the pass on the only run that ever reached minutes.
	if !filed {
		r.updatePersonalContext(ctx, text, s.startedAt, s.stoppedAt)
	}

	// The card opens the meeting in the window's Meetings screen, whose rows are the minutes' note ids, and its body is the title that list shows it under. It used to be the plain notice with minutes.md's path as its body: a path nobody can click, on a card whose Open went nowhere. When memory refused the minutes there is no row to open and minutes.md is the only copy, so that notice still says where the file is rather than sending the user to a list the meeting is not in.
	title := minutesTitle(text)
	if noteID > 0 {
		r.notifyAt("Meeting summary ready", title, "meetings", strconv.FormatInt(noteID, 10))
	} else {
		r.notify("Meeting summary ready", fmt.Sprintf("%s. June could not file it in Meetings; the minutes are in %s", title, filepath.Join(s.dir, "minutes.md")))
	}
	return nil
}

// minutesTitle names a set of minutes the way the window's Meetings list does (ipc's minutesTitle, which this package cannot import). Input: the minutes markdown. Output: the name on the line under the heading (memory.MinutesLabel), the "# " heading when there is no such line, or "Meeting" when there is neither.
func minutesTitle(minutes string) string {
	if label := memory.MinutesLabel(minutes); label != "" {
		return label
	}
	for _, line := range strings.Split(minutes, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "# "))
		}
	}
	return "Meeting"
}

// trimPreamble drops whatever the model wrote before the minutes themselves. Input: the model's whole reply. Output: the reply from its first "# " heading on, or the reply unchanged when it has no heading.
// Models often open with a line addressed to the reader — "I'll write the minutes for this platform sprint standup meeting." — and that line is not part of the minutes. It mattered beyond tidiness because the meeting's name is read off the first line that is not a heading (memory.MinutesLabel), so on 16 September 2026 two meetings were listed in the window under the model's own opening sentence. A reply with no heading at all is left alone: that is the shape of a model refusing to write minutes, and the refusal is the only record of what happened.
func trimPreamble(text string) string {
	i := strings.Index(text, "\n# ")
	if i < 0 || strings.HasPrefix(text, "# ") {
		return text
	}
	return text[i+1:]
}

// fileGivenUp files the note that puts a recording the sweep has stopped retrying into the meetings list, where GET /meetings reads notes of this kind. Without it the only trace of a recording that never became minutes is an hourly error line in the log. Input: the store's own context, the recording's session and the error its last attempt returned. Output: none — the note is best effort, like every other write in this sweep, and filing it through fileMinutes means a later retry that succeeds replaces it in place rather than adding a second note for the same meeting.
func (r *Recorder) fileGivenUp(ctx context.Context, s *session, cause error) {
	text := fmt.Sprintf("# Meeting\n\n**Could not be summarised**\n\nJune tried %d times to turn this recording into minutes and has stopped. The audio is still in %s — delete %s in there to have it try again.\n\nLast error: %v\n", maxProcessAttempts, s.dir, failedMarker, cause)
	slog.Warn("giving up on a meeting recording after too many failed attempts", "dir", s.dir, "attempts", maxProcessAttempts, "error", cause)
	r.fileMinutes(ctx, s.dir, text, s.startedAt, s.stoppedAt)
}

// fileMinutes puts a recording's minutes into memory. If the recording was filed before, the note it was filed under is corrected in place; otherwise the minutes are filed as a new note and its id written to noteIDFile so the next run corrects this one. The minutes' action items are also lifted into their own tracked rows, which is what lets a thing somebody agreed to do outlive the few days the minutes themselves are read in. Input: the recording directory, the minutes text, and when the meeting started and stopped. Output: the id of the note the minutes are filed under, which is the meeting's id in the window's Meetings screen, or 0 when memory refused them — that is logged and shrugged off, because minutes.md on disk is the copy that matters.
func (r *Recorder) fileMinutes(ctx context.Context, dir, text string, startedAt, stoppedAt time.Time) int64 {
	defer r.liftActionItems(ctx, text, startedAt)
	stored := withMeetingDuration(text, startedAt, stoppedAt)
	path := filepath.Join(dir, noteIDFile)
	if b, err := os.ReadFile(path); err == nil {
		if id, perr := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); perr == nil {
			// UpdateNote reports a missing row rather than silently doing nothing, so a note the user has since deleted falls through to being filed afresh.
			if uerr := r.store.UpdateNote(ctx, id, stored); uerr == nil {
				return id
			} else {
				slog.Warn("could not correct the note this recording was filed under, filing fresh minutes instead", "dir", dir, "note_id", id, "error", uerr)
			}
		}
	}
	id, err := r.store.LogNote(ctx, stored, noteKind)
	if err != nil {
		slog.Warn("could not file meeting minutes as a note", "error", err)
		return 0
	}
	if err := os.WriteFile(path, []byte(strconv.FormatInt(id, 10)), 0o644); err != nil {
		slog.Warn("could not record which note the meeting minutes were filed under", "dir", dir, "error", err)
	}
	return id
}

// liftActionItems files the minutes' action items as their own rows, every one of them, and then closes any open task these minutes say is finished. The minutes are left exactly as written — they are the record of what was said, and nothing may edit them to claim a task is finished; the lifted items are the live copy, the one the user closes or re-prioritises. Re-filing the same meeting adds nothing, because the store matches on the work itself rather than on the line it is rendered as.
// Other people's items are filed too, rather than dropped as they used to be: whose an item is now comes from its owner read against who the user is, so an item owed by somebody else is a thing he is waiting for (GET /tasks?owner=them) instead of something the store never heard.
func (r *Recorder) liftActionItems(ctx context.Context, minutes string, startedAt time.Time) {
	items := memory.ParseMinutesActions(minutes, memory.MinutesLabel(minutes), startedAt)
	if len(items) == 0 {
		return
	}
	added, err := r.store.AddActionItems(ctx, items)
	if err != nil {
		slog.Warn("could not file this meeting's action items", "error", err)
		return
	}
	slog.Info("filed action items from a meeting", "found", len(items), "new", added)

	// A meeting is where somebody says last week's task is done, so the freshly filed minutes are read straight back as evidence against the open list.
	if closed, err := r.store.CloseDoneActionItems(ctx, startedAt.Add(-closingEvidenceMargin)); err != nil {
		slog.Warn("could not close the tasks this meeting says are finished", "error", err)
	} else if closed > 0 {
		slog.Info("closed tasks this meeting says are finished", "closed", closed)
	}
}

// closingEvidenceMargin is how far before a meeting started the evidence sweep reads from. A day either side, so the minutes filed for this meeting are certainly inside the window however long the write-up took, without re-reading months of writing after every call.
const closingEvidenceMargin = 24 * time.Hour

// transcriptFor returns the meeting's transcript. A recording the sweep found with its transcript already written just has it read back off disk, which is what makes deleting minutes.md a request for fresh minutes; anything else is transcribed with whisper and the result written to transcript.md.
func (r *Recorder) transcriptFor(ctx context.Context, s *session) (string, error) {
	if s.fromTranscript {
		b, err := os.ReadFile(filepath.Join(s.dir, "transcript.md"))
		if err != nil {
			return "", fmt.Errorf("read the existing transcript: %w", err)
		}
		return string(b), nil
	}

	bin, err := r.findWhisper(r.dataDir)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrTranscriptionMissing, err)
	}

	// Whisper is primed with the words June already watched go past on screen during the meeting, which is what gets the domain's own acronyms and the participants' names spelled right instead of guessed at phonetically.
	prompt := r.primingPrompt(ctx, s.startedAt, s.stoppedAt)
	if prompt != "" {
		slog.Info("priming whisper with the meeting's screen context", "dir", s.dir, "prompt", prompt)
	}

	// The two streams are separate files that share nothing, so they are transcribed at the same time rather than one after the other — which halves the wall time of every meeting. transcribeThreads gives each run half the machine so the two are not simply fighting over the same cores.
	var wg sync.WaitGroup
	var mine, theirs []Segment
	var turns []diarTurn
	var micErr, sysErr error

	// Diarization runs alongside the two transcriptions rather than after them: it is CPU work while whisper decodes on the GPU, so it costs no wall time at all. It only ever reads the system audio — the microphone is one known person and there is nothing there to cluster.
	// Its context is cancelled the moment either transcription fails, because there is no transcript left to label. Without that it runs to completion on a recording already known to be lost, which measured at about ten minutes of CPU for a thirty-nine minute meeting, repeated on every retry.
	diarCtx, stopDiar := context.WithCancel(ctx)
	defer stopDiar()

	wg.Add(3)
	go func() {
		defer wg.Done()
		turns = r.diarizeCall(diarCtx, filepath.Join(s.dir, "system.wav"), s.sysOffset, s.startedAt, s.stoppedAt)
	}()
	go func() {
		defer wg.Done()
		mine, micErr = r.whisper(ctx, bin, filepath.Join(s.dir, "mic.wav"), speakerMe, prompt, s.micOffset)
		if micErr != nil {
			stopDiar()
		}
	}()
	go func() {
		defer wg.Done()
		theirs, sysErr = r.whisper(ctx, bin, filepath.Join(s.dir, "system.wav"), speakerCall, prompt, s.sysOffset)
		if sysErr != nil {
			stopDiar()
		}
	}()
	wg.Wait()
	if micErr != nil {
		return "", fmt.Errorf("transcribe mic: %w", micErr)
	}
	if sysErr != nil {
		return "", fmt.Errorf("transcribe system audio: %w", sysErr)
	}

	// Every remote voice arrived under one pooled label; the diarizer's clusters split it back into people, which is what lets the minutes attribute a line rather than hedge about it.
	theirs = assignSpeakers(theirs, turns)

	// Every line is written in Latin letters whatever language it was spoken in, and the lines nobody in the meeting said are dropped before anything counts them as speech.
	var segs []Segment
	for _, seg := range append(mine, theirs...) {
		if cannedLine.MatchString(seg.Text) {
			continue
		}
		seg.Text = Romanize(seg.Text)
		segs = append(segs, seg)
	}

	// Whisper exiting 0 with nothing to show for it is not a success: the meeting may have been silent, or this whisper build may print segments in a shape parseSegments does not recognise. Either way the WAVs are still the only copy of the meeting, so they stay put and the marker records why.
	if len(segs) == 0 {
		note := fmt.Sprintf("Transcription ran without error but found no speech in this recording, so the audio has been kept instead of deleted. Written %s.\n", time.Now().Format(time.RFC3339))
		if err := os.WriteFile(filepath.Join(s.dir, noSpeechMarker), []byte(note), 0o644); err != nil {
			slog.Warn("could not write the no-speech marker", "dir", s.dir, "error", err)
		}
		return "", fmt.Errorf("%w; the audio is kept in %s", errNoSpeech, s.dir)
	}

	transcript := renderTranscript(segs)
	if err := os.WriteFile(filepath.Join(s.dir, "transcript.md"), []byte(transcript), 0o644); err != nil {
		return "", fmt.Errorf("write transcript: %w", err)
	}

	// Both transcriptions landed and the transcript is on disk, so the audio is no longer the only copy of the meeting.
	// ponytail: keepAudio stays true for the alpha phase so bad transcripts can be diagnosed and re-run from source; flip to false (or make it config) once transcription is trusted, since a two-stream hour is ~230 MB.
	if !keepAudio {
		for _, name := range []string{"mic.wav", "system.wav"} {
			if err := os.Remove(filepath.Join(s.dir, name)); err != nil && !os.IsNotExist(err) {
				slog.Warn("could not delete recording audio", "file", name, "error", err)
			}
		}
	}
	return transcript, nil
}

// diarizeCall splits the call side into voices, best-effort. Every failure — no diarizer installed, a model missing, the run erroring — returns no turns and leaves the transcript exactly as it was, with every remote voice pooled under one label. A meeting is worth having with unattributed speech and worthless without a transcript, so nothing here is allowed to fail a recording.
func (r *Recorder) diarizeCall(ctx context.Context, path string, offset time.Duration, since, until time.Time) []diarTurn {
	bin, err := r.findSherpa(r.dataDir)
	if err != nil {
		slog.Debug("not splitting the call into voices", "reason", err)
		return nil
	}
	speakers := r.remoteVoices(ctx, since, until)
	started := time.Now()
	turns, err := r.diarize(ctx, bin, path, speakers, offset)
	if err != nil {
		// A cancelled context here is the transcription having failed and this run being stopped on purpose, not the diarizer going wrong. Reporting that as a warning would put an alarming line in the log for something the code just did deliberately.
		if ctx.Err() != nil {
			slog.Debug("stopped splitting the call into voices, the transcription it belongs to failed", "error", err)
			return nil
		}
		slog.Warn("could not split the call into voices, so every remote speaker stays pooled", "error", err)
		return nil
	}
	found := map[int]bool{}
	for _, t := range turns {
		found[t.Speaker] = true
	}
	slog.Info("split the call into voices", "turns", len(turns), "expected", speakers, "found", len(found), "took", time.Since(started))
	return turns
}

// remoteVoices is how many people the meeting app showed inside its window, which the diarizer is given so it returns that many voices instead of estimating from an audio distance that does not transfer between meetings. Zero means the window did not say, and the diarizer estimates instead.
// The window's TITLE is deliberately not counted, though it often carries a name. A one-to-one call is titled after the other person — "Microsoft Teams (PWA) - Chat | Vexil Quorin" — but a group call is titled after the meeting, and "Daily Platform Standup" is two capitalised words that read exactly like a name. Counting titles would therefore report one voice for a nine-person standup and merge all nine into one, which is worse than not knowing: one voice too many splits a person across two clusters and the summarising model rejoins them from what was said, while one too few fuses two people and nothing downstream can undo it.
// Only the window's own contents count — the participant tiles and roster the accessibility tree reads out of the meeting window itself, which name people and nothing else.
func (r *Recorder) remoteVoices(ctx context.Context, since, until time.Time) int {
	eps, err := r.store.EpisodesInWindow(ctx, since.Add(-contextMargin), until.Add(contextMargin), episodeLimit)
	if err != nil {
		return 0
	}
	bodies := make([]db.Episode, 0, len(eps))
	for _, e := range eps {
		// The title is dropped and the app and window identity kept, so the episode still has to pass the meeting-window test while contributing none of its title's words as names.
		bodies = append(bodies, db.Episode{App: e.App, Title: e.Title, ScreenText: e.ScreenText, VisibleText: e.VisibleText})
	}
	names := meetingParticipantsInBody(bodies)

	// Always logged, so a real group meeting says whether this can be trusted before it is trusted. Reading a roster out of the accessibility tree means reading names out of one flattened run of text, and a run of capitalised words does not say where one person ends and the next begins: three names listed back to back parse as one long name rather than as three people. That undercounts, and undercounting is the direction that fuses several people into one voice, which nothing downstream can undo.
	// No group meeting has ever been captured from inside its window on this machine, so there is no sample of what a roster looks like here. The setting is what turns the count on once the log line below shows real names from a real group call; until then the diarizer estimates, which over-splits, and over-splitting the summarising model can repair.
	used := config.LoadConfig().Transcribe.SpeakerCountFromScreen
	slog.Info("names read from inside the meeting window", "names", names, "count", len(names), "used", used)
	if !used {
		return 0
	}
	return len(names)
}
