package agent

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"june/internal/db"
)

// memoryAskSizes is what a plain memory question sent to the model, in bytes: the system instruction text, the first user turn's text, and every tool declaration as the JSON the request carries.
type memoryAskSizes struct {
	System, Turn, Tools int
	PerTool             map[string]int
}

func (s memoryAskSizes) total() int { return s.System + s.Turn + s.Tools }

// askOneMemoryQuestion runs one text ask, "Who owns Project X?", against a fake Gemini backend and measures the first request it sends. The store has a real schema, a standing-understanding doc and two recalled lines, so the request is the size a user with a filled store would send.
func askOneMemoryQuestion(t *testing.T) memoryAskSizes {
	t.Helper()
	var first []byte
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if first == nil {
			first = body
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"Priya Raman owns it."}]}}]}`)
	}))
	defer backend.Close()
	geminiBaseURL = backend.URL
	t.Cleanup(func() { geminiBaseURL = "" })

	store, err := db.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	schema, err := store.DescribeSchema(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	SetStoreSchema(schema)
	t.Cleanup(func() { SetStoreSchema("") })

	brain := &toolTestBrain{
		understanding: strings.Repeat("They work on a small team shipping a desktop product, mostly in the editor and the terminal, and check chat in short bursts. ", 14),
		retrieveRelevantResult: []string{
			"[note#12] Project X kickoff: Priya Raman owns delivery, Sam Ortiz owns the budget. " + strings.Repeat("Action items were agreed and written up in the shared doc. ", 12),
			"[episode] Reviewing the Project X roadmap, owner column filled in (Docs - Project X roadmap)",
		},
	}
	a := NewAgent(nil, nil, brain, nil, "test-key")
	if _, err := a.AskText(t.Context(), "Who owns Project X?"); err != nil {
		t.Fatal(err)
	}

	var req struct {
		SystemInstruction struct {
			Parts []struct{ Text string } `json:"parts"`
		} `json:"systemInstruction"`
		Contents []struct {
			Parts []struct{ Text string } `json:"parts"`
		} `json:"contents"`
		Tools []struct {
			FunctionDeclarations []json.RawMessage `json:"functionDeclarations"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(first, &req); err != nil {
		t.Fatal(err)
	}
	sizes := memoryAskSizes{PerTool: map[string]int{}}
	for _, p := range req.SystemInstruction.Parts {
		sizes.System += len(p.Text)
	}
	for _, c := range req.Contents {
		for _, p := range c.Parts {
			sizes.Turn += len(p.Text)
		}
	}
	for _, tool := range req.Tools {
		for _, raw := range tool.FunctionDeclarations {
			var named struct{ Name string }
			json.Unmarshal(raw, &named)
			sizes.PerTool[named.Name] = len(raw)
			sizes.Tools += len(raw)
		}
	}
	return sizes
}

// maxMemoryAskBytes caps the request a plain memory question sends. Measured 27,347 bytes (about 6,800 tokens at four bytes to a token) with the store, understanding doc and recalled lines askOneMemoryQuestion builds, and set 10% above that, so a new tool, a longer tool description or a longer prompt block fails here before it slows every text ask.
const maxMemoryAskBytes = 30100

// TestMemoryAskStaysUnderItsSizeBudget fails when a plain memory question sends more than maxMemoryAskBytes, and logs the size part by part so go test -v shows where a change landed.
func TestMemoryAskStaysUnderItsSizeBudget(t *testing.T) {
	got := askOneMemoryQuestion(t)
	if got.total() == 0 {
		t.Fatal("the ask sent nothing")
	}
	if got.total() > maxMemoryAskBytes {
		t.Errorf("a plain memory question sends %d bytes, over the budget of %d", got.total(), maxMemoryAskBytes)
	}
	t.Logf("system %d bytes (~%d tokens), tools %d (~%d), turn %d (~%d), total %d (~%d)", got.System, got.System/4, got.Tools, got.Tools/4, got.Turn, got.Turn/4, got.total(), got.total()/4)
	names := make([]string, 0, len(got.PerTool))
	for name := range got.PerTool {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return got.PerTool[names[i]] > got.PerTool[names[j]] })
	for _, name := range names {
		t.Logf("tool %-16s %5d bytes", name, got.PerTool[name])
	}
}
