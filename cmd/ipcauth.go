package cmd

import (
	"crypto/subtle"
	"net/http"

	"ora/internal/ipctoken"
)

// requireIPCToken wraps an HTTP handler so it only runs when the request carries the correct ipctoken.HeaderName value — constant-time compare so a timing side-channel can't leak the token a byte at a time. Every daemon IPC handler except /ping is wrapped with this (see startDaemonServices); /ping is the pre-spawn liveness probe root.go polls before the token file is guaranteed to exist yet, and reveals nothing.
func requireIPCToken(token string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get(ipctoken.HeaderName)
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}
