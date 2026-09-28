// grok.go reads what the user's Grok plan has spent, from the same billing endpoint the CLI's own /usage reads.
// Grok reports nothing about its allowance through the command line — no subcommand prints it, and unlike agy it hands its statusline no quota either (its own statusline script says so and fetches this endpoint instead). So the reading comes from the endpoint directly, with the bearer token out of the login the CLI already wrote, which is the same shape the Claude usage reader has: a file the user's own CLI maintains, read to answer a question the CLI will not answer itself.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// grokBillingURL is the endpoint the Grok CLI reads its own /usage from.
const grokBillingURL = "https://cli-chat-proxy.grok.com/v1/billing?format=credits"

// grokUsagePoll is how often the endpoint may be read however often the picker asks, and grokUsageTimeout bounds one read. Both match the Claude reader's, for the same reason: GET /brains calls this, so it is read only while somebody is looking at the picker.
const (
	grokUsagePoll    = 10 * time.Minute
	grokUsageTimeout = 10 * time.Second
)

// grokAuthPath is where the Grok CLI keeps the login it writes at sign-in.
func grokAuthPath(home string) string { return home + "/.grok/auth.json" }

// grokToken reads the bearer token out of the Grok CLI's login file. Input: the path to auth.json. Output: the token, or an error when the file is missing, is not JSON, holds no session, or no session carries a key. The file is an object keyed by issuer and session id with one entry per login; the first entry in the file that carries a key is the session the CLI itself uses, so the entries are read in file order.
func grokToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("grok usage: no login to read: %w", err)
	}
	notTheShape := errors.New("grok usage: the login file is not the shape this reads")
	dec := json.NewDecoder(bytes.NewReader(data))
	if open, _ := dec.Token(); open != json.Delim('{') {
		return "", notTheShape
	}
	for dec.More() {
		// The issuer-and-session key comes first, then its login.
		var session struct {
			Key string `json:"key"`
		}
		if _, err := dec.Token(); err != nil {
			return "", notTheShape
		}
		if err := dec.Decode(&session); err != nil {
			return "", notTheShape
		}
		if session.Key != "" {
			return session.Key, nil
		}
	}
	return "", fmt.Errorf("grok usage: the login file names no session with a key")
}

// grokUsage reads the plan's allowance from the billing endpoint. Input: a context, the HTTP client, the endpoint and the login file. Output: the one weekly window grok meters, or an error.
// Grok bills on credits rather than on request windows, so there is one number: how much of the period's credit is spent. The period is weekly on this plan and the response says so rather than it being assumed here.
func grokUsage(ctx context.Context, client *http.Client, url, authPath string) ([]UsageLimit, error) {
	token, err := grokToken(authPath)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("grok usage: building the request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("grok usage: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("%w: grok usage: the endpoint answered %d", ErrLoggedOut, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("grok usage: the endpoint answered %d", resp.StatusCode)
	}
	var body struct {
		Config struct {
			CurrentPeriod struct {
				Type string    `json:"type"`
				End  time.Time `json:"end"`
			} `json:"currentPeriod"`
			CreditUsagePercent float64 `json:"creditUsagePercent"`
		} `json:"config"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("grok usage: the endpoint did not answer with a billing period: %w", err)
	}
	return []UsageLimit{{
		Window:       grokWindowName(body.Config.CurrentPeriod.Type),
		UsedFraction: body.Config.CreditUsagePercent / 100,
		ResetsAt:     body.Config.CurrentPeriod.End,
		Source:       "cli-chat-proxy /v1/billing creditUsagePercent",
	}}, nil
}

// grokWindowName is the window name for one of the endpoint's period types. Input: the type as the endpoint spells it. Output: the name the picker draws, which is the endpoint's own suffix lowercased so a period type this has never seen still reads correctly rather than being called weekly.
func grokWindowName(periodType string) string {
	const prefix = "USAGE_PERIOD_TYPE_"
	if len(periodType) > len(prefix) && periodType[:len(prefix)] == prefix {
		return strings.ToLower(periodType[len(prefix):])
	}
	return "weekly"
}

// grokUsagePolled is when the endpoint was last read, so RefreshGrokUsage holds itself to grokUsagePoll however often the picker asks.
var grokUsagePolled struct {
	sync.Mutex
	at time.Time
}

// RefreshGrokUsage reads the Grok plan's allowance into the recorder set by SetUsageRecorder, at most once every ten minutes. GET /brains calls it, so the endpoint is only read while someone is looking at the picker. Input: a context. Output: none — a failure leaves the last good reading standing and says why at debug level.
func RefreshGrokUsage(ctx context.Context) {
	usageRecorder.Lock()
	to := usageRecorder.to
	usageRecorder.Unlock()
	if to == nil {
		return
	}
	grokUsagePolled.Lock()
	if time.Since(grokUsagePolled.at) < grokUsagePoll {
		grokUsagePolled.Unlock()
		return
	}
	grokUsagePolled.at = time.Now()
	grokUsagePolled.Unlock()

	ctx, cancel := context.WithTimeout(ctx, grokUsageTimeout)
	defer cancel()
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	limits, err := grokUsage(ctx, http.DefaultClient, grokBillingURL, grokAuthPath(home))
	if errors.Is(err, ErrLoggedOut) {
		to.RecordSignedOut(ProviderGrok, "the Grok login was refused: run grok in a terminal to sign in again")
		return
	}
	if err != nil {
		slog.Debug("grok: could not read the plan's usage window", "error", err)
		return
	}
	if len(limits) > 0 {
		to.Record(ProviderGrok, limits)
	}
}
