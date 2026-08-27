package cmd

import (
	"encoding/json"
	"net/http"
	"time"

	"ora/internal/ipctoken"
	"ora/internal/tracker"
)

// bufferProvider GETs the daemon's /buffer endpoint over authed IPC — this is what lets the client process's Agent.buildHandshakeContext see current activity (the "[working]" lines) at all; before this, Agent.Connect's compiler was always nil client-side and /buffer had zero consumers (F2).
type bufferProvider struct {
	baseURL   string
	client    *http.Client
	tokenPath string
}

// newBufferProvider builds a bufferProvider pointed at the daemon's IPC server on 127.0.0.1:<DaemonPort>. A short (300ms) client timeout is deliberate: this feeds Agent.Connect's handshake, and a dead/slow daemon must not delay the live session connecting.
func newBufferProvider() *bufferProvider {
	return &bufferProvider{
		baseURL:   "http://127.0.0.1:" + DaemonPort,
		client:    &http.Client{Timeout: 300 * time.Millisecond},
		tokenPath: ipctoken.DefaultPath,
	}
}

// Get returns the daemon's current activity buffer, or nil on any failure (unreachable daemon, timeout, non-200, bad JSON) — matches Agent.buildHandshakeContext's own "nil bufferProvider output is a no-op" handling, so a dead daemon just means the handshake proceeds without this context instead of blocking or erroring.
func (b *bufferProvider) Get() []tracker.Activity {
	req, err := http.NewRequest(http.MethodGet, b.baseURL+"/buffer", nil)
	if err != nil {
		return nil
	}
	if token, err := ipctoken.Read(b.tokenPath); err == nil {
		req.Header.Set(ipctoken.HeaderName, token)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var buf []tracker.Activity
	if err := json.NewDecoder(resp.Body).Decode(&buf); err != nil {
		return nil
	}
	return buf
}
