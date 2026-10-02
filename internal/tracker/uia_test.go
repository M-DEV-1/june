package tracker

import (
	"encoding/json"
	"testing"

	"june/internal/act"
)

// TestUIAReplyToNodesAndText decodes a walk reply shaped like the one uia.ps1 writes and checks both readings of it: the act.Node list Observe returns and the text a capture keeps.
func TestUIAReplyToNodesAndText(t *testing.T) {
	line := `{"nodes":[
		{"d":0,"c":50032,"n":"Inbox - Browser","x":0,"y":0,"w":1920,"h":1080,"id":"7:1"},
		{"d":1,"c":50000,"n":"Back","x":10,"y":10,"w":30,"h":30,"id":"7:2"},
		{"d":1,"c":50000,"n":"Mute","tg":true,"x":50,"y":10,"w":30,"h":30,"id":"7:3"},
		{"d":1,"c":50004,"n":"","v":"example.com","x":90,"y":10,"w":400,"h":30,"id":"7:4"},
		{"d":1,"c":50030,"n":"Inbox","t":"Hello there\nSecond line","x":0,"y":50,"w":1920,"h":1000,"id":"7:5"},
		{"d":2,"c":50020,"n":"Hello there","x":5,"y":60,"w":100,"h":20,"id":"7:6"},
		{"d":2,"c":50020,"n":"","x":5,"y":90,"w":100,"h":20,"id":"7:7"},
		{"d":2,"c":50004,"n":"Password","pw":true,"x":5,"y":120,"w":200,"h":20,"off":true,"id":"7:8"},
		{"d":1,"c":50030,"n":"Notes","wr":true,"x":0,"y":0,"w":10,"h":10,"id":"7:9"}
	]}`
	var r uiaReply
	if err := json.Unmarshal([]byte(line), &r); err != nil {
		t.Fatal(err)
	}

	got := uiaActNodes(r.Nodes)
	want := []act.Node{
		{Role: "push button", Label: "Back", X: 10, Y: 10, W: 30, H: 30, Showing: true, Ref: "7:2"},
		{Role: "toggle button", Label: "Mute", X: 50, Y: 10, W: 30, H: 30, Showing: true, Ref: "7:3"},
		{Role: "entry", Label: "example.com", X: 90, Y: 10, W: 400, H: 30, Showing: true, Ref: "7:4"},
		{Role: "text", Label: "Hello there", X: 5, Y: 60, W: 100, H: 20, Showing: true, Ref: "7:6"},
		{Role: "password text", Label: "Password", X: 5, Y: 120, W: 200, H: 20, Showing: false, Ref: "7:8"},
		{Role: "entry", Label: "Notes", W: 10, H: 10, Showing: true, Ref: "7:9"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d nodes, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("node %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	// The read-only document is a web page, so only its subtree's text is kept: its TextPattern text, and the static text under it that repeats a line of it is a separate node so it stays.
	if text := documentText(uiaTree(r.Nodes)); text != "Hello there\nSecond line\nHello there\nPassword" {
		t.Errorf("documentText = %q", text)
	}
}
