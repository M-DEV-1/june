package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"june/internal/config"
	"june/internal/util"
)

// codexTokens is the tokens block of the Codex CLI auth file.
type codexTokens struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	AccountID    string `json:"account_id"`
}

// codexAuth is the shape of ~/.codex/auth.json as Codex CLI writes it.
type codexAuth struct {
	AuthMode    string      `json:"auth_mode"`
	Tokens      codexTokens `json:"tokens"`
	LastRefresh string      `json:"last_refresh"`
}

// CodexAuthPath is where the Codex CLI writes its login under the home directory home when CODEX_HOME is not set.
func CodexAuthPath(home string) string {
	return filepath.Join(home, ".codex", "auth.json")
}

// codexAuthPath returns where Codex CLI keeps its login: $CODEX_HOME/auth.json when set, else ~/.codex/auth.json.
func codexAuthPath() string {
	if home := os.Getenv("CODEX_HOME"); home != "" {
		return filepath.Join(home, "auth.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return CodexAuthPath(home)
}

// loadCodexAuth reads the Codex CLI auth file at path. Output: the parsed file, or an error when it is missing, malformed, or holds no access token.
func loadCodexAuth(path string) (codexAuth, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return codexAuth{}, fmt.Errorf("codex login: %w", err)
	}
	var auth codexAuth
	if err := json.Unmarshal(raw, &auth); err != nil {
		return codexAuth{}, fmt.Errorf("codex login: %s is not the auth file Codex CLI writes", path)
	}
	if auth.Tokens.AccessToken == "" {
		return codexAuth{}, fmt.Errorf("codex login: %s has no access token; run `codex login` first", path)
	}
	return auth, nil
}

// accountID returns the ChatGPT account id for the chatgpt-account-id header: the file's account_id when present, else the chatgpt_account_id claim read from the id_token, then the access_token.
func (t codexTokens) accountID() string {
	if t.AccountID != "" {
		return t.AccountID
	}
	for _, tok := range []string{t.IDToken, t.AccessToken} {
		if id := jwtAccountID(tok); id != "" {
			return id
		}
	}
	return ""
}

// jwtAccountID decodes the payload of an OpenAI JWT without verifying it and returns the chatgpt_account_id claim under https://api.openai.com/auth, or "" when absent.
func jwtAccountID(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return ""
	}
	var claims struct {
		Auth struct {
			AccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	return claims.Auth.AccountID
}

// codexInstallID returns a stable id for this installation, kept beside June's own data so the backend sees one installation rather than a new one per restart. Output: the stored id, a freshly made one, or "" when the data directory cannot be used.
func codexInstallID() string {
	dir := config.DataDir()
	if dir == "" {
		return ""
	}
	path := filepath.Join(dir, "codex-install-id")
	if raw, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(raw)); id != "" {
			return id
		}
	}
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return ""
	}
	id := hex.EncodeToString(buf[:])
	os.WriteFile(path, []byte(id), 0o600)
	return id
}

// codexSessionKey returns a fresh random key naming one ask, sent as the session id so the backend sees each ask as its own session. Output: a hex string, or "" when randomness is unavailable.
func codexSessionKey() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(buf[:])
}

// codexPromptCacheKeyOnce guards the fallback key, so a process that cannot read the installation id still sends one key rather than a new one per ask.
var (
	codexPromptCacheKeyOnce  sync.Once
	codexPromptCacheKeyValue string
)

// codexPromptCacheKey returns the key sent as prompt_cache_key, which is what routes a request to the machine already holding the prefix it starts with. It is deliberately the same on every ask: every ask now opens with the same instruction and the same tool definitions, about 9,000 tokens of them, and a key made fresh per ask sends the next ask to a machine that has never seen any of it. Output: the installation id, or a key made once for this process when there is none.
func codexPromptCacheKey() string {
	codexPromptCacheKeyOnce.Do(func() {
		codexPromptCacheKeyValue = codexInstallID()
		if codexPromptCacheKeyValue == "" {
			codexPromptCacheKeyValue = codexSessionKey()
		}
	})
	return codexPromptCacheKeyValue
}

