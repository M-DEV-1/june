package db

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// queryStoreTimeout bounds how long one QueryStore call may run, so a bad join or a full scan over a large store cannot wedge the daemon.
const queryStoreTimeout = 5 * time.Second

// queryStoreCharBudget caps how many characters QueryStore's rendered text may carry. A tool result is prompt text — an unbounded SELECT could otherwise dump the whole table into it.
const queryStoreCharBudget = 6000

// readOnlyDB lazily opens a second connection to the same sqlite file, kept separate from the read-write pool s.db uses for everything else. QueryStore runs every statement through this handle instead of s.db, so a write that slips past the "read-only" contract fails at the sqlite layer rather than by string-matching the SQL text for DROP/DELETE. mode=ro asks sqlite's own URI parser to open the file for reading only; _pragma=query_only(1) is a second, independent guard (covers PRAGMA-based writes too). Both are verified in query_store_test.go against a real write attempt.
func (s *Store) readOnlyDB() (*sql.DB, error) {
	if s.path == "" || s.path == ":memory:" {
		return nil, fmt.Errorf("query_store needs a file-backed database")
	}
	s.roOnce.Do(func() {
		s.roDB, s.roErr = sql.Open("sqlite", "file:"+s.path+"?mode=ro&_pragma=query_only(1)")
	})
	return s.roDB, s.roErr
}

// singleStatement trims a trailing semicolon and rejects anything with a second statement after it.
// This is a plain substring scan, not a SQL parser — a semicolon inside a quoted string literal would also trip it. That's an acceptable false positive for a tool whose job is answering questions, not writing quoted semicolons; a real tokenizer is the upgrade if that ever bites.
func singleStatement(query string) (string, error) {
	trimmed := strings.TrimSpace(strings.TrimRight(strings.TrimSpace(query), ";"))
	if trimmed == "" {
		return "", fmt.Errorf("empty query")
	}
	if strings.Contains(trimmed, ";") {
		return "", fmt.Errorf("only one SQL statement is allowed per call")
	}
	return trimmed, nil
}

// QueryStore runs one read-only SQL statement against the store and renders the result as text: a header line of column names, then one line per row with values joined by a tab.
// Input: a single SQL statement — SELECT, PRAGMA table_info(...), EXPLAIN, anything that doesn't write — and rowCap, the maximum rows to render (callers pass the same row cap every other tool result obeys).
// Output: the rendered rows, the literal text "no rows matched" if the query ran but returned nothing, or an error if the statement was rejected (more than one statement, a write, a bad deadline) or failed to run. When the row cap or character budget cuts the result short, the last line says so.
func (s *Store) QueryStore(ctx context.Context, query string, rowCap int) (string, error) {
	stmt, err := singleStatement(query)
	if err != nil {
		return "", err
	}
	roDB, err := s.readOnlyDB()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, queryStoreTimeout)
	defer cancel()

	rows, err := roDB.QueryContext(ctx, stmt)
	if err != nil {
		if strings.Contains(err.Error(), "no such column") {
			return "", fmt.Errorf("%w; %s", err, columnsOf(ctx, roDB, stmt))
		}
		return "", err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return "", err
	}
	values := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range values {
		ptrs[i] = &values[i]
	}

	var b strings.Builder
	b.WriteString(strings.Join(cols, "\t"))
	rowCount := 0
	truncated := false
	for rows.Next() {
		if rowCount >= rowCap {
			truncated = true
			break
		}
		if err := rows.Scan(ptrs...); err != nil {
			return "", err
		}
		line := renderQueryStoreRow(values)
		if b.Len()+1+len(line) > queryStoreCharBudget {
			truncated = true
			break
		}
		b.WriteByte('\n')
		b.WriteString(line)
		rowCount++
	}
	if err := rows.Err(); err != nil {
		return "", err
	}

	if rowCount == 0 {
		// A row too large to render is not an absent row, and the two must never read the same. Saying "no rows matched" here would have the caller conclude the data does not exist when it does, which is the one wrong answer this tool must never give.
		if truncated {
			return "", fmt.Errorf("the first row alone exceeds the %d character budget: select fewer columns, or wrap a long one in substr(col,1,500)", queryStoreCharBudget)
		}
		return "no rows matched", nil
	}
	if truncated {
		fmt.Fprintf(&b, "\n... (truncated at %d rows — add a LIMIT or aggregate and try again)", rowCount)
	}
	return b.String(), nil
}

// renderQueryStoreRow joins one scanned row's values with a tab. A pipe would be ambiguous: a window title like "Chat | Vexil Quorin | Microsoft Teams" carries its own pipes, and a reader could not tell those from column boundaries. NULL becomes the literal "NULL"; []byte (sqlite's BLOB/untyped scan type) is rendered as a string since Ora's own tables never store binary in a column worth querying this way.
func renderQueryStoreRow(values []any) string {
	parts := make([]string, len(values))
	for i, v := range values {
		switch val := v.(type) {
		case nil:
			parts[i] = "NULL"
		case []byte:
			parts[i] = string(val)
		default:
			parts[i] = fmt.Sprintf("%v", val)
		}
	}
	return strings.Join(parts, "\t")
}

// tableNames finds the tables a statement reads: every name after FROM or JOIN.
var tableNames = regexp.MustCompile(`(?i)\b(?:from|join)\s+([a-z_][a-z0-9_]*)`)

// columnsOf lists the real columns of every table a statement reads, for the error a guessed column gets, so the next try can use a name that exists. Input: the read-only handle and the statement. Output: one "table has: a, b, c" clause per table, joined by "; ", or "" when none could be read.
func columnsOf(ctx context.Context, db *sql.DB, stmt string) string {
	var out []string
	seen := map[string]bool{}
	for _, m := range tableNames.FindAllStringSubmatch(stmt, -1) {
		table := strings.ToLower(m[1])
		if seen[table] {
			continue
		}
		seen[table] = true
		rows, err := db.QueryContext(ctx, "SELECT name FROM pragma_table_info(?)", table)
		if err != nil {
			continue
		}
		var cols []string
		for rows.Next() {
			var name string
			if rows.Scan(&name) == nil {
				cols = append(cols, name)
			}
		}
		rows.Close()
		if len(cols) > 0 {
			out = append(out, table+" has: "+strings.Join(cols, ", "))
		}
	}
	return strings.Join(out, "; ")
}
