package tracker

import (
	"context"
	"testing"
	"time"
)

// A browser publishes the page address on its web document node, as "DocURL" (Firefox) or "URI" (Chromium). Whatever the browser calls it, the address that comes out has no credentials, query string or fragment, and a page that is not on the web has no address at all.
func TestUrlFromAttributes(t *testing.T) {
	cases := []struct {
		name  string
		attrs map[string]string
		want  string
	}{
		{"firefox", map[string]string{"DocURL": "https://example.org/a/b", "MimeType": "text/html"}, "https://example.org/a/b"},
		{"chromium", map[string]string{"URI": "https://example.org/a/b", "DocType": "html"}, "https://example.org/a/b"},
		{"name case is ignored", map[string]string{"docurl": "http://example.org/"}, "http://example.org/"},
		{"query and fragment are dropped", map[string]string{"URI": "https://example.org/p?token=abc&code=1#section"}, "https://example.org/p"},
		{"credentials are dropped", map[string]string{"URI": "https://user:secret@example.org/p"}, "https://example.org/p"},
		{"a trailing question mark is dropped", map[string]string{"URI": "https://example.org/p?"}, "https://example.org/p"},
		{"a browser page that is not on the web", map[string]string{"URI": "chrome://newtab/"}, ""},
		{"a local file", map[string]string{"DocURL": "file:///home/u/notes.html"}, ""},
		{"blank page", map[string]string{"URI": "about:blank"}, ""},
		{"no host", map[string]string{"URI": "https:///path"}, ""},
		{"not a url", map[string]string{"URI": "%%%"}, ""},
		{"no address published", map[string]string{"MimeType": "text/html"}, ""},
		{"nothing published", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := urlFromAttributes(c.attrs); got != c.want {
				t.Fatalf("urlFromAttributes(%v) = %q, want %q", c.attrs, got, c.want)
			}
		})
	}
}

// The address comes from the page, never from the browser's own toolbar: a tree whose toolbar holds an address-bar entry reads as the document node's URL, and a window with no web document has none even when it has text that looks like one.
func TestPageURL(t *testing.T) {
	browser := a11yNode{Role: "frame", Children: []a11yNode{
		{Role: "tool bar", Children: []a11yNode{{Role: "entry", Text: "https://not-the-page.example/"}}},
		{Role: "document web", URL: "https://example.org/docs", Children: []a11yNode{{Role: "paragraph", Text: "hello"}}},
	}}
	if got := pageURL(browser); got != "https://example.org/docs" {
		t.Fatalf("pageURL(browser) = %q, want the document node's address", got)
	}

	loading := a11yNode{Role: "frame", Children: []a11yNode{{Role: "document web"}}}
	if got := pageURL(loading); got != "" {
		t.Fatalf("pageURL(a document that has not published an address) = %q, want nothing", got)
	}

	native := a11yNode{Role: "frame", Children: []a11yNode{{Role: "text", Text: "see https://example.org/ for details"}}}
	if got := pageURL(native); got != "" {
		t.Fatalf("pageURL(native window) = %q, want nothing", got)
	}
}

// The address travels with the capture to the episode, and it is kept when the page's text has not changed since the last capture, because the user can move between two pages that read the same.
func TestTieredCapture_CarriesThePageAddress(t *testing.T) {
	d := NewDaemon(nil, time.Second, time.Second, nil, nil)
	d.media = func(context.Context) bool { return false }
	page := screenRead{text: "a long enough body of text", url: "https://example.org/docs"}
	d.text = func() (screenRead, error) { return page, nil }

	var lastA11y, lastVision string
	var lastVisionTime time.Time
	act := Activity{App: "Brave", Title: "Docs"}

	first := d.tieredCapture(context.Background(), act, &lastA11y, &lastVision, &lastVisionTime)
	if first.url != page.url || first.text != page.text {
		t.Fatalf("first capture = %+v, want the page's text and address", first)
	}

	page.url = "https://example.org/other"
	second := d.tieredCapture(context.Background(), act, &lastA11y, &lastVision, &lastVisionTime)
	if second.text != "" {
		t.Fatalf("second capture text = %q, want nothing for an unchanged page", second.text)
	}
	if second.url != page.url {
		t.Fatalf("second capture url = %q, want %q", second.url, page.url)
	}

	var ev Activity
	applyCapture(&ev, second)
	if ev.URL != page.url {
		t.Fatalf("applyCapture left Activity.URL = %q, want %q", ev.URL, page.url)
	}
}
