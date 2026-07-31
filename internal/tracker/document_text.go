package tracker

import "strings"

// a11yNode is an in-memory, platform-agnostic mirror of one AT-SPI (or UIA) accessible: its role, its own text (if any), and its children.
// Kept separate from the live D-Bus walk in capture_linux.go so the rules below can be unit-tested without a real accessibility bus.
type a11yNode struct {
	Role     string
	Text     string
	Children []a11yNode
}

// roleDocumentWeb is the AT-SPI role name (ATSPI_ROLE_DOCUMENT_WEB) Chromium/Firefox give the root accessible of an actual web page. Browser chrome — tab strip, nav buttons, omnibox — lives outside this subtree, as siblings of it.
const roleDocumentWeb = "document web"

// documentText picks the text worth keeping out of an accessible tree.
// If the tree has one or more DOCUMENT_WEB nodes (a browser showing a real page), return only their concatenated text — drops browser chrome by structure, not by name. Otherwise (native apps have no DOCUMENT_WEB node) fall back to the full tree's text.
func documentText(root a11yNode) string {
	if texts, found := collectRoleSubtrees(root, roleDocumentWeb); found {
		return strings.Join(texts, "\n")
	}
	return strings.Join(collectAllText(root), "\n")
}

// collectRoleSubtrees walks the tree for nodes whose Role matches targetRole, collecting each match's subtree text without descending further once matched (other matching subtrees elsewhere are still visited).
// The bool reports whether any match was found at all, regardless of whether it had text — so an empty/loading web document still suppresses chrome rather than falling back to it.
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

// collectAllText returns every non-empty node Text in the subtree, in depth-first order.
func collectAllText(n a11yNode) []string {
	return collectTextDedupingAncestors(n, nil)
}

// collectTextDedupingAncestors walks the subtree collecting each node's own Text, skipping a node whose Text exactly matches one already contributed by an ancestor on the current path.
// AT-SPI's Text interface on a container (a link, a paragraph) often returns its descendants' full aggregated text, and the descendant leaf reports that same text again — without this check every link/label gets counted twice ("LEARN MORE LEARN MORE"). Dedup is scoped to the ancestor chain, so unrelated siblings sharing a label are both kept.
func collectTextDedupingAncestors(n a11yNode, ancestorTexts map[string]bool) []string {
	var texts []string
	if t := strings.TrimSpace(n.Text); t != "" && !ancestorTexts[t] {
		texts = append(texts, t)
		extended := make(map[string]bool, len(ancestorTexts)+1)
		for k := range ancestorTexts {
			extended[k] = true
		}
		extended[t] = true
		ancestorTexts = extended
	}
	for _, child := range n.Children {
		texts = append(texts, collectTextDedupingAncestors(child, ancestorTexts)...)
	}
	return texts
}
