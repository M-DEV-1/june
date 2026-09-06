package agent

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"
)

// codexCall is one function call the model made in a round.
type codexCall struct {
	ID        string
	CallID    string
	Name      string
	Arguments string
}

// codexUsage is the token count the backend reports for one round. InputTokens counts the whole input whether the backend read it afresh or answered it out of its prompt cache, so Details.CachedTokens is a part of InputTokens and never extra to it — measured against the real endpoint on 2026-09-05, a second call behind the same 4,605-token instruction reported input_tokens 4,614 with cached_tokens 3,840.
type codexUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
	Details      struct {
		CachedTokens     int `json:"cached_tokens"`
		CacheWriteTokens int `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`
}

// codexRound is what one streamed response yields: every finished output item as sent so it can be echoed into the next round, the function calls among them, the answer text, and the model and usage from response.completed.
type codexRound struct {
	Items []json.RawMessage
	Calls []codexCall
	Text  string
	Model string
	Usage codexUsage
}

// codexHTTPError is a non-2xx answer from the backend, with the status and the start of the body.
type codexHTTPError struct {
	Code int
	Body string
	// RetryAfter is the delay the backend named in its Retry-After header, and zero when it named none.
	RetryAfter time.Duration
}

func (e codexHTTPError) Error() string {
	return fmt.Sprintf("codex: HTTP %d: %s", e.Code, e.Body)
}

// codexErrorBody is the error object the backend nests in failed responses and error events.
type codexErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// text returns the error's message, or fallback when the object is missing or empty.
func (e *codexErrorBody) text(fallback string) string {
	if e == nil || e.Message == "" {
		return fallback
	}
	if e.Code != "" {
		return e.Code + ": " + e.Message
	}
	return e.Message
}

// parseCodexStream reads the text/event-stream body of one Responses call. Input: the stream, and a callback given each shape of a draw call as soon as that shape's object closes in the arguments still arriving, so the ink starts while the model is still writing the rest of the call — nil to wait for the whole call, which is what every path but an ask does. Output: the round, or an error carrying the backend's message when the response failed, an error event arrived, or the stream ended before response.completed.
func parseCodexStream(r io.Reader, onShape func(itemID string, shape map[string]any)) (codexRound, error) {
	var round codexRound
	var deltas, message strings.Builder
	sawMessage, completed := false, false
	// firstCallDone is true once any function_call in this response has finished. The calls of one response only execute after the whole stream ends, so a draw call after the first one would otherwise resolve its shapes against the screen as it was before that first call ran rather than as it will be once it has — onShape stops early instead.
	firstCallDone := false
	// drawing holds a scanner for each draw call whose arguments are still arriving, keyed by the stream item they belong to.
	drawing := map[string]*shapeStream{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	// One event's payload may arrive as several data lines, which the stream format says to join with newlines, so the lines are gathered until the blank line that ends the event.
	var payload []string
	var fatal error
	handle := func(data string) bool {
		if data == "" || data == "[DONE]" {
			return true
		}
		var ev struct {
			Type     string          `json:"type"`
			Delta    string          `json:"delta"`
			ItemID   string          `json:"item_id"`
			Item     json.RawMessage `json:"item"`
			Code     string          `json:"code"`
			Message  string          `json:"message"`
			Error    *codexErrorBody `json:"error"`
			Response struct {
				Model             string          `json:"model"`
				Usage             codexUsage      `json:"usage"`
				Error             *codexErrorBody `json:"error"`
				IncompleteDetails struct {
					Reason string `json:"reason"`
				} `json:"incomplete_details"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			slog.Debug("codex: a stream event did not parse", "error", err)
			return true
		}
		switch ev.Type {
		case "response.output_text.delta":
			deltas.WriteString(ev.Delta)
		case "response.output_item.added":
			// A draw call is the one call worth acting on before it has finished arriving, so only its arguments are scanned; the name is only on this event, never on the deltas that follow. Nothing is scanned once the response's first call has finished (firstCallDone) or without an item id to key the scanner and the shapes it collects by (finding 5 of the 2026-09 draw-batch review: an empty id shared by two calls would share their buckets too).
			if onShape == nil || firstCallDone {
				return true
			}
			var item struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			}
			if json.Unmarshal(ev.Item, &item) == nil && item.Type == "function_call" && item.Name == "draw" && item.ID != "" {
				drawing[item.ID] = &shapeStream{}
			}
		case "response.function_call_arguments.delta":
			if scan := drawing[ev.ItemID]; scan != nil {
				for _, shape := range scan.Push(ev.Delta) {
					onShape(ev.ItemID, shape)
				}
			}
		case "response.output_item.done":
			round.Items = append(round.Items, ev.Item)
			var item struct {
				Type      string `json:"type"`
				ID        string `json:"id"`
				CallID    string `json:"call_id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
				Content   []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			}
			if json.Unmarshal(ev.Item, &item) != nil {
				return true
			}
			switch item.Type {
			case "function_call":
				round.Calls = append(round.Calls, codexCall{ID: item.ID, CallID: item.CallID, Name: item.Name, Arguments: item.Arguments})
				firstCallDone = true
			case "message":
				for _, part := range item.Content {
					if part.Type == "output_text" {
						message.WriteString(part.Text)
						sawMessage = true
					}
				}
			}
		case "response.completed":
			completed = true
			round.Model = ev.Response.Model
			round.Usage = ev.Response.Usage
		case "response.incomplete":
			// A cut-off turn is a failure, not an answer, because handing back half a sentence as if it were finished is worse than saying nothing.
			reason := ev.Response.IncompleteDetails.Reason
			if reason == "" {
				reason = "unknown"
			}
			fatal = fmt.Errorf("codex: the response was cut short: %s", reason)
			return false
		case "response.failed":
			fatal = fmt.Errorf("codex: %s", ev.Response.Error.text("the response failed"))
			return false
		case "error":
			if ev.Message != "" {
				fatal = fmt.Errorf("codex: %s", ev.Message)
			} else {
				fatal = fmt.Errorf("codex: %s", ev.Error.text("the backend reported an error"))
			}
			return false
		}
		return true
	}
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" {
			if len(payload) > 0 {
				if !handle(strings.Join(payload, "\n")) {
					return round, fatal
				}
				payload = payload[:0]
			}
			continue
		}
		if after, ok := strings.CutPrefix(line, "data:"); ok {
			payload = append(payload, strings.TrimPrefix(after, " "))
		}
	}
	if len(payload) > 0 && !handle(strings.Join(payload, "\n")) {
		return round, fatal
	}
	if err := sc.Err(); err != nil {
		return round, fmt.Errorf("codex: reading the stream: %w", err)
	}
	if !completed {
		return round, errors.New("codex: the stream ended before response.completed")
	}
	if sawMessage {
		round.Text = message.String()
	} else {
		round.Text = deltas.String()
	}
	return round, nil
}
