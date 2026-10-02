package tracker

import (
	"strings"

	"june/internal/act"
)

// uiaNode is one UI Automation element as the Windows host script (uia.ps1) reports it. The field names are the script's short JSON keys.
type uiaNode struct {
	D       int    `json:"d"` // depth below the window the walk started at, 0 for the window itself
	Control int    `json:"c"` // UIA ControlType id, such as 50000 for a button
	Name    string `json:"n"` // the Name property
	Value   string `json:"v"` // the ValuePattern value, "" for a password box or when there is none
	Text    string `json:"t"` // the TextPattern document text, read only by a capture walk
	// X, Y, W and H are the bounding rectangle in physical virtual-desktop pixels, all zero when the element has none.
	X         int    `json:"x"`
	Y         int    `json:"y"`
	W         int    `json:"w"`
	H         int    `json:"h"`
	Offscreen bool   `json:"off"` // IsOffscreen
	Password  bool   `json:"pw"`  // IsPassword
	Toggle    bool   `json:"tg"`  // IsTogglePatternAvailable
	Writable  bool   `json:"wr"`  // ValuePattern present with IsReadOnly false
	Focused   bool   `json:"f"`   // HasKeyboardFocus, or for the focused op whether the keyboard is at or under the element
	Ref       string `json:"id"`  // "hwnd:runtime.id.parts", the handle the act functions take
}

// uiaReply is one line the host script answers a request with. Err is set when the request failed; the other fields belong to whichever op was asked. A desc or focus reply carries the element itself in the embedded node.
type uiaReply struct {
	uiaNode
	Err    string    `json:"err"`
	Nodes  []uiaNode `json:"nodes"`
	Action string    `json:"action"`
	Pid    uint32    `json:"p"`
	None   bool      `json:"none"`
}

// UIA ControlType ids this file branches on (UIA_*ControlTypeId).
const (
	uiaButton   = 50000
	uiaEdit     = 50004
	uiaText     = 50020
	uiaDocument = 50030
	uiaWindow   = 50032
	uiaPane     = 50033
	uiaTitleBar = 50037
	uiaScrollBr = 50014
	uiaThumb    = 50027
)

// uiaRoles maps UIA ControlType ids to the AT-SPI role words the Linux walk produces, so act.Filter, act.ContentRole and the prompts read a Windows listing the same way. Button, Edit and Document are decided in uiaRole because they depend on the element's flags.
var uiaRoles = map[int]string{
	50001: "calendar", 50002: "check box", 50003: "combo box", 50005: "link", 50006: "image",
	50007: "list item", 50008: "list", 50009: "menu", 50010: "menu bar", 50011: "menu item",
	50012: "progress bar", 50013: "radio button", 50014: "scroll bar", 50015: "slider", 50016: "spin button",
	50017: "status bar", 50018: "page tab list", 50019: "page tab", 50020: "text", 50021: "tool bar",
	50022: "tool tip", 50023: "tree", 50024: "tree item", 50025: "unknown", 50026: "panel",
	50027: "unknown", 50028: "table", 50029: "table cell", 50031: "push button", 50032: "frame",
	50033: "panel", 50034: "header", 50035: "table column header", 50036: "table", 50037: "title bar",
	50038: "separator", 50039: "panel", 50040: "tool bar",
}

// uiaRole names an element's role in AT-SPI words. Input: the element as the script reported it. Output: the role, "unknown" for a control type not in the table.
// A Document that takes typing (Win11 Notepad's page) is an "entry" so the model can click into it; one that does not (a browser's web page) is "document web", which is what documentText keeps a browser's page text by.
func uiaRole(n uiaNode) string {
	switch n.Control {
	case uiaButton:
		if n.Toggle {
			return "toggle button"
		}
		return "push button"
	case uiaEdit:
		if n.Password {
			return "password text"
		}
		return "entry"
	case uiaDocument:
		if n.Writable {
			return "entry"
		}
		return roleDocumentWeb
	}
	if r, ok := uiaRoles[n.Control]; ok {
		return r
	}
	return "unknown"
}

// uiaLabel is the label a person would use for an element: its name, or its value when it has no name, as the Linux walk falls back to a node's text. Input: the element. Output: the trimmed label.
func uiaLabel(n uiaNode) string {
	if s := strings.TrimSpace(n.Name); s != "" {
		return s
	}
	return strings.TrimSpace(n.Value)
}

// uiaActNodes turns a walk's elements into the nodes Observe returns. Input: the elements in tree order. Output: one act.Node per element with an actionable role, in the same order; a static text element with no label is left out, because act.Filter would otherwise keep it as an empty box to type in and on Windows such an element is never one.
func uiaActNodes(nodes []uiaNode) []act.Node {
	out := make([]act.Node, 0, len(nodes))
	for _, n := range nodes {
		role := uiaRole(n)
		if !act.Actionable(role) {
			continue
		}
		label := uiaLabel(n)
		if n.Control == uiaText && label == "" {
			continue
		}
		out = append(out, act.Node{Role: role, Label: label, X: n.X, Y: n.Y, W: n.W, H: n.H, Showing: !n.Offscreen, Ref: n.Ref})
	}
	return out
}

// uiaChrome is the control types whose name is window furniture rather than content, so a capture leaves their names out of the text.
var uiaChrome = map[int]bool{uiaWindow: true, uiaPane: true, uiaTitleBar: true, uiaScrollBr: true, uiaThumb: true}

// uiaTree rebuilds the walked window as the tree documentText reads. Input: the elements in depth-first order, each with its depth. Output: the tree rooted at the first element, the zero node for an empty walk. Each node's text is its TextPattern text, else its value, else its name unless it is window furniture (see uiaChrome).
func uiaTree(nodes []uiaNode) a11yNode {
	if len(nodes) == 0 {
		return a11yNode{}
	}
	var build func(i int) (a11yNode, int)
	build = func(i int) (a11yNode, int) {
		n := nodes[i]
		out := a11yNode{Role: uiaRole(n), Text: uiaNodeText(n)}
		j := i + 1
		for j < len(nodes) && nodes[j].D > n.D {
			var kid a11yNode
			kid, j = build(j)
			out.Children = append(out.Children, kid)
		}
		return out, j
	}
	root, _ := build(0)
	return root
}

// uiaNodeText is the text one element contributes to a capture. Input: the element. Output: its TextPattern text, else its value, else its name, else "".
func uiaNodeText(n uiaNode) string {
	for _, s := range []string{n.Text, n.Value} {
		if s = strings.TrimSpace(s); s != "" {
			return s
		}
	}
	if uiaChrome[n.Control] {
		return ""
	}
	return strings.TrimSpace(n.Name)
}
