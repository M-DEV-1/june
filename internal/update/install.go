package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// installerAsset is the Windows installer every release carries (packaging/windows/june.iss), and sumsAsset the checksums file the release pipeline writes over all of its assets.
	installerAsset = "June-Setup-x64.exe"
	sumsAsset      = "SHA256SUMS"
	// installerPrefix names a downloaded installer in <data>/update, followed by its version and ".exe".
	installerPrefix = "June-Setup-x64-"
	// downloadTimeout bounds the whole download, and stallTimeout a download that stops moving, so a dead connection fails the banner rather than leaving it at the same percentage for half an hour.
	downloadTimeout = 30 * time.Minute
	stallTimeout    = 60 * time.Second
	// maxInstaller refuses an asset claiming to be larger than any installer June ships, before a byte of it is written to disk.
	maxInstaller = 512 << 20
	// maxSums bounds the checksums file, which lists a handful of assets.
	maxSums = 1 << 20
	// progressEvery is the most often a download reports how far it has got, as the component downloader does.
	progressEvery = 250 * time.Millisecond
)

// install asks GitHub for the latest release again, downloads its installer, verifies it and starts it; the installer then stops this daemon, replaces June's files and starts the new June. Input: the daemon's context, which ends the download if the daemon stops first, and the version the window offered, which names the events until the new answer names another. Output: none; every outcome is an "update" event and the state GET /update reports.
func (c *Checker) install(ctx context.Context, ver string) {
	rel, err := c.freshRelease(ctx)
	if err != nil {
		c.fail(ver, err, "")
		return
	}
	ver = rel.version()
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	path, err := c.fetchInstaller(ctx, rel)
	if err != nil {
		c.fail(ver, err, "")
		return
	}
	c.setState(stateReady)
	// Asked again here because a download takes minutes and a meeting may have started since the button was pressed. The verified installer stays in <data>/update, so trying again afterwards starts it at once.
	if code := c.blocker(); code != "" {
		c.fail(ver, &userError{shown: "The update is downloaded and waiting. " + busyRefusal(code)}, "")
		return
	}
	c.publish(ver, "installing", "", false)
	c.setState(stateInstalling)
	logPath := filepath.Join(filepath.Dir(path), "install.log")
	wait, err := startInstaller(path, logPath)
	if err != nil {
		c.fail(ver, &userError{"Couldn't start the installer.", err}, "")
		return
	}
	slog.Info("update: installer started; it stops June, installs the new version and opens it again", "version", ver, "log", logPath)
	// Normally the installer stops this daemon before it exits, and this wait never returns. Coming back means it gave up, or finished without managing to stop the copy that is running.
	go func() {
		err := wait()
		if err != nil {
			c.fail(ver, &userError{"The update didn't install, so June is still on " + c.opts.Version + ". Try again, or download it from the release page.", err}, logPath)
			return
		}
		c.fail(ver, &userError{shown: "June " + ver + " is installed, but the June that is running is still the old one. Quit June from its tray icon and open it again."}, logPath)
	}()
}

// freshRelease checks GitHub again before anything is downloaded and returns the release to install. The answer the banner was built from can be a day old, or older on a machine that was offline: a build fixed since by uploading its installer and SHA256SUMS again under the same tag would fail the checks below, on every retry, as if it had been tampered with, and a release out since then would be passed over. With the ETag held it is a conditional request of a few hundred bytes. Output: the release, or why there is none to install, as the user reads it.
func (c *Checker) freshRelease(ctx context.Context) (*release, error) {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	if err := c.check(ctx); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case !c.availableLocked():
		return nil, &userError{shown: "GitHub no longer lists a June newer than this one, so there is nothing to install."}
	case !c.installable(c.rel):
		return nil, &userError{shown: "The latest release has no Windows installer, or no checksums to check one against. Download it from the release page."}
	}
	return c.rel, nil
}

// fetchInstaller downloads rel's installer into <data>/update and checks it against the release's SHA256SUMS, against GitHub's own digest of the upload when it has one, and against the publisher who signed the June that is running (samePublisher), who is read before any download (ownPublisher). An installer already there from an earlier try is reused when it still matches. Output: the verified installer's path, or the error; a file that does not match is deleted, never run.
func (c *Checker) fetchInstaller(ctx context.Context, rel *release) (string, error) {
	ver := rel.version()
	inst, sums := rel.asset(installerAsset), rel.asset(sumsAsset)
	if inst == nil || sums == nil {
		return "", &userError{shown: "This release has no Windows installer or no checksums to check one against."}
	}
	for _, a := range []*asset{inst, sums} {
		if !c.fromRelease(a.URL, rel) {
			return "", &userError{shown: "The release lists a download that isn't on its own GitHub release page, so June won't fetch it."}
		}
	}
	if inst.Size <= 0 || inst.Size > maxInstaller {
		return "", &userError{shown: fmt.Sprintf("The release says its installer is %d bytes, which can't be right.", inst.Size)}
	}
	ours, err := ownPublisher()
	if err != nil {
		return "", err
	}
	want, err := c.expectedSum(ctx, sums)
	if err != nil {
		return "", err
	}
	if d, ok := strings.CutPrefix(inst.Digest, "sha256:"); ok && !strings.EqualFold(d, want) {
		return "", &userError{shown: "The release's SHA256SUMS and GitHub disagree about the installer's checksum, so June won't run it."}
	}
	dir := filepath.Join(c.opts.DataDir, "update")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", &userError{"Couldn't make a folder to download the update into.", err}
	}
	// The checksum is all that marks a finished download: a file cut short by a crash hashes to something else and is fetched again.
	path := filepath.Join(dir, installerPrefix+ver+".exe")
	got, err := fileSum(path)
	if err != nil || got != want {
		got, err = c.download(ctx, ver, inst, path)
		if err != nil {
			os.Remove(path)
			return "", err
		}
	}
	c.publish(ver, "verifying", "", false)
	if got != want {
		os.Remove(path)
		return "", &userError{shown: "The downloaded installer didn't match the release's checksum, so it was deleted and not run."}
	}
	if err := samePublisher(path, ours); err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}

