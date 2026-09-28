package ipc

// A failed model call does not arrive as a sentence. Gemini returns "Error 429, Message: You exceeded your current quota... Status: RESOURCE_EXHAUSTED, Details: [map[@type:type.googleapis.com/google.rpc.QuotaFailure ...]]" — a thousand characters of Go-formatted JSON, a URL and a stack of map literals. Handed to the window as the answer, it filled the panel and pushed the rest of the conversation off screen. Everything here turns one of those into the one line a person would say out loud, and keeps the provider's own message beside it for the log.

import (
	"context"
	"errors"
	"net"
	"regexp"

	"june/internal/util"

	"google.golang.org/genai"
)

// The sentences a failed ask can read as. Each says what happened and, where there is something to do about it, what to do; none carries a URL, a status code or any of the provider's own JSON.
const (
	askQuotaSpent  = "Today's free Gemini requests are used up. It works again tomorrow, or now on another brain."
	askTooFast     = "That is too many requests in a row. Give it a moment, then ask again."
	askOverloaded  = "Every model is busy right now. Ask again in a moment."
	askKeyRefused  = "The key for this brain was refused. Check it in settings, then ask again."
	askMalformed   = "The model turned the question down as malformed. Try asking it another way."
	askTooLong     = "That took too long and was given up on. Ask again, or ask for less at once."
	askStopped     = "That question was stopped before it was answered."
	askNoNetwork   = "No network. The model cannot be reached from here."
	askUnexplained = "That ask failed. The whole message is in the log."
)

// statusInText finds the HTTP status a provider named in a plain error string, for the paths that return no typed API error: another brain's HTTP client, and any error whose typed cause was flattened to text on the way here. It only reads a number that follows a word saying it is one, so a model name or a byte count is never mistaken for a status.
var statusInText = regexp.MustCompile(`(?i)\b(?:error|status|code|http)\b\W{0,3}(\d{3})\b`)

// AskErrorText turns a failed model call into what the window shows and what the log keeps. Input: the error a model call returned, wrapped or not. Output: sentence, one line a person would say out loud — never longer than 120 characters, never a URL, a status code or a stack of map literals — and detail, the error's whole text unchanged. A nil error gives two empty strings.
func AskErrorText(err error) (sentence, detail string) {
	if err == nil {
		return "", ""
	}
	detail = err.Error()
	return askSentence(err, detail), detail
}

// AskSentence is the sentence on its own, for a caller that shows the line and logs the error itself — the /ask error event, which is what the window draws. Input: the error a model call returned. Output: the sentence, and "" for a nil error.
func AskSentence(err error) string {
	sentence, _ := AskErrorText(err)
	return sentence
}

// askSentence picks the sentence for one failed call. Input: the error and its text. Output: the sentence. The context cases are read first, because a cancelled or timed-out call can carry a provider's half-written message that would otherwise be read as the reason.
func askSentence(err error, text string) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return askTooLong
	case errors.Is(err, context.Canceled):
		return askStopped
	}
	if code := askStatus(err, text); code != 0 {
		switch code {
		case 429:
			// RESOURCE_EXHAUSTED is the free tier's day being spent; a bare 429 is the per-minute rate, which comes back on its own.
			if util.ContainsAny(text, "resource_exhausted", "quota") {
				return askQuotaSpent
			}
			return askTooFast
		case 503, 502, 500:
			return askOverloaded
		case 401, 403:
			return askKeyRefused
		case 400, 404, 422:
			return askMalformed
		}
	}
	if noNetwork(err, text) {
		return askNoNetwork
	}
	if util.ContainsAny(text, "context deadline exceeded", "deadline exceeded", "timed out", "timeout") {
		return askTooLong
	}
	if util.ContainsAny(text, "context canceled", "context cancelled") {
		return askStopped
	}
	return askUnexplained
}

// askStatus reads the HTTP status a failed call came back with. Input: the error and its text. Output: the status code, or 0 when the failure named none. The SDK returns its API error by value and callers wrap it, so both forms are asked for before the text is read.
func askStatus(err error, text string) int {
	var byValue genai.APIError
	if errors.As(err, &byValue) && byValue.Code != 0 {
		return byValue.Code
	}
	var byPointer *genai.APIError
	if errors.As(err, &byPointer) && byPointer != nil && byPointer.Code != 0 {
		return byPointer.Code
	}
	if m := statusInText.FindStringSubmatch(text); m != nil {
		code := 0
		for _, digit := range m[1] {
			code = code*10 + int(digit-'0')
		}
		return code
	}
	return 0
}

// noNetwork reports whether the call never reached the provider at all. Input: the error and its text. Output: true for a dial, DNS or connection failure, whether it arrived as one of net's own types or only as text.
func noNetwork(err error, text string) bool {
	var dns *net.DNSError
	var op *net.OpError
	if errors.As(err, &dns) || errors.As(err, &op) {
		return true
	}
	return util.ContainsAny(text, "no such host", "connection refused", "network is unreachable", "no route to host", "dial tcp", "server misbehaving")
}
