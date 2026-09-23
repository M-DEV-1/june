package agent

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// The body cli-chat-proxy.grok.com/v1/billing?format=credits answered on 2026-09-15, trimmed to the fields read. Grok meters one window, a weekly one, which is why its row drew an empty bar and a note saying the CLI exposes no usage: the endpoint was simply not known until the statusline the user had Grok write for itself named it.
const grokBillingBody = `{"config":{
	"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-09-15T10:46:41.506403+00:00","end":"2026-09-22T10:46:41.506403+00:00"},
	"creditUsagePercent":5.0,
	"productUsage":[{"product":"GrokBuild","usagePercent":5.0}]}}`

// grokAuthFile writes an auth.json shaped like the real one: an object keyed by issuer and session id, whose first entry carries the bearer key. Output: its path.
func grokAuthFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestGrokUsage_ReadsTheWeeklyWindowWithTheLoginsOwnToken(t *testing.T) {
	auth := grokAuthFile(t, `{"https://auth.x.ai::abc":{"key":"the-token","email":"someone@example.com"}}`)
	var sent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent = r.Header.Get("Authorization")
		w.Write([]byte(grokBillingBody))
	}))
	defer srv.Close()

	limits, err := grokUsage(t.Context(), srv.Client(), srv.URL, auth)
	if err != nil {
		t.Fatal(err)
	}
	if sent != "Bearer the-token" {
		t.Errorf("Authorization = %q, want the login's own bearer token", sent)
	}
	if len(limits) != 1 {
		t.Fatalf("read %d windows, want the one weekly window grok meters: %+v", len(limits), limits)
	}
	if limits[0].Window != "weekly" {
		t.Errorf("window = %q, want weekly", limits[0].Window)
	}
	if limits[0].UsedFraction != 0.05 {
		t.Errorf("used = %v, want 0.05 for creditUsagePercent 5.0", limits[0].UsedFraction)
	}
	if limits[0].ResetsAt.IsZero() {
		t.Error("no reset time, so the bar cannot say when it refills")
	}
}
