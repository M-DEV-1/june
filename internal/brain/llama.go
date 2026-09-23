package brain

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"ora/internal/util"
)

// LlamaServer answers by posting to a local llama-server's OpenAI-compatible /v1/chat/completions endpoint. It exists for the dream package's shadow brain: a second, local model that shadows the primary dream brain on the same prompts for offline comparison and never itself decides anything.
// chat_template_kwargs.enable_thinking rides on every request — unlike the other Brain constructors in this package, the shadow's whole point is to capture the model's reasoning trace for a later distillation pass, not just its final answer.
// Input: the server's base URL (e.g. "http://127.0.0.1:6944") and a hard timeout in seconds, which bounds the whole call including a slow local decode. Output: the reply's message content.
func LlamaServer(baseURL string, timeoutSeconds int) Brain {
	baseURL = strings.TrimRight(baseURL, "/")
	client := &http.Client{}
	return func(ctx context.Context, prompt string) (string, error) {
		ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSeconds)*time.Second)
		defer cancel()

		body, err := json.Marshal(map[string]any{
			"model":                "local",
			"messages":             []map[string]string{{"role": "user", "content": prompt}},
			"temperature":          0.7,
			"chat_template_kwargs": map[string]bool{"enable_thinking": true},
		})
		if err != nil {
			return "", fmt.Errorf("llama-server: marshal request: %w", err)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/chat/completions", bytes.NewReader(body))
		if err != nil {
			return "", fmt.Errorf("llama-server: build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			return "", fmt.Errorf("llama-server: post to %s: %w", baseURL, err)
		}
		defer resp.Body.Close()

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return "", fmt.Errorf("llama-server: server returned %d: %s", resp.StatusCode, util.LogHead(util.BodySnippet(resp.Body)))
		}

		var parsed struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
			return "", fmt.Errorf("llama-server: decode response: %w", err)
		}
		if len(parsed.Choices) == 0 {
			return "", fmt.Errorf("llama-server: response contained no choices")
		}
		text := strings.TrimSpace(parsed.Choices[0].Message.Content)
		if text == "" {
			return "", fmt.Errorf("llama-server: response contained no text")
		}
		return text, nil
	}
}
