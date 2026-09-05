package memory

import (
	"strings"

	oratext "ora/internal/text"
)

// Domain partitions memory into work vs. personal; used by hybrid search's domain filter/boost (internal/db's HybridSearch).
type Domain string

const (
	DomainWork     Domain = "work"
	DomainPersonal Domain = "personal"
	DomainUnset    Domain = ""
)

// workApps are case-insensitive app/title substrings that signal work: dev tools, terminals, office/comms apps.
// Only distinctive terms belong here — short/generic ones that collide with ordinary English go in workAppOnly instead.
var workApps = []string{
	"slack", "vscode", "visual studio code", "zoom", "outlook",
	"iterm2", "iterm", "terminal", "jira", "confluence", "github", "gitlab",
	"docker", "kubernetes", "postman", "figma", "notion", "linear",
	"powerpoint", "google docs", "google sheets", "asana",
	"intellij", "pycharm", "goland", "xcode", "sourcetree", "webstorm",
}

// workAppOnly are work terms too short/generic for free-text titles — "code", "word", "excel" collide with ordinary English ("decode", "barcode", "Wordle", "password", "keyword", "excellent").
// Matched only against the app name, never the title.
var workAppOnly = []string{"code", "word", "excel", "teams"}

// personalApps are case-insensitive app/title substrings that signal a personal context: streaming, gaming, social media.
var personalApps = []string{
	"netflix", "steam", "spotify", "instagram", "youtube", "twitch",
	"tiktok", "facebook", "hulu", "disney+", "playstation", "xbox",
	"whatsapp", "snapchat", "reddit", "pinterest", "tinder",
}

// Classify is a lookup, not a model call, so it's cheap enough to run on every write.
// It matches app/title against curated work/personal substring lists (workAppOnly terms match the app name only, never the free-text title).
// Ambiguous signal — matches neither list or both — resolves to DomainUnset rather than guessing.
func Classify(app, title string) Domain {
	app = strings.ToLower(strings.TrimSpace(app))
	combined := strings.ToLower(strings.TrimSpace(app + " " + title))
	if combined == "" {
		return DomainUnset
	}

	isWork := oratext.ContainsAny(combined, workApps...) || oratext.ContainsAny(app, workAppOnly...)
	isPersonal := oratext.ContainsAny(combined, personalApps...)

	switch {
	case isWork && isPersonal:
		return DomainUnset
	case isWork:
		return DomainWork
	case isPersonal:
		return DomainPersonal
	default:
		return DomainUnset
	}
}
