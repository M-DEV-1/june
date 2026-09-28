package tracker

import "testing"

func TestDocumentText(t *testing.T) {
	cases := []struct {
		name string
		tree a11yNode
		want string
	}{
		{
			name: "web page drops chrome",
			tree: a11yNode{
				Role: "frame",
				Children: []a11yNode{
					{
						Role: "tool bar",
						Children: []a11yNode{
							{Role: "push button", Text: "Back"},
							{Role: "push button", Text: "Forward"},
							{Role: "push button", Text: "Brave Shields"},
						},
					},
					{
						Role: roleDocumentWeb,
						Children: []a11yNode{
							{Role: "heading", Text: "Article headline"},
							{Role: "paragraph", Text: "the actual article text"},
						},
					},
				},
			},
			want: "Article headline\nthe actual article text",
		},
		{
			// Native apps must fall back to full-tree text.
			name: "native app falls back to full tree",
			tree: a11yNode{
				Role: "frame",
				Children: []a11yNode{
					{Role: "menu bar", Text: "File Edit View"},
					{
						Role: "panel",
						Children: []a11yNode{
							{Role: "terminal", Text: "$ echo hello"},
						},
					},
				},
			},
			want: "File Edit View\n$ echo hello",
		},
		{
			name: "multiple web documents",
			tree: a11yNode{
				Role: "frame",
				Children: []a11yNode{
					{Role: roleDocumentWeb, Children: []a11yNode{{Role: "paragraph", Text: "frame one"}}},
					{Role: roleDocumentWeb, Children: []a11yNode{{Role: "paragraph", Text: "frame two"}}},
				},
			},
			want: "frame one\nframe two",
		},
		{
			// Must not fall back to chrome just because the doc is empty.
			name: "empty web document does not fall back to chrome",
			tree: a11yNode{
				Role: "frame",
				Children: []a11yNode{
					{Role: "tool bar", Children: []a11yNode{{Role: "push button", Text: "Back"}}},
					{Role: roleDocumentWeb}, // still loading, no text yet
				},
			},
			want: "",
		},
		{
			// Reproduces a real production bug: AT-SPI's Text interface on a container node (e.g. a link) returns its descendants' aggregated text, doubling every link/label on a captured page ("LEARN MORE LEARN MORE"). Parent/child duplicate text must be collapsed.
			name: "dedupes parent/child text aggregation",
			tree: a11yNode{
				Role: roleDocumentWeb,
				Children: []a11yNode{
					{
						Role: "link",
						Text: "LEARN MORE", // container's GetText aggregates its child's text
						Children: []a11yNode{
							{Role: "static text", Text: "LEARN MORE"},
						},
					},
					{Role: "paragraph", Text: "the actual article text"},
				},
			},
			want: "LEARN MORE\nthe actual article text",
		},
		{
			// Ensures the dedup above is scoped to parent/child aggregation, not "one occurrence per page" — two unrelated links sharing a label must both survive.
			name: "keeps distinct repeated labels",
			tree: a11yNode{
				Role: roleDocumentWeb,
				Children: []a11yNode{
					{Role: "link", Text: "LEARN MORE"},
					{Role: "paragraph", Text: "Product A"},
					{Role: "link", Text: "LEARN MORE"},
					{Role: "paragraph", Text: "Product B"},
				},
			},
			want: "LEARN MORE\nProduct A\nLEARN MORE\nProduct B",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := documentText(c.tree); got != c.want {
				t.Errorf("documentText = %q, want %q", got, c.want)
			}
		})
	}
}
