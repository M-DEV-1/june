package ipc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"

	"google.golang.org/genai"
)

// What the user saw on 2026-09-04: the free tier's day ran out mid-question and the window showed this, all of it, as the answer. It is one line in the store and about a thousand characters wide.
const quotaBlob = `ask text: generate (iteration 0): Error 429, Message: You exceeded your current quota, please check your plan and billing details. For more information on this error, head to: https://ai.google.dev/gemini-api/docs/rate-limits., Status: RESOURCE_EXHAUSTED, Details: [map[@type:type.googleapis.com/google.rpc.QuotaFailure violations:[map[quotaDimensions:map[location:global model:gemini-3.5-flash] quotaId:GenerateRequestsPerDayPerProjectPerModel-FreeTier quotaValue:250]]] map[@type:type.googleapis.com/google.rpc.Help links:[map[description:Learn more about Gemini API quotas url:https://ai.google.dev/gemini-api/docs/rate-limits]]] map[@type:type.googleapis.com/google.rpc.RetryInfo retryDelay:16s]]`

func TestAskErrorText(t *testing.T) {
	quota := genai.APIError{Code: 429, Status: "RESOURCE_EXHAUSTED", Message: "You exceeded your current quota, please check your plan and billing details."}
	cases := map[string]struct {
		err  error
		want string
	}{
		"the day's free requests are spent":      {quota, "Today's free Gemini requests are used up. It works again tomorrow, or now on another brain."},
		"the same error wrapped by the agent":    {fmt.Errorf("ask text: generate (iteration 0): %w", quota), "Today's free Gemini requests are used up. It works again tomorrow, or now on another brain."},
		"the same error as the plain text of it": {errors.New(quotaBlob), "Today's free Gemini requests are used up. It works again tomorrow, or now on another brain."},
		"a 429 that is not the daily quota":      {genai.APIError{Code: 429, Status: "TOO_MANY_REQUESTS"}, "That is too many requests in a row. Give it a moment, then ask again."},
		"every model overloaded":                 {genai.APIError{Code: 503, Status: "UNAVAILABLE", Message: "The model is overloaded. Please try again later."}, "Every model is busy right now. Ask again in a moment."},
		"overloaded, reported by pointer":        {&genai.APIError{Code: 503}, "Every model is busy right now. Ask again in a moment."},
		"overloaded, reported by another brain":  {errors.New("codex: HTTP 503 service unavailable"), "Every model is busy right now. Ask again in a moment."},
		"the key is missing":                     {genai.APIError{Code: 401, Status: "UNAUTHENTICATED"}, "The key for this brain was refused. Check it in settings, then ask again."},
		"the key is not allowed here":            {genai.APIError{Code: 403, Status: "PERMISSION_DENIED"}, "The key for this brain was refused. Check it in settings, then ask again."},
		"the request was malformed":              {genai.APIError{Code: 400, Status: "INVALID_ARGUMENT", Message: "Unable to submit request because it has a mimeType parameter with value image/heic"}, "The model turned the question down as malformed. Try asking it another way."},
		"the ask timed out":                      {fmt.Errorf("ask text: %w", context.DeadlineExceeded), "That took too long and was given up on. Ask again, or ask for less at once."},
		"the ask was cancelled":                  {fmt.Errorf("ask text: %w", context.Canceled), "That question was stopped before it was answered."},
		"there is no network":                    {&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: network is unreachable")}, "No network. The model cannot be reached from here."},
		"the name does not resolve":              {&net.DNSError{Err: "no such host", Name: "generativelanguage.googleapis.com"}, "No network. The model cannot be reached from here."},
		"anything else":                          {errors.New("tool loop: the store is locked"), "That ask failed. The whole message is in the log."},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			sentence, detail := AskErrorText(c.err)
			if sentence != c.want {
				t.Errorf("sentence = %q, want %q", sentence, c.want)
			}
			if detail != c.err.Error() {
				t.Errorf("detail = %q, want the error's own text %q", detail, c.err.Error())
			}
			if len(sentence) > 120 {
				t.Errorf("sentence is %d characters, want 120 or fewer: %q", len(sentence), sentence)
			}
			for _, banned := range []string{"http", "map[", "@type", "{", "\n"} {
				if strings.Contains(strings.ToLower(sentence), banned) {
					t.Errorf("sentence carries %q, which is the provider's own text leaking through: %q", banned, sentence)
				}
			}
		})
	}
}

// A nil error has nothing to say, and must not be turned into a sentence claiming something failed.
func TestAskErrorText_NilErrorSaysNothing(t *testing.T) {
	sentence, detail := AskErrorText(nil)
	if sentence != "" || detail != "" {
		t.Errorf("AskErrorText(nil) = %q, %q, want two empty strings", sentence, detail)
	}
}

// The line the ask handler needs on its error event is AskSentence(err): the window is handed the sentence while the store still keeps the provider's whole message under the failed turn.
func TestAskSentence_IsTheLineOnItsOwn(t *testing.T) {
	err := fmt.Errorf("ask text: %w", genai.APIError{Code: 503})

	if got := AskSentence(err); got != askOverloaded {
		t.Errorf("AskSentence = %q, want %q", got, askOverloaded)
	}
	if got := AskSentence(nil); got != "" {
		t.Errorf("AskSentence(nil) = %q, want an empty string", got)
	}
}

// The other way to wire it is err = AskError(err): from there on every reader — the error event, the stored turn, the act run — reads the sentence, while the provider's own message stays reachable.
func TestAskError_ReadsAsTheSentenceAndKeepsTheRest(t *testing.T) {
	original := fmt.Errorf("ask text: generate (iteration 0): %w", genai.APIError{Code: 429, Status: "RESOURCE_EXHAUSTED"})

	wrapped := AskError(original)

	if got := wrapped.Error(); got != "Today's free Gemini requests are used up. It works again tomorrow, or now on another brain." {
		t.Errorf("Error() = %q, want the short sentence", got)
	}
	if got := AskDetail(wrapped); got != original.Error() {
		t.Errorf("AskDetail = %q, want the provider's own message %q", got, original.Error())
	}
	var api genai.APIError
	if !errors.As(wrapped, &api) || api.Code != 429 {
		t.Errorf("errors.As lost the API error through the wrapper; the Codex hand-over reads that code")
	}
	if AskError(nil) != nil {
		t.Errorf("AskError(nil) = %v, want nil", AskError(nil))
	}
	if again := AskError(wrapped); again != wrapped {
		t.Errorf("AskError wrapped an already wrapped failure a second time")
	}
}

// A failure that reaches the window must be one line whatever the provider sent, since the window sizes itself to what it is given.
func TestAskErrorText_IsOneShortLineForATextWallOfAnError(t *testing.T) {
	sentence, detail := AskErrorText(errors.New(strings.Repeat(quotaBlob, 6)))

	if strings.Contains(sentence, "\n") || len(sentence) > 120 {
		t.Errorf("sentence = %q, want one line of 120 characters or fewer", sentence)
	}
	if len(detail) != len(quotaBlob)*6 {
		t.Errorf("detail is %d characters, want the whole %d kept", len(detail), len(quotaBlob)*6)
	}
}
