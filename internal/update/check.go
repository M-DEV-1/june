package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"june/internal/util"
)

// apiBase is GitHub's REST API, where releases/latest names the newest published release and leaves drafts and pre-releases out, so testers' rc builds are never pushed on everyone.
const apiBase = "https://api.github.com"

// maxReleaseJSON bounds how much of an answer from GitHub is read; a release with every asset listed is a few kilobytes.
const maxReleaseJSON = 4 << 20

// client makes every request the updater sends: Go's default transport, but through util.SystemProxy, which adds the manual proxy set in Windows' settings (the one a browser also follows) to the proxy variables the default transport reads. A proxy script or automatic detection, a socks= entry and a proxy that asks for a Windows sign-in are not followed; see osProxy in internal/util/proxy_windows.go. Each request carries its own deadline in its context, so the client has none.
var client = func() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = util.SystemProxy
	return &http.Client{Transport: t}
}()

// asset is one file attached to a release, with the fields GitHub's API names it by.
type asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
	// Digest is "sha256:<hex>", computed by GitHub when the file was uploaded; older releases have none.
	Digest string `json:"digest"`
}

// release is the part of GitHub's release object the updater reads. The same type is what check.json keeps, so a restart knows what the last check found without asking again.
type release struct {
	Tag     string  `json:"tag_name"`
	HTMLURL string  `json:"html_url"`
	Assets  []asset `json:"assets"`
}

// version is the release's number without the tag's leading "v", as the window shows it.
func (r *release) version() string { return strings.TrimPrefix(r.Tag, "v") }

// asset finds the file called name. Output: it, or nil when the release has no such file.
func (r *release) asset(name string) *asset {
	for i := range r.Assets {
		if r.Assets[i].Name == name {
			return &r.Assets[i]
		}
	}
	return nil
}

// savedCheck is <data>/update/check.json: the last answer and its ETag, so the first check after a restart can be a conditional one.
type savedCheck struct {
	Repo      string    `json:"repo"`
	ETag      string    `json:"etag"`
	CheckedAt time.Time `json:"checked_at"`
	Release   *release  `json:"release"`
}

// rateLimited is the error for a check GitHub asked June not to make before until.
func rateLimited(until time.Time) error {
	return &userError{shown: "GitHub asked June to wait before checking for updates again, until " + until.Format("15:04") + "."}
}

// check asks GitHub for the latest release and records the answer. Input: a context bounding the request. Output: nil when GitHub answered, or the error; the caller decides whether the user hears about it.
func (c *Checker) check(ctx context.Context) error {
	c.checkMu.Lock()
	defer c.checkMu.Unlock()

	c.mu.Lock()
	if until := c.notBefore; time.Now().Before(until) {
		c.mu.Unlock()
		return rateLimited(until)
	}
	etag := c.etag
	if c.rel == nil {
		// A 304 with nothing remembered to stand for would leave the updater knowing nothing.
		etag = ""
	}
	// A download under way keeps its state through a check; a past failure is shown again if this check fails too.
	before := c.state
	if before == stateIdle || before == stateFailed {
		c.state = stateChecking
	}
	c.mu.Unlock()

	rel, newETag, err := c.fetchLatest(ctx, etag)

	c.mu.Lock()
	if err != nil {
		if c.state == stateChecking {
			c.state = before
		}
		c.mu.Unlock()
		return err
	}
	if c.state == stateChecking {
		c.state, c.err = stateIdle, ""
	}
	if rel != nil || newETag == "" {
		c.rel, c.etag = rel, newETag
	}
	// Wall-clock time only: the monotonic reading time.Now carries stops while a Linux machine is suspended, and a laptop asleep most of the day would then go days between checks.
	c.checkedAt = time.Now().Round(0)
	saved := savedCheck{Repo: c.opts.Repo, ETag: c.etag, CheckedAt: c.checkedAt, Release: c.rel}
	announce := ""
	if c.availableLocked() && c.rel.version() != c.announced {
		announce = c.rel.version()
		c.announced = announce
	}
	c.mu.Unlock()

	c.save(saved)
	if announce != "" {
		slog.Info("update: a newer June is out", "current", c.opts.Version, "latest", announce)
		c.publish(announce, "available", "", false)
	}
	return nil
}