// fromRelease reports whether url is a download from rel's own release on this repo; GitHub redirects such a URL to its storage, and the client follows. GitHub names are not case-sensitive, so neither is the compare.
func (c *Checker) fromRelease(url string, rel *release) bool {
	prefix := "https://github.com/" + c.opts.Repo + "/releases/download/" + rel.Tag + "/"
	return len(url) > len(prefix) && strings.EqualFold(url[:len(prefix)], prefix)
}

// expectedSum downloads the release's SHA256SUMS and finds the installer's line in it. Output: the checksum as lowercase hex, or the error.
func (c *Checker) expectedSum(ctx context.Context, sums *asset) (string, error) {
	// A file this small is done in a moment or not at all; without its own bound a stalled connection would hold the banner at "downloading", with no progress to show, for the whole download's half hour.
	ctx, cancel := context.WithTimeout(ctx, stallTimeout)
	defer cancel()
	resp, err := c.get(ctx, sums.URL)
	if err != nil {
		return "", &userError{"Couldn't download the release's checksums.", err}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSums))
	if err != nil {
		return "", &userError{"Couldn't download the release's checksums.", err}
	}
	sum, ok := sumFor(data, installerAsset)
	if !ok {
		return "", &userError{shown: "The release's SHA256SUMS has no line for " + installerAsset + "."}
	}
	return sum, nil
}

// sumFor finds name's checksum in a file in sha256sum's format, "<hex>  <name>" with a "*" before the name in binary mode. Output: the checksum as lowercase hex, and whether a well-formed line for name was there.
func sumFor(sums []byte, name string) (string, bool) {
	for line := range bytes.Lines(sums) {
		fields := strings.Fields(string(line))
		if len(fields) != 2 || strings.TrimPrefix(strings.TrimPrefix(fields[1], "*"), "./") != name {
			continue
		}
		if b, err := hex.DecodeString(fields[0]); err == nil && len(b) == sha256.Size {
			return strings.ToLower(fields[0]), true
		}
	}
	return "", false
}

// download fetches a into dst, hashing it on the way and publishing progress. Input: a context, the version for the events, the asset and the file to write. Output: the SHA256 of what was written as lowercase hex, or the error; a short or long file is an error, whatever its hash.
func (c *Checker) download(ctx context.Context, ver string, a *asset, dst string) (string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stall := time.AfterFunc(stallTimeout, cancel)
	defer stall.Stop()
	resp, err := c.get(ctx, a.URL)
	if err != nil {
		return "", &userError{"Couldn't download the update.", err}
	}
	defer resp.Body.Close()
	f, err := os.Create(dst)
	if err != nil {
		return "", &userError{"Couldn't save the update.", err}
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 256<<10)
	var done int64
	last := time.Time{}
	progress := func() {
		c.publish(ver, "downloading", fmt.Sprintf(`{"done":%d,"total":%d}`, done, a.Size), false)
	}
	progress()
	body := io.LimitReader(resp.Body, a.Size+1)
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			stall.Reset(stallTimeout)
			if _, err := f.Write(buf[:n]); err != nil {
				return "", &userError{"Couldn't save the update.", err}
			}
			h.Write(buf[:n])
			done += int64(n)
			if time.Since(last) >= progressEvery {
				progress()
				last = time.Now()
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", &userError{"The download stopped before it finished.", rerr}
		}
	}
	progress()
	if err := f.Close(); err != nil {
		return "", &userError{"Couldn't save the update.", err}
	}
	if done != a.Size {
		return "", &userError{shown: fmt.Sprintf("The download was %d bytes; the release says %d.", done, a.Size)}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// get starts a GET of a release download. Output: the response, whose body the caller closes, or an error for anything but 200.
func (c *Checker) get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.userAgent())
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("%s answered %s", resp.Request.URL.Host, resp.Status)
	}
	return resp, nil
}

// fileSum hashes the file at path. Output: its SHA256 as lowercase hex, or the error from reading it.
func fileSum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// fail records a failed update and tells the window. Input: the version it was for, the error, and the installer's log when it got as far as running, "" otherwise; the log goes in the event's detail and the daemon's log, not in the sentence the user reads.
func (c *Checker) fail(ver string, err error, logPath string) {
	slog.Warn("update: failed", "version", ver, "error", err, "installer_log", logPath)
	c.mu.Lock()
	c.state, c.err = stateFailed, shown(err)
	c.mu.Unlock()
	d := map[string]string{"error": shown(err)}
	if logPath != "" {
		d["log"] = logPath
	}
	detail, _ := json.Marshal(d)
	c.publish(ver, "failed", string(detail), true)
}

// setState moves the update along.
func (c *Checker) setState(s string) {
	c.mu.Lock()
	c.state = s
	c.mu.Unlock()
}

// removeOldInstallers deletes installers left in <data>/update by an earlier run: the one that updated June to this version has done its job, and any other is fetched again if it is wanted. One the finishing installer still holds open stays until the next start.
func (c *Checker) removeOldInstallers() {
	matches, _ := filepath.Glob(filepath.Join(c.opts.DataDir, "update", installerPrefix+"*"))
	for _, m := range matches {
		os.Remove(m)
	}
}
