//go:build linux

package tracker

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"

	"github.com/godbus/dbus/v5"
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

// WarmUpScreenshotPermission triggers the screenshot consent dialog at daemon startup, so the user grants permission up front instead of the first vision capture silently failing later.
// No-op if already granted; a stale deny is cleared first so the portal prompts again instead of auto-rejecting. Blocks on the dialog, so callers run it in a goroutine.
func WarmUpScreenshotPermission(ctx context.Context) {
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

// grabScreen returns a PNG of the current screen for the vision tier.
func grabScreen(ctx context.Context) ([]byte, error) {
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
	defer conn.RemoveMatchSignal( //nolint:errcheck
		dbus.WithMatchObjectPath(handlePath),
		dbus.WithMatchInterface("org.freedesktop.portal.Request"),
		dbus.WithMatchMember("Response"),
	)

	sigCh := make(chan *dbus.Signal, 4)
	conn.Signal(sigCh)
	defer conn.RemoveSignal(sigCh)

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
