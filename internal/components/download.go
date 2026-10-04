package components

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"june/internal/config"
	"june/internal/util"
)

// Download tuning. stallTimeout ends a connection that has gone quiet without closing, which a laptop changing networks mid-download leaves behind; fetchAttempts is how many times one file is retried, each attempt resuming where the last stopped; progressEvery keeps the event stream to four updates a second however fast the bytes arrive.
const (
	stallTimeout  = 60 * time.Second
	fetchAttempts = 5
	progressEvery = 250 * time.Millisecond
)

// newHTTPClient is the client every download goes through. It has no overall timeout, since a 2.8 GB model on a slow line takes as long as it takes; instead each phase that can hang has its own bound, and the body is watched for stalls in copyBody. It goes through util.SystemProxy: the proxy variables, or the manual proxy set in Windows' settings, which a browser also follows. What a browser follows and June does not (a proxy script or automatic detection, a socks= entry, a proxy that asks for a Windows sign-in) is listed at osProxy in internal/util/proxy_windows.go.
func newHTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy:                 util.SystemProxy,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   20 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     true,
	}}
}

// partialMeta is the note kept beside a partial download: the validator the server gave the bytes already on disk, so a resumed request can ask for the rest only if they are still the same file.
type partialMeta struct {
	URL  string `json:"url"`
	ETag string `json:"etag"`
}

// errChecksum is a download whose bytes are not the pinned file. It is never retried as it stands, since the same URL would give the same wrong bytes.
var errChecksum = errors.New("the download does not match its pinned checksum")

// partialPath is where an artifact's download is kept until it is verified and installed. It is named by the artifact's hash rather than its URL, so the same file wanted by two features is fetched once.
func (s *Service) partialPath(a *artifact) string {
	return filepath.Join(s.opts.DataDir, "components", ".partial", a.SHA256[:8]+".part")
}

// partialBytes is how much of an artifact is already on disk from an earlier, interrupted download.
func (s *Service) partialBytes(a *artifact) int64 {
	info, err := os.Stat(s.partialPath(a))
	if err != nil {
		return 0
	}
	return min(info.Size(), a.Size)
}

// fetch downloads an artifact into its partial file, resuming what an earlier attempt left, and checks the whole file against the pinned SHA256 as it goes. A connection that drops is retried, each retry asking only for the bytes still missing; GitHub and Hugging Face both answer the pinned URL with a redirect to a short-lived signed one, so every attempt starts again from the pinned URL rather than from where the last one was sent. Input: the job's context, which cancelling stops the download with its bytes kept; the artifact; and the job's progress. Output: the path of the verified file, or the error that ended the last attempt.
func (s *Service) fetch(ctx context.Context, a *artifact, prog *progress) (_ string, err error) {
	path := s.partialPath(a)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", humanError(fmt.Sprintf("could not save %s in June's data folder; try again", a.fileName()), a, err)
	}
	// The partial file is made before the server answers, so an attempt that never got a byte would leave an empty one behind for the window to count and offer to discard.
	defer func() {
		if info, statErr := os.Stat(path); err != nil && statErr == nil && info.Size() == 0 {
			os.Remove(path)
			os.Remove(path + ".json")
		}
	}()
	// A file found to be wrong is fetched once more from nothing, in case a resume stitched two different files together, and only then reported.
	fresh := false
	var lastErr error
	for attempt := 0; attempt < fetchAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Duration(1<<attempt) * time.Second):
			}
		}
		err := s.fetchOnce(ctx, a, path, prog)
		if err == nil {
			return path, nil
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		lastErr = err
		if errors.Is(err, errChecksum) {
			if fresh {
				return "", humanError(fmt.Sprintf("the downloaded %s is not the file June expects, so it was thrown away; try again later", a.fileName()), a, err)
			}
			fresh = true
			continue
		}
		if diskFull(err) {
			return "", humanError(fmt.Sprintf("not enough disk space to finish downloading %s; free some space and try again", a.fileName()), a, err)
		}
		var perm permanentError
		if errors.As(err, &perm) {
			if perm.gone {
				// The pin names one release or commit, so the file will not come back by retrying, and nothing the user can do on this machine brings it back either; a newer June pins a file that is there.
				return "", humanError(fmt.Sprintf("this version of June can no longer download %s, because the site it comes from no longer has it; update June and try again", a.fileName()), a, err)
			}
			return "", humanError(fmt.Sprintf("could not download %s (%s)", a.fileName(), cause(err)), a, err)
		}
	}
	// A file error is this machine's, often an antivirus holding the partial file open, and the internet connection has nothing to do with it.
	var local *fs.PathError
	if errors.As(lastErr, &local) {
		return "", humanError(fmt.Sprintf("could not save %s in June's data folder; try again", a.fileName()), a, lastErr)
	}
	return "", humanError(fmt.Sprintf("could not download %s (%s); check the internet connection, and on a work or school network whether a firewall or proxy blocks it, then try again", a.fileName(), cause(lastErr)), a, lastErr)
}