// fetchLatest makes one request for the latest release. Input: a context and the ETag of the answer already held, or "". Output: the release and its ETag; a nil release with the old ETag when nothing changed (304); a nil release and "" when the repo has no published release yet; or an error.
func (c *Checker) fetchLatest(ctx context.Context, etag string) (*release, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+"/repos/"+c.opts.Repo+"/releases/latest", nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", c.userAgent())
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", &userError{"Couldn't reach GitHub to check for updates. Check the internet connection, and on a work or school network whether a firewall or proxy blocks it.", err}
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		var rel release
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxReleaseJSON)).Decode(&rel); err != nil {
			return nil, "", &userError{"GitHub's answer about the latest release couldn't be read.", err}
		}
		if _, ok := parseVersion(rel.Tag); !ok {
			return nil, "", &userError{shown: fmt.Sprintf("The latest release's tag %q isn't a version number.", rel.Tag)}
		}
		return &rel, resp.Header.Get("ETag"), nil
	case http.StatusNotModified:
		return nil, etag, nil
	case http.StatusNotFound:
		return nil, "", nil
	case http.StatusForbidden, http.StatusTooManyRequests:
		until := retryAt(resp.Header, time.Now())
		c.mu.Lock()
		c.notBefore = until
		c.mu.Unlock()
		return nil, "", rateLimited(until)
	default:
		return nil, "", &userError{shown: "GitHub answered " + resp.Status + " when June checked for updates."}
	}
}

// retryAt reads when GitHub will take another request: Retry-After for a secondary limit, X-RateLimit-Reset once the hourly allowance is spent, and an hour when it says neither. Input: the response's headers and the time now. Output: the earliest time to ask again.
func retryAt(h http.Header, now time.Time) time.Time {
	if s, err := strconv.Atoi(h.Get("Retry-After")); err == nil && s > 0 {
		return now.Add(time.Duration(s) * time.Second)
	}
	if h.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil && time.Unix(reset, 0).After(now) {
			return time.Unix(reset, 0)
		}
	}
	return now.Add(time.Hour)
}

// userAgent names June to GitHub, which refuses API requests that carry none.
func (c *Checker) userAgent() string {
	return "June/" + c.opts.Version + " (" + runtime.GOOS + "; " + runtime.GOARCH + ")"
}

// savePath is where the last check is kept between runs.
func (c *Checker) savePath() string { return filepath.Join(c.opts.DataDir, "update", "check.json") }

// load restores the last check from disk, so a banner for an update found yesterday is up as soon as the daemon is. A missing or unreadable file, or one written for another repo, is the same as never having checked.
func (c *Checker) load() {
	data, err := os.ReadFile(c.savePath())
	if err != nil {
		return
	}
	var s savedCheck
	if err := json.Unmarshal(data, &s); err != nil || s.Repo != c.opts.Repo || s.Release == nil {
		return
	}
	if _, ok := parseVersion(s.Release.Tag); !ok {
		return
	}
	c.rel, c.etag, c.checkedAt = s.Release, s.ETag, s.CheckedAt
}

// save writes the last check to disk. A failure is only logged: the next run asks GitHub afresh.
func (c *Checker) save(s savedCheck) {
	data, err := json.Marshal(s)
	if err == nil {
		err = os.MkdirAll(filepath.Dir(c.savePath()), 0o755)
	}
	if err == nil {
		err = util.WriteFileAtomic(c.savePath(), data, 0o644)
	}
	if err != nil {
		slog.Debug("update: could not keep the last check", "error", err)
	}
}
