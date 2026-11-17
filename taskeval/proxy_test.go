//go:build taskeval && linux

package taskeval

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
)

// scriptStep is one scripted model reply: either a tool call (Call and Args) or the final answer (Say).
type scriptStep struct {
	Call string         `json:"call"`
	Args map[string]any `json:"args"`
	Say  string         `json:"say"`
}

// toolResult is what one tool returned to the model, read back out of the request the daemon sent next.
type toolResult struct{ Name, Output string }

// scriptedModel is a fake Gemini API. It replies to each request with the next step of the loaded script and keeps the requests so tests can see what the daemon sent the model.
type scriptedModel struct {
	srv *httptest.Server

	mu       sync.Mutex
	steps    []scriptStep
	next     int
	requests int
	last     []toolResult
	problems []string
}

func newScriptedModel() *scriptedModel {
	m := &scriptedModel{}
	m.srv = httptest.NewServer(http.HandlerFunc(m.serve))
	return m
}

func (m *scriptedModel) close() { m.srv.Close() }

// url is the address to give the daemon as GOOGLE_GEMINI_BASE_URL.
func (m *scriptedModel) url() string { return m.srv.URL }

// load starts a new script and forgets everything recorded for the previous one. A request that arrives while no script is loaded is refused and counted in problems.
func (m *scriptedModel) load(steps []scriptStep) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.steps, m.next, m.requests, m.last, m.problems = steps, 0, 0, nil, nil
}

// finish returns what the tools returned to the model in the last request of the script, how many requests the model got, and anything that went wrong (a request past the end of the script, an unreadable body, a script step never used).
func (m *scriptedModel) finish() (tools []toolResult, requests int, problems []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	problems = m.problems
	if m.next < len(m.steps) {
		problems = append(problems, fmt.Sprintf("script has %d steps but the model was asked %d times", len(m.steps), m.next))
	}
	tools, requests = m.last, m.requests
	m.steps = nil
	return tools, requests, problems
}

func (m *scriptedModel) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.steps == nil {
		m.problems = append(m.problems, "model asked with no script loaded: "+r.URL.Path)
		http.Error(w, `{"error":{"code":503,"message":"no script loaded","status":"UNAVAILABLE"}}`, http.StatusServiceUnavailable)
		return
	}
	m.requests++
	tools, err := toolResultsIn(body)
	if err != nil {
		m.problems = append(m.problems, "unreadable request: "+err.Error())
	}
	m.last = tools

	if m.next >= len(m.steps) {
		m.problems = append(m.problems, fmt.Sprintf("model asked %d times, more than the script's %d steps", m.next+1, len(m.steps)))
		m.next = len(m.steps) - 1
	}
	step := m.steps[m.next]
	m.next++

	part := map[string]any{"text": step.Say}
	if step.Call != "" {
		part = map[string]any{"functionCall": map[string]any{"name": step.Call, "args": step.Args}}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"role": "model", "parts": []any{part}}}}})
}

// toolResultsIn reads a Gemini generateContent request body and returns every function response in it, which are the tool results the daemon is handing the model.
func toolResultsIn(body []byte) ([]toolResult, error) {
	var req struct {
		Contents []struct {
			Parts []struct {
				FunctionResponse *struct {
					Name     string         `json:"name"`
					Response map[string]any `json:"response"`
				} `json:"functionResponse"`
			} `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	var out []toolResult
	for _, c := range req.Contents {
		for _, p := range c.Parts {
			if fr := p.FunctionResponse; fr != nil {
				text, _ := fr.Response["output"].(string)
				out = append(out, toolResult{Name: fr.Name, Output: text})
			}
		}
	}
	return out, nil
}

// readScript loads scripts/<task>.json and replaces each {{name}} in it with the matching value from vars, escaped so it is safe inside a JSON string.
func readScript(task string, vars map[string]string) ([]scriptStep, error) {
	raw, err := os.ReadFile("scripts/" + task + ".json")
	if err != nil {
		return nil, err
	}
	pairs := make([]string, 0, 2*len(vars))
	for k, v := range vars {
		quoted, _ := json.Marshal(v)
		pairs = append(pairs, "{{"+k+"}}", strings.Trim(string(quoted), `"`))
	}
	var steps []scriptStep
	if err := json.Unmarshal([]byte(strings.NewReplacer(pairs...).Replace(string(raw))), &steps); err != nil {
		return nil, fmt.Errorf("scripts/%s.json: %w", task, err)
	}
	return steps, nil
}
