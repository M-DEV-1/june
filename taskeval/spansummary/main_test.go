package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"june/internal/obs"
)

// spanLines renders spans as the lines a spans.jsonl holds, using the type that writes them.
func spanLines(t *testing.T, spans ...obs.SpanRecord) *strings.Reader {
	t.Helper()
	var b strings.Builder
	for _, s := range spans {
		line, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "%s\n", line)
	}
	return strings.NewReader(b.String())
}

// Ten spans of 10, 20, ... 100 ms give a p50 of 50, a p95 of 100 and a total of 550, and a name with more total time sorts above one with less.
func TestSummarize_PercentilesAndOrder(t *testing.T) {
	var spans []obs.SpanRecord
	for i := 1; i <= 10; i++ {
		spans = append(spans, obs.SpanRecord{Name: "Ask.Text", DurationMS: float64(i * 10)})
	}
	spans = append(spans, obs.SpanRecord{Name: "DB.Quick", DurationMS: 1})

	rows, err := summarize(spanLines(t, spans...), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].name != "Ask.Text" || rows[1].name != "DB.Quick" {
		t.Fatalf("rows = %+v, want Ask.Text then DB.Quick", rows)
	}
	got := rows[0]
	if got.count != 10 || got.total != 550 || got.p50 != 50 || got.p95 != 100 || got.max != 100 {
		t.Errorf("Ask.Text row = %+v, want count 10, total 550, p50 50, p95 100, max 100", got)
	}
}

// With -by set, spans that carry the attribute split into one row per value and spans without it keep their plain name.
func TestSummarize_SplitsByAttribute(t *testing.T) {
	rows, err := summarize(spanLines(t,
		obs.SpanRecord{Name: "Ask.Tool", DurationMS: 5, Attrs: map[string]any{"tool.name": "click"}},
		obs.SpanRecord{Name: "Ask.Tool", DurationMS: 7, Attrs: map[string]any{"tool.name": "look"}},
		obs.SpanRecord{Name: "Ask.Tool", DurationMS: 9},
	), "tool.name")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, r := range rows {
		names[r.name] = true
	}
	for _, want := range []string{"Ask.Tool[click]", "Ask.Tool[look]", "Ask.Tool"} {
		if !names[want] {
			t.Errorf("rows have no %q: %+v", want, rows)
		}
	}
}
