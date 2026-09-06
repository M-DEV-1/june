package main

// The judge is one Gemini call that returns JSON. Every track shares it: a track supplies the rubric as the system instruction and the material as the user turn, and gets back a decoded struct. The model is config.TextModel (the cheap flash-lite the memory compiler already uses), because judging is a high-volume background job and no track needs the voice model.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/genai"

	"ora/internal/config"

	"ora/internal/brain"
)

// verdict is one binary criterion scored by the judge. Pass is meaningless when Verdict is "na" — a criterion the turn gives no occasion to test (T6 on a greeting, say) is excluded from rates rather than counted as a pass.
type verdict struct {
	Verdict string `json:"verdict"` // "pass" | "fail" | "na"
	Why     string `json:"why"`
}

func (v verdict) passed() bool { return strings.EqualFold(v.Verdict, "pass") }
func (v verdict) na() bool     { return strings.EqualFold(v.Verdict, "na") }

// judgeInterval paces judging calls. The free tier allows 15 generate_content requests a minute on flash-lite, and firing a track's calls back to back spends that in twenty seconds and then 429s the rest of the run into blank rows. One call every four seconds stays under the limit with room to spare.
// ponytail: a fixed interval, not a token bucket. If this ever runs on a paid key, delete the pacing rather than tuning it.
const judgeInterval = 4 * time.Second

// evalQuota is the daily counter every Gemini call this runner makes is metered against: brain_quota.json under config.DataDir(), the same file the daemon meters its own background jobs and asks against. Without it a default run's forty-odd judge calls spent the day's free-tier allowance on the same key with the count never moving, so the daemon went on believing the requests it reserves for the user's own asks were still there. Tests point it at a temp directory.
var evalQuota = brain.NewQuotaState(config.DataDir())

// meteredGemini runs one Gemini call under that counter, in the background band, so an eval run can never eat the requests reserved for an interactive ask. Input: the context, the model the call is billed under, and the call itself. Output: the call's own error, or *brain.ErrDailyQuota when today's background allowance for that model is already spent, in which case call is never made.
func meteredGemini(ctx context.Context, model string, call func(context.Context) error) error {
	gated := brain.WithDailyQuota(evalQuota, model, false, brain.DefaultQuotaOptions(), func(c context.Context, _ string) (string, error) {
		return "", call(c)
	})
	_, err := gated(ctx, "")
	return err
}

// judge holds the one genai client every judged call reuses, and the clock that keeps the run under the request-per-minute limit.
type judge struct {
	client *genai.Client
	last   time.Time
}

func newJudge(ctx context.Context, apiKey string) (*judge, error) {
	c, err := genai.NewClient(ctx, &genai.ClientConfig{APIKey: apiKey, Backend: genai.BackendGeminiAPI})
	if err != nil {
		return nil, fmt.Errorf("judge client: %w", err)
	}
	return &judge{client: c}, nil
}

// ask runs one judging call and unmarshals the model's JSON into out. Temperature is pinned to zero so a rerun over the same material moves the scorecard only when the material moved. Input: the rubric as a system instruction, the material to score, and a pointer to decode into. Output: an error if the call or the decode failed.
func (j *judge) ask(ctx context.Context, instruction, material string, out any) error {
	var temp float32
	cfg := &genai.GenerateContentConfig{
		SystemInstruction: &genai.Content{Role: "system", Parts: []*genai.Part{{Text: instruction}}},
		ResponseMIMEType:  "application/json",
		Temperature:       &temp,
	}
	var lastErr error
	// Three tries with a growing wait: a 429 means the minute's quota is spent, and the only useful response is to wait out the window rather than blank the row.
	for attempt := 0; attempt < 3; attempt++ {
		wait := judgeInterval - time.Since(j.last)
		if attempt > 0 {
			wait = time.Duration(attempt) * 25 * time.Second
		}
		if wait > 0 {
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		j.last = time.Now()
		// Every attempt is its own request to Google, so every attempt takes its own slot; a refusal ends the loop, because waiting cannot give back a day's allowance.
		var resp *genai.GenerateContentResponse
		err := meteredGemini(ctx, config.TextModel, func(c context.Context) error {
			var callErr error
			resp, callErr = j.client.Models.GenerateContent(c, config.TextModel,
				[]*genai.Content{genai.NewContentFromText(material, genai.RoleUser)}, cfg)
			return callErr
		})
		if err != nil {
			lastErr = err
			var spent *brain.ErrDailyQuota
			if errors.As(err, &spent) {
				return err
			}
			continue
		}
		text := resp.Text()
		if strings.TrimSpace(text) == "" {
			lastErr = fmt.Errorf("judge returned no text")
			continue
		}
		if err := json.Unmarshal([]byte(brain.StripFence(text)), out); err != nil {
			lastErr = fmt.Errorf("decode judge JSON: %w (body: %.200s)", err, text)
			continue
		}
		return nil
	}
	return lastErr
}

// rate turns a set of scored criteria into a pass count over the criteria that actually applied. Input: the verdicts for one criterion across many items. Output: passes, applicable (non-"na") count.
func rate(vs []verdict) (passes, applicable int) {
	for _, v := range vs {
		if v.na() {
			continue
		}
		applicable++
		if v.passed() {
			passes++
		}
	}
	return passes, applicable
}

// pct formats a pass rate for the scorecard, saying "n/a" rather than an invented 0% when no item gave the criterion an occasion to apply.
func pct(passes, applicable int) string {
	if applicable == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%d/%d (%.0f%%)", passes, applicable, 100*float64(passes)/float64(applicable))
}
