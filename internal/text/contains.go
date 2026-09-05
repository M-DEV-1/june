package text

import "strings"

// ContainsAny reports whether s contains any of the given substrings, compared without case. Input: any string, and the substrings to look for, which must already be lower case — s is lowercased here, they are not. Output: true on the first substring found, false when none of them is there or none was given.
func ContainsAny(s string, subs ...string) bool {
	lower := strings.ToLower(s)
	for _, sub := range subs {
		if strings.Contains(lower, sub) {
			return true
		}
	}
	return false
}