// currentTokens returns the tokens to send, loading the auth file on first use and reloading it when the access token is at or near its expiry, since the Codex CLI may have refreshed it already.
func (c *codexClient) currentTokens() (codexTokens, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tokens == nil || c.expiringLocked() {
		auth, err := loadCodexAuth(c.AuthPath)
		if err != nil {
			if c.tokens != nil {
				return *c.tokens, nil
			}
			return codexTokens{}, err
		}
		c.tokens = &auth.Tokens
	}
	return *c.tokens, nil
}

// expiringLocked reports whether the access token held in memory is within the refresh window of its expiry; the caller must hold the mutex.
func (c *codexClient) expiringLocked() bool {
	exp := jwtExpiry(c.tokens.AccessToken)
	return !exp.IsZero() && time.Until(exp) <= codexRefreshWindow
}

// jwtExpiry decodes the payload of a JWT without verifying it and returns its exp claim as a time, or the zero time when there is none.
func jwtExpiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp == 0 {
		return time.Time{}
	}
	return time.Unix(claims.Exp, 0)
}

// writeCodexTokens merges refreshed tokens back into the Codex CLI auth file through a temp file and a rename, keeping every field June does not model, so a crash cannot leave the user with a truncated login. Input: the auth file path and the tokens to store. Output: an error when the file cannot be read or replaced.
func writeCodexTokens(path string, tok codexTokens) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var file map[string]json.RawMessage
	if err := json.Unmarshal(raw, &file); err != nil {
		return err
	}
	tokens, err := json.Marshal(tok)
	if err != nil {
		return err
	}
	file["tokens"] = tokens
	stamp, _ := json.Marshal(time.Now().UTC().Format(time.RFC3339Nano))
	file["last_refresh"] = stamp
	out, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	return util.WriteFileAtomic(path, out, 0o600)
}

