// spansummary reads one or more spans.jsonl files and prints, for each span name, how many spans there were and their p50, p95 and max duration, sorted by total time. Run: go run ./taskeval/spansummary [-by attribute] spans.jsonl...
package main

import (
	"bufio"
	"cmp"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"slices"
	"text/tabwriter"

	"june/internal/obs"
)

// row is the summary of every span that shares one name.
type row struct {
	name                 string
	count                int
	total, p50, p95, max float64
}

func main() {
	by := flag.String("by", "", "also split each span name by this attribute, such as tool.name")
	flag.Parse()
	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: spansummary [-by attribute] spans.jsonl...")
		os.Exit(2)
	}
	var readers []io.Reader
	for _, path := range flag.Args() {
		f, err := os.Open(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "spansummary:", err)
			os.Exit(1)
		}
		defer f.Close()
		readers = append(readers, f)
	}
	rows, err := summarize(io.MultiReader(readers...), *by)
	if err != nil {
		fmt.Fprintln(os.Stderr, "spansummary:", err)
		os.Exit(1)
	}
	printRows(os.Stdout, rows)
}

// summarize reads span lines from r and returns one row per span name, longest total time first. Input: the lines of a spans.jsonl, and an attribute name ("" for none) that, when a span carries it, is appended to the span's name as name[value]. Output: the rows, or the error of the first line that is not a span record.
func summarize(r io.Reader, by string) ([]row, error) {
	durations := map[string][]float64{}
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var rec obs.SpanRecord
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			return nil, fmt.Errorf("not a span record: %w", err)
		}
		name := rec.Name
		if v, ok := rec.Attrs[by]; ok && by != "" {
			name = fmt.Sprintf("%s[%v]", name, v)
		}
		durations[name] = append(durations[name], rec.DurationMS)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	rows := make([]row, 0, len(durations))
	for name, ds := range durations {
		slices.Sort(ds)
		total := 0.0
		for _, d := range ds {
			total += d
		}
		rows = append(rows, row{name, len(ds), total, percentile(ds, 0.50), percentile(ds, 0.95), ds[len(ds)-1]})
	}
	slices.SortFunc(rows, func(a, b row) int { return cmp.Or(cmp.Compare(b.total, a.total), cmp.Compare(a.name, b.name)) })
	return rows, nil
}

// percentile is the nearest-rank percentile of an ascending slice: the smallest value that at least p of the values are at or below.
func percentile(sorted []float64, p float64) float64 {
	return sorted[int(math.Ceil(p*float64(len(sorted))))-1]
}

// printRows writes the rows as an aligned table, durations in milliseconds.
func printRows(w io.Writer, rows []row) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "span\tcount\ttotal_ms\tp50_ms\tp95_ms\tmax_ms\t")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%d\t%.1f\t%.1f\t%.1f\t%.1f\t\n", r.name, r.count, r.total, r.p50, r.p95, r.max)
	}
	tw.Flush()
}
