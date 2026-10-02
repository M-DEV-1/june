package cmd

import (
	"crypto/subtle"
	"net/http"
	"os"

	"june/internal/ipctoken"
)

// This file is the daemon's side of local IPC. The desktop window, the tray and the `june` command reach the daemon over HTTP on 127.0.0.1:<DaemonPort>, authenticated with a shared token file. Server side is requireIPCToken; client side is ipctoken.Attach.

// windowOrigins are the origins the packaged desktop window's webview sends. The Vite dev server used while working on the window is not in this list: a release daemon must never let a page on some local port read the store, so that origin is only honoured when JUNE_DEV_ORIGIN names it (see windowOriginAllowed).
var windowOrigins = map[string]bool{
	"tauri://localhost":       true,
	"http://tauri.localhost":  true,
	"https://tauri.localhost": true,
}

// windowOriginAllowed reports whether origin may read IPC responses from a browser context: the packaged window's origins always, plus exactly the origin named in the JUNE_DEV_ORIGIN environment variable (for example http://localhost:1420 while developing the window). Input: the request's Origin header. Output: true when CORS headers should name it back.
func windowOriginAllowed(origin string) bool {
	if windowOrigins[origin] {
		return true
	}
	dev := os.Getenv("JUNE_DEV_ORIGIN")
	return dev != "" && origin == dev
}

// requireIPCToken wraps an HTTP handler so it only runs when the request carries the correct ipctoken.HeaderName value. The compare is constant-time so a timing side-channel can't leak the token a byte at a time. Every daemon IPC handler except /ping is wrapped with this (see startDaemonServices); /ping is the pre-spawn liveness probe root.go polls before the token file is guaranteed to exist, and it reveals nothing.
// An empty token is always rejected, never compared: subtle.ConstantTimeCompare([]byte(""), []byte("")) returns 1, so without this guard a request with no header (or an explicit empty header) would authenticate whenever the daemon's own token is empty — e.g. ipctoken.Generate failed at startup (see startDaemonServices). That would fail auth open instead of leaving IPC unreachable as intended.
// On /events only, the token may also arrive as the query parameter "token", because a browser EventSource (the desktop window's /events stream) cannot set request headers; the header wins when both are present. Every other route accepts the header only, so the secret never lands in a URL an access log or a proxy records.
func requireIPCToken(token string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// The desktop window is a webview, so its requests carry an Origin and the browser discards the response unless that origin is named back. Only the window's own origins are named; any other page on this machine gets no CORS headers, so even with the token it cannot read a byte. A preflight needs no token because it carries no data.
		if origin := r.Header.Get("Origin"); origin != "" && windowOriginAllowed(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Headers", ipctoken.HeaderName+", Content-Type")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		got := r.Header.Get(ipctoken.HeaderName)
		if got == "" && r.URL.Path == "/events" {
			got = r.URL.Query().Get("token")
		}
		if token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// Every IPC body is a small JSON object, so a body past this cap is a mistake or an attack and the decoder fails on it instead of buffering it.
		r.Body = http.MaxBytesReader(w, r.Body, ipcBodyLimit)
		next(w, r)
	}
}

// ipcBodyLimit is the most bytes one IPC request body may carry: one mebibyte, hundreds of times the largest legitimate ask.
const ipcBodyLimit = 1 << 20

// authedDaemonGet fires a fire-and-forget authenticated GET at the local daemon. Used by the tray's pause/resume clicks on both platforms (tray_linux.go, tray_windows.go), which would otherwise each repeat the read-token-attach-header-GET dance.
func authedDaemonGet(url string) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return
	}
	ipctoken.Attach(req, ipctoken.DefaultPath)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}