// refresh trades the refresh token for a new access token at the OAuth token endpoint, the way Codex CLI does, and writes the rotated pair back to the auth file because OpenAI spends a refresh token on use and the CLI would otherwise find itself logged out. Input: the access token the failed request used, so a refresh another ask already did is not repeated. Output: an error when there is no refresh token or the endpoint refuses.
func (c *codexClient) refresh(ctx context.Context, stale string) error {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	tok, err := c.currentTokens()
	if err != nil {
		return err
	}
	// Another ask refreshed while this one waited for the lock, so its new token is the one to use rather than spending the rotated refresh token again.
	if stale != "" && tok.AccessToken != stale {
		return nil
	}
	if tok.RefreshToken == "" {
		return fmt.Errorf("%w: codex login: the token expired and there is no refresh token; run `codex login` again", ErrLoggedOut)
	}
	body, _ := json.Marshal(map[string]string{"client_id": codexClientID, "grant_type": "refresh_token", "refresh_token": tok.RefreshToken})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.TokenURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("codex login refresh: %w", err)
	}
	defer resp.Body.Close()
	// A 4xx is the endpoint refusing this refresh token, which no retry fixes: the login is dead. A 5xx is the endpoint itself being down, which says nothing about the login and must not grey the row out.
	if resp.StatusCode/100 == 4 {
		return fmt.Errorf("%w: codex login refresh: HTTP %d; run `codex login` again", ErrLoggedOut, resp.StatusCode)
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("codex login refresh: HTTP %d", resp.StatusCode)
	}
	var fresh struct {
		IDToken      string `json:"id_token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&fresh); err != nil || fresh.AccessToken == "" {
		return errors.New("codex login refresh: the reply carried no access token")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	next := tok
	next.AccessToken = fresh.AccessToken
	if fresh.IDToken != "" {
		next.IDToken = fresh.IDToken
	}
	if fresh.RefreshToken != "" {
		next.RefreshToken = fresh.RefreshToken
	}
	c.tokens = &next
	// The rotated tokens go back to the file the Codex CLI reads, so the user's own CLI keeps working; a failure here is logged rather than failing the ask, which can still proceed on the new access token.
	if err := writeCodexTokens(c.AuthPath, next); err != nil {
		slog.Warn("codex login refresh: could not write the refreshed tokens back", "path", c.AuthPath, "error", err)
	}
	return nil
}

// codexProfileURL is the account profile the Codex CLI's own /usage card reads (codex-rs/backend-client: `{base}/wham/profiles/me` on the ChatGPT path style). It answers a plain GET with the login's bearer token and account id, spends no model tokens and rotates nothing, so unlike a token refresh it can be called whenever the picker renders. It carries lifetime and daily token stats and no rate-limit windows: those stay in the headers of real calls.
const codexProfileURL = "https://chatgpt.com/backend-api/wham/profiles/me"

// codexLoginCheck asks the backend whether this login still works. Input: a context, the HTTP client, the profile endpoint and the CLI's auth file. Output: nil when the login answers, an error wrapping ErrLoggedOut when the backend refuses it, and a plain error when the request could not be made or the backend itself is down — which says nothing about the login and must not grey the row out.
func codexLoginCheck(ctx context.Context, client *http.Client, url, authPath string) error {
	tok, err := loadCodexAuth(authPath)
	if err != nil {
		return fmt.Errorf("%w: codex login: %v", ErrLoggedOut, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok.Tokens.AccessToken)
	req.Header.Set("ChatGPT-Account-Id", tok.Tokens.accountID())
	req.Header.Set("User-Agent", fmt.Sprintf("%s/%s (%s; %s)", codexOriginator, codexClientVersion, runtime.GOOS, runtime.GOARCH))
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("codex login check: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("%w: codex login check: HTTP %d; run `codex login` again", ErrLoggedOut, resp.StatusCode)
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("codex login check: HTTP %d", resp.StatusCode)
	}
	return nil
}

// codexLoginChecked is when the profile endpoint was last asked, so RefreshCodexLogin holds itself to one check every codexLoginPoll however often the picker asks.
var codexLoginChecked struct {
	sync.Mutex
	at time.Time
}

// codexLoginPoll matches the other providers' usage polls: the picker is the only caller, so the endpoint is touched only while someone is looking at it.
const codexLoginPoll = 10 * time.Minute

// RefreshCodexLogin checks the Codex login, at most once every ten minutes, and marks the row signed out when the backend refuses it or signed in again when it answers. GET /brains calls it. Input: a context. Output: none — a check that could not be made leaves the row alone.
func RefreshCodexLogin(ctx context.Context) {
	usageRecorder.Lock()
	to := usageRecorder.to
	usageRecorder.Unlock()
	if to == nil {
		return
	}
	codexLoginChecked.Lock()
	if time.Since(codexLoginChecked.at) < codexLoginPoll {
		codexLoginChecked.Unlock()
		return
	}
	codexLoginChecked.at = time.Now()
	codexLoginChecked.Unlock()

	ctx, cancel := context.WithTimeout(ctx, codexLoginCheckTimeout)
	defer cancel()
	err := codexLoginCheck(ctx, http.DefaultClient, codexProfileURL, codexAuthPath())
	if errors.Is(err, ErrLoggedOut) {
		to.RecordSignedOut(ProviderCodex, "the Codex login was refused: run codex login to sign in again")
		return
	}
	if err != nil {
		slog.Debug("codex: could not check the login", "error", err)
		return
	}
	// The login answered, which clears a signed-out mark an earlier refusal left; the check carries no windows, so the last ones read stay as they were.
	to.Record(ProviderCodex, nil)
}

// codexLoginCheckTimeout bounds the profile request.
const codexLoginCheckTimeout = 10 * time.Second
