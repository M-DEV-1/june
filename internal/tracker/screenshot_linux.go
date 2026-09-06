//go:build linux

package tracker

import (
	"context"
	"fmt"
	"image"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/jezek/xgb"
	"github.com/jezek/xgb/randr"
	"github.com/jezek/xgb/xproto"
)

var screenshotSeq atomic.Uint64

// xdg-desktop-portal PermissionStore: where the compositor records the user's screenshot allow/deny decision. Unsandboxed binaries (us) get the empty app-id.
const (
	permStoreService = "org.freedesktop.impl.portal.PermissionStore"
	permStorePath    = "/org/freedesktop/impl/portal/PermissionStore"
	permStoreIface   = "org.freedesktop.impl.portal.PermissionStore"
	screenshotTable  = "screenshot"
	screenshotID     = "screenshot"
)

// screenshotApps are the app-ids a stored screenshot decision can land under for us: "" because we run unsandboxed, plus our own name as a defensive fallback.
var screenshotApps = []string{"", "ora"}

// gnome-shell's own screenshot API, which takes a flash flag the xdg-desktop-portal API does not expose. The portal on GNOME just calls this method with flash=true, which is where the full-screen white flash on every vision capture comes from.
// gnome-shell only accepts this call from a short list of well-known bus names (settings-daemon media keys, the GNOME and GTK portal backends, and org.gnome.Screenshot). org.gnome.Screenshot is the one we can claim: it belongs to the standalone gnome-screenshot tool, which is not installed on a default GNOME 46 desktop.
// Tradeoff: while ORA runs, gnome-screenshot would fail to take the name and refuse to start. We ask with DoNotQueue so we never steal it from a running instance, and fall back to the portal if it is already owned.
const (
	shellScreenshotName   = "org.gnome.Screenshot"
	shellScreenshotPath   = "/org/gnome/Shell/Screenshot"
	shellScreenshotMethod = "org.gnome.Shell.Screenshot.Screenshot"
)

