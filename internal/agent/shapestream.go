package agent

import "encoding/json"

// shapeStream turns the argument text of a draw call, arriving in fragments, into complete shape objects as soon as each one closes.
// It reads the raw bytes one at a time rather than through encoding/json's own decoder, because that decoder needs a whole document and this text is never whole until the call has finished streaming in; the state kept between calls to Push is exactly enough to know where a string is open, how deep the brace and bracket nesting runs, and whether that nesting currently sits inside the top-level "shapes" array.
type shapeStream struct {
	// stack holds one entry, '{' or '[', for every object or array container currently open, oldest first, so a '}' or ']' closes exactly the container it belongs to rather than being assumed.
	stack []byte
	// inString is true while the scanner sits inside a JSON string literal, so a brace or bracket byte inside it is kept as plain text instead of being read as structure.
	inString bool
	// escaped is true when the byte just read inside the current string was an unescaped backslash, so the next byte is read as the character it escapes rather than as a quote or backslash of its own.
	escaped bool
	// strBuf collects the bytes of the string literal currently open. It is only ever compared against anything when that string closes directly inside the top-level object, before the shapes array has been found — see the pendingKey field.
	strBuf []byte
	// pendingKey is true right after a top-level string closed reading exactly "shapes", until the next non-whitespace, non-colon byte says whether an array actually follows it.
	pendingKey bool
	// haveArray is true once the "shapes" key's value has been seen to open with '[', which happens at most once per stream — a later, unrelated string that also reads "shapes" is never looked at again.
	haveArray bool
	// arrayDepth is the depth of stack right after the shapes array's own '[' was pushed onto it, so a '{' pushed while stack is exactly that deep is a direct element of that array, never something nested inside one of its shapes.
	arrayDepth int
	// capturing is true while the scanner is inside one direct element of the shapes array, collecting its raw bytes so the whole of it can be unmarshaled once it closes.
	capturing bool
	// current collects the raw bytes of the shape object being captured, from its opening '{' through its closing '}' inclusive.
	current []byte
}

// Push takes the next fragment of the arguments and returns every shape that became complete because of it, in the order they appeared.
// Input: the fragment, which may split anywhere including inside a string or an escape.
// Output: the finished shape objects, nil when the fragment completed none.
func (s *shapeStream) Push(fragment string) []map[string]any {
	var out []map[string]any
	for i := 0; i < len(fragment); i++ {
		c := fragment[i]
		if s.capturing {
			s.current = append(s.current, c)
		}
		if s.inString {
			switch {
			case s.escaped:
				s.escaped = false
			case c == '\\':
				s.escaped = true
			case c == '"':
				s.inString = false
				// Only a string that closed directly inside the top-level object, before the shapes array is found, can be the "shapes" key itself.
				if !s.haveArray && len(s.stack) == 1 && string(s.strBuf) == "shapes" {
					s.pendingKey = true
				}
				s.strBuf = nil
			default:
				s.strBuf = append(s.strBuf, c)
			}
			continue
		}
		switch c {
		case '"':
			s.inString = true
			s.strBuf = nil
		case '{':
			s.pendingKey = false
			// A '{' opened exactly where the shapes array's own elements sit, and only when no element is already being captured, is the start of one shape.
			if s.haveArray && !s.capturing && len(s.stack) == s.arrayDepth {
				s.capturing = true
				s.current = []byte{'{'}
			}
			s.stack = append(s.stack, '{')
		case '[':
			s.stack = append(s.stack, '[')
			if s.pendingKey {
				s.haveArray = true
				s.arrayDepth = len(s.stack)
			}
			s.pendingKey = false
		case '}':
			if len(s.stack) > 0 {
				s.stack = s.stack[:len(s.stack)-1]
			}
			// The stack falling back to the array's own depth means this '}' closed the shape being captured, not something nested inside it.
			if s.capturing && len(s.stack) == s.arrayDepth {
				s.capturing = false
				var shape map[string]any
				if err := json.Unmarshal(s.current, &shape); err == nil {
					out = append(out, shape)
				}
				s.current = nil
			}
		case ']':
			if len(s.stack) > 0 {
				s.stack = s.stack[:len(s.stack)-1]
			}
			// The stack falling back to one below the shapes array's own depth means this ']' closed the shapes array itself, not some other array nested inside one of its shapes. arrayDepth is set to a depth no '{' can ever match again, so a later top-level array's elements are never mistaken for more shapes.
			if s.haveArray && len(s.stack) == s.arrayDepth-1 {
				s.arrayDepth = -1
			}
		default:
			// Whitespace and the colon between the key and its value leave a pending "shapes" key standing; anything else means the value that followed it was not the array, so it stops being a candidate.
			if s.pendingKey && c != ' ' && c != '\t' && c != '\n' && c != '\r' && c != ':' {
				s.pendingKey = false
			}
		}
	}
	return out
}
