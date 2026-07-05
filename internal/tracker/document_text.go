package tracker

import "strings"

// a11yNode is an in-memory, platform-agnostic mirror of one AT-SPI (or UIA)
// accessible: its role, its own text (if any), and its children. Kept
// separate from the live D-Bus walk in capture_linux.go so the content-
// selection rule below can be unit-tested without a real accessibility bus.
type a11yNode struct {
	Role     string
	Text     string
	Children []a11yNode
}

// roleDocumentWeb is the AT-SPI canonical role name (org.a11y.atspi.Accessible
// GetRoleName / ATSPI_ROLE_DOCUMENT_WEB, enum value 95) that Chromium and
// Firefox assign to the root accessible of an actual web page. Everything
// else in a browser's tree — the tab strip, nav buttons, extension icons,
// Brave Shields, the omnibox — lives OUTSIDE this subtree, as siblings of it
// in the browser chrome frame.
const roleDocumentWeb = "document web"

// documentText picks the text worth keeping out of an accessible tree.
//
// Rule: if the tree contains one or more DOCUMENT_WEB nodes (i.e. it's a
// browser window showing an actual page), return ONLY the concatenated text
// of those subtree(s) — this drops all browser chrome automatically, by
// structure rather than by name, while still keeping in-page UI (which is
// page content, not chrome). If there is no DOCUMENT_WEB node anywhere (a
// native app — terminal, editor, native Teams, etc.), fall back to the full
// tree's text, matching prior behavior exactly so native-app capture does
// not regress.
//
// Fallback note: if role separation ever proves insufficient in the wild
// (some toolkit misreports roles, etc.), a regex-based chrome strip over the
// flat text would be the secondary fallback — but roles are the primary,
// principled approach since they need no hardcoded element-name lists.
func documentText(root a11yNode) string {
	if texts, found := collectRoleSubtrees(root, roleDocumentWeb); found {
		return strings.Join(texts, "\n")
	}
	return strings.Join(collectAllText(root), "\n")
}

// collectRoleSubtrees walks the tree looking for nodes whose Role matches
// targetRole. For each match it collects that node's own subtree text (via
// collectAllText) without descending further once matched (siblings
// elsewhere in the tree are still visited, so multiple matching subtrees —
// e.g. multiple frames — are all included). The bool return reports whether
// any matching node was found at all, independent of whether it had text —
// so an empty/loading web document still suppresses chrome text rather than
// falling back to it.
func collectRoleSubtrees(n a11yNode, targetRole string) ([]string, bool) {
	if n.Role == targetRole {
		return collectAllText(n), true
	}
	var texts []string
	var found bool
	for _, child := range n.Children {
		if sub, ok := collectRoleSubtrees(child, targetRole); ok {
			found = true
			texts = append(texts, sub...)
		}
	}
	return texts, found
}

// collectAllText returns every non-empty node Text in the subtree, in
// depth-first order.
func collectAllText(n a11yNode) []string {
	var texts []string
	if t := strings.TrimSpace(n.Text); t != "" {
		texts = append(texts, t)
	}
	for _, child := range n.Children {
		texts = append(texts, collectAllText(child)...)
	}
	return texts
}
