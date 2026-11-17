package obs

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"june/internal/config"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace"
)

const (
	// spansFileName is the file in config.DataDir() that finished spans are appended to, one JSON object per line.
	spansFileName = "spans.jsonl"
	// maxSpansBytes is how large spans.jsonl may grow before RotatingWriter rolls it aside.
	maxSpansBytes = 10 << 20
	// spansFlushEvery is how often buffered span lines are written to disk, so a span ends without a disk write and still reaches the file within a second.
	spansFlushEvery = time.Second
	// spansBufferBytes is the size of the in-memory buffer that span lines wait in until the next flush.
	spansBufferBytes = 64 << 10
	// maxSpanAttrs and maxSpanAttrChars bound how much of a span's attributes one line carries.
	maxSpanAttrs     = 8
	maxSpanAttrChars = 200
)

// unrecordedSpans are spans too frequent to be worth a line each: the tracker's window poll fires every two seconds, about 43,000 a day, and would fill spans.jsonl with idle ticks.
var unrecordedSpans = map[string]bool{"Tracker.PollActiveWindow": true}

// SpanRecord is one line of spans.jsonl: a finished span with its timing, ids, status and a few attributes. Parent is empty for a root span.
type SpanRecord struct {
	Name       string         `json:"name"`
	Start      time.Time      `json:"start"`
	DurationMS float64        `json:"duration_ms"`
	TraceID    string         `json:"trace_id"`
	SpanID     string         `json:"span_id"`
	Parent     string         `json:"parent_id,omitempty"`
	Status     string         `json:"status"`
	Attrs      map[string]any `json:"attrs,omitempty"`
}

// spanSink is a span processor that appends each finished span to spans.jsonl through a fixed-size buffer that a background goroutine flushes once a second.
type spanSink struct {
	mu   sync.Mutex
	buf  *bufio.Writer
	file *RotatingWriter
	done chan struct{}
	wg   sync.WaitGroup
}

// newSpanSink opens spans.jsonl at path (0600, capped at maxBytes) and starts the flusher. Output: the processor, or an error when the file could not be opened.
func newSpanSink(path string, maxBytes int64) (*spanSink, error) {
	file, err := NewRotatingWriter(path, maxBytes, 0600)
	if err != nil {
		return nil, err
	}
	s := &spanSink{buf: bufio.NewWriterSize(file, spansBufferBytes), file: file, done: make(chan struct{})}
	s.wg.Add(1)
	go s.flushLoop()
	return s, nil
}

// spansEnabled reports whether the local span file is on: it is unless JUNE_SPANS=off.
func spansEnabled() bool { return os.Getenv("JUNE_SPANS") != "off" }

// spansPath is where the local span file lives.
func spansPath() string { return filepath.Join(config.DataDir(), spansFileName) }

func (s *spanSink) flushLoop() {
	defer s.wg.Done()
	t := time.NewTicker(spansFlushEvery)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.ForceFlush(context.Background())
		case <-s.done:
			return
		}
	}
}

func (s *spanSink) OnStart(_ context.Context, _ trace.ReadWriteSpan) {}

// OnEnd encodes the finished span as one line and adds it to the buffer. A line that would not fit flushes the buffer first, so one line is never split across two writes and a rotation never cuts a line in half.
func (s *spanSink) OnEnd(sp trace.ReadOnlySpan) {
	if unrecordedSpans[sp.Name()] {
		return
	}
	line, err := json.Marshal(spanRecord(sp))
	if err != nil {
		return
	}
	line = append(line, '\n')
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.buf.Available() < len(line) {
		s.buf.Flush()
	}
	s.buf.Write(line)
}

func (s *spanSink) ForceFlush(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Flush()
}

// Shutdown stops the flusher, writes what is buffered and closes the file.
func (s *spanSink) Shutdown(context.Context) error {
	close(s.done)
	s.wg.Wait()
	err := s.ForceFlush(context.Background())
	if cerr := s.file.Close(); err == nil {
		err = cerr
	}
	return err
}

// spanRecord turns a finished span into its file line, keeping the first maxSpanAttrs attributes and cutting long strings.
func spanRecord(sp trace.ReadOnlySpan) SpanRecord {
	rec := SpanRecord{
		Name:       sp.Name(),
		Start:      sp.StartTime(),
		DurationMS: float64(sp.EndTime().Sub(sp.StartTime())) / float64(time.Millisecond),
		TraceID:    sp.SpanContext().TraceID().String(),
		SpanID:     sp.SpanContext().SpanID().String(),
		Status:     "unset",
	}
	if p := sp.Parent(); p.SpanID().IsValid() {
		rec.Parent = p.SpanID().String()
	}
	switch sp.Status().Code {
	case codes.Ok:
		rec.Status = "ok"
	case codes.Error:
		rec.Status = "error"
	}
	for _, kv := range sp.Attributes() {
		if len(rec.Attrs) == maxSpanAttrs {
			break
		}
		if v, ok := attrValue(kv.Value); ok {
			if rec.Attrs == nil {
				rec.Attrs = map[string]any{}
			}
			rec.Attrs[string(kv.Key)] = v
		}
	}
	return rec
}

// attrValue returns the value of a string, int or bool attribute (strings cut to maxSpanAttrChars) and false for any other type.
func attrValue(v attribute.Value) (any, bool) {
	switch v.Type() {
	case attribute.STRING:
		s := v.AsString()
		if len(s) > maxSpanAttrChars {
			s = s[:maxSpanAttrChars]
		}
		return s, true
	case attribute.INT64:
		return v.AsInt64(), true
	case attribute.BOOL:
		return v.AsBool(), true
	}
	return nil, false
}
