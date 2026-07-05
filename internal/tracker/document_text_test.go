package tracker

import "testing"

func TestDocumentText_WebPage_DropsChrome(t *testing.T) {
	tree := a11yNode{
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
	}

	got := documentText(tree)
	want := "Article headline\nthe actual article text"
	if got != want {
		t.Errorf("documentText = %q, want %q", got, want)
	}
	if got == "Back" || got == "Forward" || got == "Brave Shields" {
		t.Errorf("documentText leaked chrome text: %q", got)
	}
}

func TestDocumentText_NativeApp_FallsBackToFullTree(t *testing.T) {
	tree := a11yNode{
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
	}

	got := documentText(tree)
	want := "File Edit View\n$ echo hello"
	if got != want {
		t.Errorf("documentText = %q, want %q (native apps must fall back to full-tree text)", got, want)
	}
}

func TestDocumentText_MultipleWebDocuments(t *testing.T) {
	tree := a11yNode{
		Role: "frame",
		Children: []a11yNode{
			{Role: roleDocumentWeb, Children: []a11yNode{{Role: "paragraph", Text: "frame one"}}},
			{Role: roleDocumentWeb, Children: []a11yNode{{Role: "paragraph", Text: "frame two"}}},
		},
	}

	got := documentText(tree)
	want := "frame one\nframe two"
	if got != want {
		t.Errorf("documentText = %q, want %q", got, want)
	}
}

func TestDocumentText_EmptyWebDocument_DoesNotFallBackToChrome(t *testing.T) {
	tree := a11yNode{
		Role: "frame",
		Children: []a11yNode{
			{Role: "tool bar", Children: []a11yNode{{Role: "push button", Text: "Back"}}},
			{Role: roleDocumentWeb}, // still loading, no text yet
		},
	}

	got := documentText(tree)
	if got != "" {
		t.Errorf("documentText = %q, want empty (must not fall back to chrome just because doc is empty)", got)
	}
}
