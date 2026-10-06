package tracker

import (
	"strings"
	"unicode"

	"june/internal/act"
)

// uiaNode is one UI Automation element as the Windows host script (uia.ps1) reports it. The field names are the script's short JSON keys.
type uiaNode struct {
	D       int    `json:"d"` // depth below the window the walk started at, 0 for the window itself
	Control int    `json:"c"` // UIA ControlType id, such as 50000 for a button
	Name    string `json:"n"` // the Name property
	Value   string `json:"v"` // the ValuePattern value, "" for a password box or when there is none
	Text    string `json:"t"` // the TextPattern document text, read only by a capture walk and by a contents read of a focused element that has no value (see FocusedContents)
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
	Err     string    `json:"err"`
	Nodes   []uiaNode `json:"nodes"`
	Action  string    `json:"action"`
	Pending bool      `json:"pending"` // an act's action was fired and had not returned when the request's ms ran out
	Pid     uint32    `json:"p"`
	None    bool      `json:"none"`
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

// uiaLabel is the label a person would use for an element: its name, or its value when it has no name, as the Linux walk falls back to a node's text. Input: the element. Output: the trimmed label, a value's line breaks as "\n" (see uiaLineEnds).
func uiaLabel(n uiaNode) string {
	if s := strings.TrimSpace(n.Name); s != "" {
		return s
	}
	return strings.TrimSpace(uiaLineEnds(n.Value))
}

// uiaPageValue is the text of a page that has a name of its own, which Observe hands on in act.Node.Value for the listing to show after the name. Input: the element. Output: the page's text, its line breaks as "\n" (see uiaLineEnds), or "" for anything else: a password box, an element with no name (uiaLabel lists that one by its value already), or one whose value is its name over again.
// A page is a Document that takes typing, such as Win11 Notepad's, or an Edit whose value runs over more than one line, such as classic Notepad's. Notepad names its page "Text editor" whatever it holds, so listed by name alone the text was nowhere in the listing and the model fell back to a picture of it. The text travels apart from the label rather than in it because the label is what the stop lines judge a click or a typing by, and a note that began "buy milk" or "card pin" made clicking into it ask for consent, or typing into it be refused outright. A single-line box keeps to its name, as a named box does on Linux.
func uiaPageValue(n uiaNode) string {
	name := strings.TrimSpace(n.Name)
	value := strings.TrimSpace(uiaLineEnds(n.Value))
	page := (n.Control == uiaDocument && n.Writable) || (n.Control == uiaEdit && strings.Contains(value, "\n"))
	if !page || n.Password || name == "" || value == name {
		return ""
	}
	return value
}

// uiaKeyTipSide is the longest side, in physical pixels, of the text of a KeyTip: a 12 to 14 px letter at up to 300% display scale.
const uiaKeyTipSide = 48

// uiaKeyTip reports whether an element is the letter of a KeyTip, the badges a WinUI or UWP app shows over its controls while the user holds Alt, rather than text of the window's own. Input: the element. Output: true for a text element named one or two capital letters, drawn no larger than uiaKeyTipSide (or with no rectangle at all). Callers ask it only of elements outside a Document: a web page has no KeyTips, and its text is where a small lone label is most often content.
// Each KeyTip is a text element of the window's tree for as long as it shows, so a capture made then began "P C S T K G I B R L H E U F V" in Notepad and "I H" in Calculator, and a listing carried each letter as a line of text. Where in the tree they hang has not been measured, so they are told by what they are, at the cost of a lone capital or two that small anywhere else in a native window. Letters only, because every KeyTip measured was one, while a lone digit that small is more often a count worth keeping.
func uiaKeyTip(n uiaNode) bool {
	if n.Control != uiaText || max(n.W, n.H) > uiaKeyTipSide {
		return false
	}
	name := []rune(strings.TrimSpace(n.Name))
	if len(name) == 0 || len(name) > 2 {
		return false
	}
	for _, r := range name {
		if !unicode.IsUpper(r) {
			return false
		}
	}
	return true
}

// uiaLineEnds turns the "\r\n" and the bare "\r" a Windows control ends its lines with into "\n". Win11 Notepad's TextPattern and ValuePattern end every line in a bare "\r", which nothing downstream reads as a line break: the capture's cleaning deleted it and fused the last word of one line onto the first of the next ("alpha 42second").
func uiaLineEnds(s string) string {
	if !strings.Contains(s, "\r") {
		return s
	}
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
}

// uiaActNodes turns a walk's elements into the nodes Observe returns. Input: the elements in tree order, each with its depth. Output: one act.Node per element with an actionable role, in the same order, labelled by uiaLabel, with a named page's text in Value (see uiaPageValue); a static text element with no label is left out, because act.Filter would otherwise keep it as an empty box to type in and on Windows such an element is never one, and so is a KeyTip's letter outside a Document (see uiaKeyTip).
func uiaActNodes(nodes []uiaNode) []act.Node {
	out := make([]act.Node, 0, len(nodes))
	// doc is the depth of the Document the elements in hand are under, -1 outside any. An observe walk reports every Document, so an element at its depth or above is past its subtree.
	doc := -1
	for _, n := range nodes {
		if doc >= 0 && n.D <= doc {
			doc = -1
		}
		if doc < 0 && n.Control == uiaDocument {
			doc = n.D
		}
		role := uiaRole(n)
		if !act.Actionable(role) || (doc < 0 && uiaKeyTip(n)) {
			continue
		}
		label := uiaLabel(n)
		if n.Control == uiaText && label == "" {
			continue
		}
		out = append(out, act.Node{Role: role, Label: label, X: n.X, Y: n.Y, W: n.W, H: n.H, Showing: !n.Offscreen, Ref: n.Ref, Value: uiaPageValue(n)})
	}
	return out
}

// uiaChrome is the control types whose name is window furniture rather than content, so a capture leaves their names out of the text.
var uiaChrome = map[int]bool{uiaWindow: true, uiaPane: true, uiaTitleBar: true, uiaScrollBr: true, uiaThumb: true}

// uiaTree rebuilds the walked window as the tree documentText reads. Input: the elements in depth-first order, each with its depth. Output: the tree rooted at the first element, the zero node for an empty walk. Each node's text is its TextPattern text, else its value, else its name unless it is window furniture (see uiaChrome); a KeyTip's letter outside a Document has none (see uiaKeyTip).
func uiaTree(nodes []uiaNode) a11yNode {
	if len(nodes) == 0 {
		return a11yNode{}
	}
	var build func(i int, inDoc bool) (a11yNode, int)
	build = func(i int, inDoc bool) (a11yNode, int) {
		n := nodes[i]
		out := a11yNode{Role: uiaRole(n)}
		// Asked before the text is read, because a capture walk reads a XAML text element's letter through its TextPattern as well.
		if inDoc || !uiaKeyTip(n) {
			out.Text = uiaNodeText(n)
		}
		inDoc = inDoc || n.Control == uiaDocument
		j := i + 1
		for j < len(nodes) && nodes[j].D > n.D {
			var kid a11yNode
			kid, j = build(j, inDoc)
			out.Children = append(out.Children, kid)
		}
		return out, j
	}
	root, _ := build(0, false)
	return root
}

// uiaNodeText is the text one element contributes to a capture. Input: the element. Output: its TextPattern text, else its value, else its name, else "", with its line breaks as "\n" (see uiaLineEnds).
func uiaNodeText(n uiaNode) string {
	for _, s := range []string{n.Text, n.Value} {
		if s = strings.TrimSpace(uiaLineEnds(s)); s != "" {
			return s
		}
	}
	if uiaChrome[n.Control] {
		return ""
	}
	return strings.TrimSpace(n.Name)
}
