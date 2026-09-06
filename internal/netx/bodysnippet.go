package netx

import (
	"io"
	"strings"
)

// BodySnippet reads up to 512 bytes of r and trims the surrounding whitespace, for logging a small, safe piece of a failed HTTP response body rather than risking an unbounded read or an error page filling the log. Input: the body to read. Output: the trimmed text, or "" when the read failed or the body was empty.
func BodySnippet(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, 512))
	return strings.TrimSpace(string(b))
}