// shownError is a failure as the window shows it on the feature's card, a sentence for a person, with the error behind it kept for errors.Is and the log.
type shownError struct {
	msg string
	err error
}

func (e shownError) Error() string { return e.msg }
func (e shownError) Unwrap() error { return e.err }

// humanError logs the whole of a download's failure, where a transport error's address and socket call help, and returns the sentence for the card, which is all the person reading it can act on.
func humanError(msg string, a *artifact, err error) error {
	slog.Warn("component download failed", "file", a.fileName(), "url", a.URL, "error", err)
	return shownError{msg: msg, err: err}
}

// cause is a few words on why a download failed, for the card.
func cause(err error) string {
	var status statusError
	var dns *net.DNSError
	var op *net.OpError
	var cert *tls.CertificateVerificationError
	var netErr net.Error
	switch {
	case err == nil:
		return "the connection failed"
	case errors.As(err, &status):
		return "the server answered " + status.status
	case errors.Is(err, errStalled):
		return "the download stopped arriving"
	case errors.Is(err, errShort):
		return "the download ended early"
	// Before the certificate, name and timeout cases: Go wraps every failure to reach the proxy (its name not found, no answer, its own certificate) in this one OpError, and the cases below would blame the download site for what is the proxy's.
	case errors.As(err, &op) && op.Op == "proxyconnect":
		return "the proxy could not be reached"
	case errors.As(err, &cert):
		return "the download site's certificate was not trusted; something on this network may be intercepting it"
	case errors.As(err, &dns):
		return "the download site could not be found"
	case errors.As(err, &netErr) && netErr.Timeout():
		return "the connection timed out"
	case errors.As(err, &op) && op.Op == "dial":
		return "could not connect to the download site"
	case errors.As(err, &op), errors.Is(err, io.ErrUnexpectedEOF):
		return "the connection dropped"
	}
	return "the connection failed"
}

// permanentError is a refusal a retry cannot change, such as a 404. gone is a 404 or 410 for the pinned URL itself, which means upstream took the file down.
type permanentError struct {
	error
	gone bool
}

// fetchOnce makes one attempt: hash whatever is already on disk, ask for the rest, append it while hashing, and check the result. A checksum failure removes the file, so the next attempt starts from nothing.
func (s *Service) fetchOnce(ctx context.Context, a *artifact, path string, prog *progress) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	have, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	if have > a.Size {
		have = 0
	}
	meta := readPartialMeta(path)
	if meta.URL != a.URL {
		meta = partialMeta{URL: a.URL}
	}
	if have == 0 {
		meta.ETag = ""
	}
	// The bytes already here go through the hash first, so the check at the end covers the whole file and not just what this attempt added.
	h := sha256.New()
	if have > 0 {
		s.publishStep(prog, "verifying", a.fileName())
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if _, err := io.CopyN(h, ctxReader{ctx, f}, have); err != nil {
			return err
		}
	}
	prog.resumeAt(a, have)
	if have < a.Size {
		got, etag, err := s.request(ctx, a, have, meta.ETag)
		if errors.Is(err, errRangeGone) {
			// The bytes here are not this file's, so this attempt starts again from nothing, truncating through the open handle: Windows will not delete a file that is open, and a delete that silently failed would ask for the same range on every retry.
			have, meta.ETag = 0, ""
			h.Reset()
			prog.resumeAt(a, 0)
			got, etag, err = s.request(ctx, a, 0, "")
		}
		if err != nil {
			return err
		}
		defer got.Body.Close()
		if got.from == 0 && have > 0 {
			// The server sent the whole file again, either because it ignores ranges or because the If-Range validator no longer matches, so what was on disk is thrown away.
			have = 0
			h.Reset()
			prog.resumeAt(a, 0)
		}
		if err := f.Truncate(have); err != nil {
			return err
		}
		if _, err := f.Seek(have, io.SeekStart); err != nil {
			return err
		}
		if etag != "" {
			meta.ETag = etag
			writePartialMeta(path, meta)
		}
		n, err := copyBody(ctx, io.MultiWriter(f, h), got.Body, a.Size-have, func(n int64) { prog.add(a, n) })
		have += n
		if err != nil {
			return err
		}
		// The last chunk is usually inside the throttle window, so without this the bar would stop short of the end.
		prog.emit(true)
	}
	if have != a.Size {
		return fmt.Errorf("%w: the server sent %d bytes, expected %d", errShort, have, a.Size)
	}
	s.publishStep(prog, "verifying", a.fileName())
	if hex.EncodeToString(h.Sum(nil)) != a.SHA256 {
		f.Close()
		os.Remove(path)
		os.Remove(path + ".json")
		return fmt.Errorf("%s: %w", a.fileName(), errChecksum)
	}
	return nil
}

