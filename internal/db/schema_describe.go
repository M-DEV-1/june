package db

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

const (
	// maxVocabularyValues is how many distinct values a column may hold and still be described by listing them. Above this it is data rather than a vocabulary, and naming every value would put the store's contents into the prompt.
	maxVocabularyValues = 12
	// maxVocabularyValueLen is how long one of those values may be. A vocabulary is made of short labels — "action", "meeting", "work" — so anything longer is prose that happens to repeat.
	maxVocabularyValueLen = 40
)

// ftsShadowSuffixes are the tables sqlite creates to store a full-text index. They are storage internals: their columns are segment blobs and row sizes, and no question anyone asks is answered by reading them.
var ftsShadowSuffixes = []string{"_data", "_idx", "_content", "_docsize", "_config"}

// DescribeSchema returns the store's tables, their columns, and — for any column that only ever holds a handful of short values — the values themselves.
//
// Input: a context bounding the queries. Output: one line per table listing its columns, followed by an indented line for each column with a small vocabulary.
//
// It is read from the store rather than written by hand because a written one goes stale without saying so. The hand-written version of this claimed notes.kind included "action_item"; the real value is "action", so a query counting open action items returned zero against twenty-five real ones and looked entirely correct doing it.
// Listing the values is the half a schema cannot give. "kind TEXT" is true and useless — what a caller needs is that it is one of action, fact, meeting or system-log, and that is a fact about the data, computed here rather than remembered.
func (s *Store) DescribeSchema(ctx context.Context) (string, error) {
	tables, err := s.describableTables(ctx)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	for _, t := range tables {
		cols, err := s.columnsOf(ctx, t)
		if err != nil {
			continue
		}
		if len(cols) == 0 {
			continue
		}
		names := make([]string, 0, len(cols))
		for _, c := range cols {
			names = append(names, c.name+" "+c.typ)
		}
		fmt.Fprintf(&b, "%s: %s\n", t, strings.Join(names, ", "))
		for _, c := range cols {
			if vals := s.vocabularyOf(ctx, t, c); len(vals) > 0 {
				fmt.Fprintf(&b, "  %s is one of: %s\n", c.name, strings.Join(vals, ", "))
			}
		}
	}
	return b.String(), nil
}

// describableTables lists the tables worth describing, in a stable order and without the shadow tables a full-text index brings with it.
func (s *Store) describableTables(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		if isFTSShadow(name) {
			continue
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// isFTSShadow reports whether a table is one of the five sqlite creates behind a full-text index.
func isFTSShadow(name string) bool {
	for _, suffix := range ftsShadowSuffixes {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

// column is one column's name and its declared type.
type column struct{ name, typ string }

// columnsOf returns a table's columns in the order they were declared.
func (s *Store) columnsOf(ctx context.Context, table string) ([]column, error) {
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", quoteIdent(table)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []column
	for rows.Next() {
		var (
			cid, notnull, pk int
			name, typ        string
			dflt             any
		)
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		if typ == "" {
			typ = "TEXT"
		}
		out = append(out, column{name: name, typ: typ})
	}
	return out, rows.Err()
}

// vocabularyOf returns the values a column holds when there are few enough of them to be a vocabulary, and nothing when there are not.
//
// The probe stops as soon as one value too many is found, so a column holding a different long string in every row costs a handful of rows to reject rather than a scan of the table.
func (s *Store) vocabularyOf(ctx context.Context, table string, c column) []string {
	if !strings.Contains(strings.ToUpper(c.typ), "CHAR") && !strings.EqualFold(c.typ, "TEXT") {
		return nil
	}
	q := fmt.Sprintf("SELECT DISTINCT %s FROM %s WHERE %s IS NOT NULL AND %s != '' LIMIT %d",
		quoteIdent(c.name), quoteIdent(table), quoteIdent(c.name), quoteIdent(c.name), maxVocabularyValues+1)
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var vals []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil
		}
		if len(v) > maxVocabularyValueLen || strings.ContainsAny(v, "\n\r") {
			return nil
		}
		vals = append(vals, v)
	}
	if rows.Err() != nil || len(vals) > maxVocabularyValues || len(vals) < 2 || allDates(vals) {
		return nil
	}
	sort.Strings(vals)
	return vals
}

// allDates reports whether every value is a calendar date.
// A date column is not a vocabulary even when it currently holds few enough values to look like one: the set grows by one every day, so it would be listed for a fortnight and then quietly stop being listed, and the description would change shape with the calendar rather than with the schema.
func allDates(vals []string) bool {
	for _, v := range vals {
		if !looksLikeDate(v) {
			return false
		}
	}
	return len(vals) > 0
}

// looksLikeDate reports whether a value starts with a YYYY-MM-DD calendar date, which covers both a bare day and a timestamp written as text.
func looksLikeDate(v string) bool {
	if len(v) < 10 {
		return false
	}
	for i, r := range v[:10] {
		switch i {
		case 4, 7:
			if r != '-' {
				return false
			}
		default:
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

// quoteIdent wraps a table or column name so a name that collides with sqlite's own keywords still parses. The names come from sqlite_master and are the store's own, but doubling any quote inside keeps the string safe to build by hand.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