// screenshotShell captures the whole screen straight through gnome-shell and returns the PNG bytes. No flash, no shutter animation, no consent dialog.
// Returns an error on any non-GNOME desktop, or when org.gnome.Screenshot is already owned — callers fall back to the portal.
func screenshotShell(ctx context.Context) ([]byte, error) {
	conn, err := dbus.SessionBus()
	if err != nil {
		return nil, fmt.Errorf("session bus: %w", err)
	}

	// Idempotent and cheap: a name we already hold comes back as AlreadyOwner, so this re-arms itself if the shared bus connection was ever replaced.
	reply, err := conn.RequestName(shellScreenshotName, dbus.NameFlagDoNotQueue)
	if err != nil {
		return nil, fmt.Errorf("request %s: %w", shellScreenshotName, err)
	}
	if reply != dbus.RequestNameReplyPrimaryOwner && reply != dbus.RequestNameReplyAlreadyOwner {
		return nil, fmt.Errorf("%s unavailable (reply %d)", shellScreenshotName, reply)
	}

	path, cleanup, err := shotTempPath()
	if err != nil {
		return nil, err
	}
	defer cleanup()

	// Signature is (include_cursor, flash, filename) -> (success, filename_used). flash=false is the whole point of this path.
	var ok bool
	var used string
	if err := conn.Object("org.gnome.Shell", shellScreenshotPath).
		CallWithContext(ctx, shellScreenshotMethod, 0, false, false, path).Store(&ok, &used); err != nil {
		return nil, fmt.Errorf("shell screenshot: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("shell screenshot reported failure")
	}

	data, err := os.ReadFile(used)
	if err != nil {
		return nil, fmt.Errorf("read screenshot %q: %w", used, err)
	}
	if used != path {
		os.Remove(used) //nolint:errcheck
	}
	return data, nil
}

// shotTempPath makes a private directory for gnome-shell to write one screenshot into. Input: none. Output: the path to hand the shell, a function that removes the file and the directory, and an error when the directory cannot be made.
// The mode has to sit on the directory rather than the file: gnome-shell creates the PNG itself, under its own umask, so a file we pre-created 0600 can come back 0644 and a picture of the whole desktop is readable by every other account on the machine for as long as it is on disk.
func shotTempPath() (string, func(), error) {
	dir, err := os.MkdirTemp("", "ora-shot-")
	if err != nil {
		return "", nil, fmt.Errorf("temp dir: %w", err)
	}
	// MkdirTemp already makes the directory 0700, but the mode is set again so a change there cannot silently widen it.
	if err := os.Chmod(dir, 0o700); err != nil {
		os.RemoveAll(dir) //nolint:errcheck
		return "", nil, fmt.Errorf("temp dir mode: %w", err)
	}
	return filepath.Join(dir, "screen.png"), func() { os.RemoveAll(dir) }, nil //nolint:errcheck — best-effort cleanup
}

// screenshotGranted reports whether the stored permissions already allow screenshots, so warm-up can skip prompting. Portal stores ["yes"] for allow.
func screenshotGranted(perms map[string][]string) bool {
	for _, app := range screenshotApps {
		for _, p := range perms[app] {
			if p == "yes" {
				return true
			}
		}
	}
	return false
}

// screenshotDenied reports a stored ["no"] — a stale deny silently auto-rejects future calls and suppresses the consent dialog, so warm-up clears it first.
func screenshotDenied(perms map[string][]string) bool {
	for _, app := range screenshotApps {
		for _, p := range perms[app] {
			if p == "no" {
				return true
			}
		}
	}
	return false
}

// warmUpConsentWait is how long the startup warm-up waits for the whole permission check, including the consent dialog. Two minutes is long enough for a person to notice the dialog and answer it; past that the warm-up gives up and the first real vision capture prompts again.
const warmUpConsentWait = 2 * time.Minute

// WarmUpScreenshotPermission triggers the screenshot consent dialog at daemon startup, so the user grants permission up front instead of the first vision capture silently failing later.
// No-op if already granted; a stale deny is cleared first so the portal prompts again instead of auto-rejecting. Blocks on the dialog, so callers run it in a goroutine.
func WarmUpScreenshotPermission(ctx context.Context) {
	// The warm-up gets its own deadline rather than living on the daemon's root context: the consent dialog is the one call here that waits on a person, and a dialog nobody ever answers used to keep this goroutine and its portal request alive for the life of the process.
	ctx, cancel := context.WithTimeout(ctx, warmUpConsentWait)
	defer cancel()
	// The gnome-shell path asks no permission of anyone, so if it works there is nothing to warm up and popping the portal dialog would be pointless.
	if _, err := screenshotShell(ctx); err == nil {
		slog.Info("vision warm-up: gnome-shell screenshot available, no consent needed")
		return
	}

	conn, err := dbus.SessionBus()
	if err != nil {
		slog.Warn("vision warm-up: session bus unavailable", "error", err)
		return
	}

	store := conn.Object(permStoreService, permStorePath)
	var perms map[string][]string
	var data dbus.Variant
	lookupErr := store.CallWithContext(ctx, permStoreIface+".Lookup", 0, screenshotTable, screenshotID).Store(&perms, &data)

	if lookupErr == nil && screenshotGranted(perms) {
		slog.Info("vision warm-up: screenshot permission already granted")
		return
	}

	if lookupErr == nil && screenshotDenied(perms) {
		if err := store.CallWithContext(ctx, permStoreIface+".DeletePermission", 0, screenshotTable, screenshotID, "").Err; err != nil {
			slog.Warn("vision warm-up: could not clear stale deny", "error", err)
		}
	}

	if _, err := grabScreen(ctx); err != nil {
		slog.Warn("vision warm-up: consent screenshot failed (user may have declined)", "error", err)
		return
	}
	slog.Info("vision warm-up: screenshot permission granted")
}

// screenLayout reports the monitor rectangles making up the desktop canvas, plus the pointer position, in the same coordinate space as a whole-screen screenshot. It reads them from the X RandR extension, which under Wayland answers through XWayland — mutter keeps that in step with the real monitor layout.
// Returns (nil, (-1,-1)) when X or RandR is unreachable (headless, or a compositor with no XWayland), which leaves stored frames whole-canvas.
func screenLayout() ([]image.Rectangle, image.Point) {
	unknown := image.Pt(-1, -1)
	conn, err := xgb.NewConn()
	if err != nil {
		return nil, unknown
	}
	defer conn.Close()
	if err := randr.Init(conn); err != nil {
		return nil, unknown
	}

	root := xproto.Setup(conn).DefaultScreen(conn).Root
	reply, err := randr.GetMonitors(conn, root, true).Reply()
	if err != nil {
		return nil, unknown
	}
	mons := make([]image.Rectangle, 0, len(reply.Monitors))
	for _, m := range reply.Monitors {
		mons = append(mons, image.Rect(int(m.X), int(m.Y), int(m.X)+int(m.Width), int(m.Y)+int(m.Height)))
	}

	// ponytail: the pointer stands in for the focused window, whose position no Wayland API will give us — gnome-shell refuses Introspect.GetWindows to unlisted app-ids, and AT-SPI reports no screen coordinates for Wayland clients. This picks the wrong primary monitor only when the pointer rests on a different screen than the keyboard focus, and every monitor is captured either way. Upgrade path: read the focused window's rectangle if a compositor ever exposes it.
	pointer := unknown
	if p, err := xproto.QueryPointer(conn, root).Reply(); err == nil {
		pointer = image.Pt(int(p.RootX), int(p.RootY))
	}
	return mons, pointer
}

// grabScreen returns a PNG of the current screen for the vision tier.
// Prefers gnome-shell's direct API because it captures silently and invisibly; falls back to the portal on other compositors, which flashes on GNOME but at least works everywhere.
func grabScreen(ctx context.Context) ([]byte, error) {
	defer standAside()()
	png, err := screenshotShell(ctx)
	if err == nil {
		return png, nil
	}
	slog.Debug("vision: gnome-shell screenshot unavailable, falling back to portal", "error", err)
	return screenshotPortal(ctx)
}

// screenshotPortal captures the whole screen via xdg-desktop-portal and returns the PNG bytes. First call on GNOME prompts for permission; once granted the compositor remembers it. interactive=false so no region picker appears.
// Portal flow: call Screenshot -> get a Request object path -> wait for its Response signal -> read the returned file:// URI.
func screenshotPortal(ctx context.Context) ([]byte, error) {
	conn, err := dbus.SessionBus()
	if err != nil {
		return nil, fmt.Errorf("session bus: %w", err)
	}

	// Predict the Request object path so we can subscribe before calling and
	// avoid missing a fast Response. Format per the portal spec:
	// /org/freedesktop/portal/desktop/request/SENDER/TOKEN
	// where SENDER is our unique name sans leading ':' with '.' -> '_'.
	sender := strings.TrimPrefix(conn.Names()[0], ":")
	sender = strings.ReplaceAll(sender, ".", "_")
	token := fmt.Sprintf("ora_shot_%d", screenshotSeq.Add(1))
	handlePath := dbus.ObjectPath(
		"/org/freedesktop/portal/desktop/request/" + sender + "/" + token,
	)

	if err := conn.AddMatchSignal(
		dbus.WithMatchObjectPath(handlePath),
		dbus.WithMatchInterface("org.freedesktop.portal.Request"),
		dbus.WithMatchMember("Response"),
	); err != nil {
		return nil, fmt.Errorf("subscribe response: %w", err)
	}

	sigCh := make(chan *dbus.Signal, 4)
	conn.Signal(sigCh)
	// The unsubscribe is a named function rather than two defers because the timeout path below hands it to a goroutine that outlives this call: removing the match while that goroutine is still waiting would mean it never sees the response and never removes the file.
	unsubscribe := func() {
		conn.RemoveSignal(sigCh)
		conn.RemoveMatchSignal( //nolint:errcheck
			dbus.WithMatchObjectPath(handlePath),
			dbus.WithMatchInterface("org.freedesktop.portal.Request"),
			dbus.WithMatchMember("Response"),
		)
	}
	detached := false
	defer func() {
		if !detached {
			unsubscribe()
		}
	}()

	portal := conn.Object("org.freedesktop.portal.Desktop", "/org/freedesktop/portal/desktop")
	options := map[string]dbus.Variant{
		"handle_token": dbus.MakeVariant(token),
		"interactive":  dbus.MakeVariant(false),
		"modal":        dbus.MakeVariant(false),
	}

	var returned dbus.ObjectPath
	if err := portal.CallWithContext(ctx, "org.freedesktop.portal.Screenshot.Screenshot", 0, "", options).Store(&returned); err != nil {
		return nil, fmt.Errorf("call screenshot: %w", err)
	}

	for {
		select {
		case <-ctx.Done():
			// The portal is still working and will write the PNG regardless, so the request is drained in the background and the file it names removed. Without this a picture of the whole desktop stays in /run/user/<uid>/doc for good every time the capture bound wins.
			detached = true
			go func() {
				defer unsubscribe()
				discardLateShot(sigCh, handlePath, returned, lateShotWait)
			}()
			return nil, ctx.Err()
		case sig := <-sigCh:
			if sig.Path != handlePath && sig.Path != returned {
				continue
			}
			uri, err := responseURI(sig.Body)
			if err != nil {
				return nil, err
			}
			return readFileURI(uri)
		}
	}
}

// lateShotWait is how long the background drain waits for a portal screenshot the capture already gave up on. Thirty seconds: a portal that has not answered by then is not going to write a file either.
const lateShotWait = 30 * time.Second

// discardLateShot waits for the Response of a portal screenshot request the caller abandoned and removes the file it names. Input: the signal channel the request was subscribed on, the two object paths that identify it, and how long to wait. Output: none — a request that answers nothing, or answers a failure, just ends the wait.
func discardLateShot(sigCh <-chan *dbus.Signal, handlePath, returned dbus.ObjectPath, wait time.Duration) {
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		select {
		case <-deadline.C:
			return
		case sig := <-sigCh:
			if sig == nil {
				return
			}
			if sig.Path != handlePath && sig.Path != returned {
				continue
			}
			uri, err := responseURI(sig.Body)
			if err != nil {
				return
			}
			path := strings.TrimPrefix(uri, "file://")
			if err := os.Remove(path); err != nil {
				slog.Debug("vision: could not remove the screenshot of an abandoned portal request", "path", path, "error", err)
			}
			return
		}
	}
}

// responseURI parses a portal Request.Response signal body: (u code, a{sv} results). code 0 = success; results["uri"] holds the screenshot file URI.
func responseURI(body []interface{}) (string, error) {
	if len(body) != 2 {
		return "", fmt.Errorf("unexpected response body len %d", len(body))
	}
	code, _ := body[0].(uint32)
	if code != 0 {
		return "", fmt.Errorf("screenshot denied or cancelled (code %d)", code)
	}
	results, ok := body[1].(map[string]dbus.Variant)
	if !ok {
		return "", fmt.Errorf("response results not a dict")
	}
	v, ok := results["uri"]
	if !ok {
		return "", fmt.Errorf("response missing uri")
	}
	uri, _ := v.Value().(string)
	if uri == "" {
		return "", fmt.Errorf("empty uri")
	}
	return uri, nil
}

// readFileURI reads the bytes behind a file:// URI and removes the temp file the portal created.
func readFileURI(uri string) ([]byte, error) {
	path := strings.TrimPrefix(uri, "file://")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read screenshot %q: %w", path, err)
	}
	os.Remove(path) //nolint:errcheck — best-effort cleanup of portal temp file
	return data, nil
}