// ranged is a response body together with the offset its first byte belongs at.
type ranged struct {
	Body io.ReadCloser
	from int64
}

// request asks for the artifact from byte from on. Input: the context, the artifact, the offset of the first missing byte, and the validator the bytes on disk came with ("" for none). Output: the body and where it starts — from itself for a 206, 0 when the server sent the whole file — the response's own validator, and an error for any other answer.
func (s *Service) request(ctx context.Context, a *artifact, from int64, etag string) (ranged, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return ranged{}, "", permanentError{error: err}
	}
	req.Header.Set("User-Agent", "June/"+config.Version)
	if from > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(from, 10)+"-")
		// A weak validator cannot be used with If-Range (RFC 9110 13.1.5), and without one the range is still safe here: the URL names a fixed release or commit and the hash is checked at the end.
		if etag != "" && !strings.HasPrefix(etag, "W/") {
			req.Header.Set("If-Range", etag)
		}
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return ranged{}, "", err
	}
	tag := resp.Header.Get("ETag")
	switch resp.StatusCode {
	case http.StatusOK:
		return ranged{Body: resp.Body}, tag, nil
	case http.StatusPartialContent:
		start, ok := contentRangeStart(resp.Header.Get("Content-Range"))
		if !ok || start != from {
			resp.Body.Close()
			return ranged{}, "", fmt.Errorf("asked for the rest from byte %d and the server sent %q", from, resp.Header.Get("Content-Range"))
		}
		return ranged{Body: resp.Body, from: from}, tag, nil
	case http.StatusRequestedRangeNotSatisfiable:
		// Only possible when the file on disk is longer than the server's, so it is not this file; fetchOnce starts it again.
		resp.Body.Close()
		return ranged{}, "", errRangeGone
	}
	snippet := util.BodySnippet(resp.Body)
	resp.Body.Close()
	err = statusError{status: resp.Status, body: util.LogHead(snippet)}
	if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusRequestTimeout {
		return ranged{}, "", permanentError{error: err, gone: resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone}
	}
	return ranged{}, "", err
}

// statusError is a server's refusal: its status line for the card, and the start of its body for the log.
type statusError struct {
	status string
	body   string
}

func (e statusError) Error() string { return "the server answered " + e.status + " " + e.body }

// errRangeGone is a 416 for the bytes still missing, which means the partial file is longer than the server's. errStalled and errShort are a connection that went quiet and one that closed early, which cause puts into words for the card.
var (
	errRangeGone = errors.New("the server has fewer bytes than were already downloaded")
	errStalled   = errors.New("no data arrived")
	errShort     = errors.New("the download ended early")
)

// contentRangeStart reads the first byte offset out of a Content-Range header such as "bytes 100-199/200".
func contentRangeStart(v string) (int64, bool) {
	v, ok := strings.CutPrefix(v, "bytes ")
	if !ok {
		return 0, false
	}
	start, _, ok := strings.Cut(v, "-")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(start, 10, 64)
	return n, err == nil
}

// copyBody copies at most limit bytes of body into w, reporting each chunk to onChunk, and gives up when no byte arrives for stallTimeout or ctx ends. Output: the bytes written and the error that stopped the copy, nil when the body ended cleanly; a write that failed for want of disk space comes back as it is, for fetch to tell apart from the line dropping.
func copyBody(ctx context.Context, w io.Writer, body io.ReadCloser, limit int64, onChunk func(int64)) (int64, error) {
	stalled := make(chan struct{})
	timer := time.AfterFunc(stallTimeout, func() {
		close(stalled)
		body.Close()
	})
	defer timer.Stop()
	stop := context.AfterFunc(ctx, func() { body.Close() })
	defer stop()
	buf := make([]byte, 1<<20)
	var written int64
	for written < limit {
		n, err := body.Read(buf[:min(int64(len(buf)), limit-written)])
		if n > 0 {
			timer.Reset(stallTimeout)
			if _, werr := w.Write(buf[:n]); werr != nil {
				return written, werr
			}
			written += int64(n)
			onChunk(int64(n))
		}
		if err == io.EOF {
			return written, nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return written, ctx.Err()
			}
			select {
			case <-stalled:
				return written, fmt.Errorf("%w for %s", errStalled, stallTimeout)
			default:
			}
			return written, err
		}
	}
	return written, nil
}

// ctxReader stops a long read when ctx ends, so cancelling a job is not held up by re-hashing gigabytes already on disk.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

func readPartialMeta(path string) partialMeta {
	var m partialMeta
	if b, err := os.ReadFile(path + ".json"); err == nil {
		json.Unmarshal(b, &m)
	}
	return m
}

func writePartialMeta(path string, m partialMeta) {
	if b, err := json.Marshal(m); err == nil {
		util.WriteFileAtomic(path+".json", b, 0o600)
	}
}
