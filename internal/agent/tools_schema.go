package agent

import (
	"strings"

	"google.golang.org/genai"

	"june/internal/config"
)

// drawShapeProperties are the fields one shape takes. The flat form of the draw tool and every entry of its shapes array share this one definition, so the two forms cannot drift apart and a model that learned one has learned the other.
func drawShapeProperties() map[string]*genai.Schema {
	return map[string]*genai.Schema{
		"shape": {Type: genai.TypeString, Enum: []string{"arrow", "line", "path", "box", "circle"}},
		"from":  {Type: genai.TypeNumber, Description: "start element"},
		"to":    {Type: genai.TypeNumber, Description: "end element"},
		"on":    {Type: genai.TypeNumber, Description: "element to surround (box, circle)"},
		"points": {
			Type:        genai.TypeArray,
			Description: "[[x,y],...] in look coordinates; two, three for path. Not box or circle.",
			Items:       &genai.Schema{Type: genai.TypeArray, Items: &genai.Schema{Type: genai.TypeNumber}},
		},
		"rect": {
			Type:        genai.TypeObject,
			Description: "{x,y,w,h} in look coordinates (box, circle), instead of on",
			Properties: map[string]*genai.Schema{
				"x": {Type: genai.TypeNumber}, "y": {Type: genai.TypeNumber},
				"w": {Type: genai.TypeNumber}, "h": {Type: genai.TypeNumber},
			},
		},
		"label": {Type: genai.TypeString, Description: "A short label beside it"},
	}
}

// drawParameters are the draw tool's arguments: a list of shapes, always, even for one. Only the list is declared, because declaring a single-shape form beside it would send both field sets on every round of every screen task to say the same thing twice — and because a model shown a list draws a diagram in one call, where one shown both drew a shape per round and spent a whole turn's steps on it. The handler still accepts a bare shape (see drawShapeList), so a model that sends one anyway is answered rather than refused.
func drawParameters() *genai.Schema {
	return &genai.Schema{
		Type: genai.TypeObject,
		Properties: map[string]*genai.Schema{
			"shapes": {
				Type:        genai.TypeArray,
				Description: "All shapes in one call.",
				Items:       &genai.Schema{Type: genai.TypeObject, Properties: drawShapeProperties(), Required: []string{"shape"}},
			},
		},
		Required: []string{"shapes"},
	}
}

// toolDefinitions returns June's own function declarations for the Live API.
// Every declaration is NON_BLOCKING. An unset Behavior means BLOCKING, which tells the Live API to freeze the conversation for the whole duration of a tool call — the model stops speaking and stops listening until the result lands, so a two-second memory lookup becomes two seconds of dead air on a voice call. NON_BLOCKING keeps the model talking and listening while the call runs; the result is folded back in later, at the moment picked by scheduleFor in connect.go. June's own tool execution was already off the receive loop (see runToolCall), so this changes nothing about the transport — only the model-level contract.
func toolDefinitions() []*genai.Tool {
	return []*genai.Tool{{
		FunctionDeclarations: []*genai.FunctionDeclaration{
			{
				Behavior: genai.BehaviorNonBlocking,
				// TODO: need an approve/suggest feature for these tools
				Name:        "shell_exec",
				Description: "Execute a shell command on the user's system. Use powershell syntax on windows, sh on linux/mac. ALWAYS ask for confirmation before running destructive commands (rm, del, format, etc).",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"command": {Type: genai.TypeString, Description: "The shell command to execute"},
					},
					Required: []string{"command"},
				},
			},
			{
				Behavior:    genai.BehaviorNonBlocking,
				Name:        "read_clipboard",
				Description: "Read the current contents of the user's clipboard",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
				},
			},
			{
				Behavior:    genai.BehaviorNonBlocking,
				Name:        "read_file",
				Description: "Read the contents of a file on the user's filesystem. Use this to inspect code, configs, or any text file.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"path": {Type: genai.TypeString, Description: "Absolute or relative file path to read"},
					},
					Required: []string{"path"},
				},
			},
			{
				Behavior:    genai.BehaviorNonBlocking,
				Name:        "list_files",
				Description: "List files and directories at a given path. Returns names with [dir] or [file] prefix.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"path": {Type: genai.TypeString, Description: "Directory path to list. Defaults to current directory if empty."},
					},
				},
			},
			// The declarations from here to open_url ride on every round of every screen task (see screenRoundTools in ask.go), so each states its rule once and stops; the guidance a screen round also carries is in screenTaskGuidance, and TestScreenRoundDeclarations_StayShort holds the whole set to its byte budget.
			{
				Behavior:    genai.BehaviorNonBlocking,
				Name:        "observe_screen",
				Description: "List the window in front: its app and title, then every clickable or readable element showing, numbered, with its position. Call it before point_at and whenever asked what is on screen.",
				Parameters:  &genai.Schema{Type: genai.TypeObject},
			},
			{
				Behavior:    genai.BehaviorNonBlocking,
				Name:        "look",
				Description: "See the window in front as a picture, for what observe_screen cannot list: video, photos, games, drawings, maps, charts, or a frame-only window. It photographs that window's frame, not the whole screen, so the size changes when the front window changes. The result gives the picture's size and place; read points off it in its own coordinates, and only points inside it.",
				Parameters:  &genai.Schema{Type: genai.TypeObject},
			},
			{
				Behavior:    genai.BehaviorNonBlocking,
				Name:        "point_at",
				Description: "Ring one element from the latest observe_screen list so the user can see which you mean.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"n":     {Type: genai.TypeNumber, Description: "Element number"},
						"label": {Type: genai.TypeString, Description: "Two or three words beside the ring"},
					},
					Required: []string{"n"},
				},
			},
			{
				Behavior:    genai.BehaviorNonBlocking,
				Name:        "show_marks",
				Description: "Draw a numbered mark over every element in the latest observe_screen list.",
				Parameters:  &genai.Schema{Type: genai.TypeObject},
			},
			{
				Behavior:    genai.BehaviorNonBlocking,
				Name:        "draw",
				Description: "Mark up the screen, all shapes in one call. arrow/line take from/to or points, path takes points, box/circle take on or rect. Nothing is clicked.",
				Parameters:  drawParameters(),
			},
			{
				Behavior:    genai.BehaviorNonBlocking,
				Name:        "click",
				Description: "Press something. n presses a numbered element from the latest observe_screen list through its own accessibility action, no pointer moves. x,y off the last look's picture clicks a bare point, for what the list has no element or action for. then is further taps fired at once, only for UI that closes before another round can see it: a menu or a toast. Call observe_screen after. Never click anything that sends, pays, deletes or submits unless the user said go.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"n":      {Type: genai.TypeNumber, Description: "Element number"},
						"x":      {Type: genai.TypeNumber, Description: "x in the look's picture"},
						"y":      {Type: genai.TypeNumber, Description: "y in the look's picture"},
						"button": {Type: genai.TypeString, Description: "right for the context menu; left by default"},
						"then": {
							Type:        genai.TypeArray,
							Description: "Further taps, each {n} or {x,y}",
							Items: &genai.Schema{
								Type: genai.TypeObject,
								Properties: map[string]*genai.Schema{
									"n": {Type: genai.TypeNumber},
									"x": {Type: genai.TypeNumber},
									"y": {Type: genai.TypeNumber},
								},
							},
						},
					},
				},
			},
			{
				// Kept deliberately short: this declaration rides on every round of every screen task, and the four kinds fit in one line of the kind argument rather than needing an enum as well.
				Behavior:    genai.BehaviorNonBlocking,
				Name:        "wait_for",
				Description: "Wait for the change an action was expected to make, instead of looking again. Polls to 5s and says what it found.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"kind":       {Type: genai.TypeString, Description: "title_contains, item_present, item_absent, field_holds, or screen_changed for what no list shows"},
						"value":      {Type: genai.TypeString, Description: "the text to look for"},
						"timeout_ms": {Type: genai.TypeNumber, Description: "milliseconds, 5000 by default"},
					},
					Required: []string{"kind", "value"},
				},
			},
			{
				Behavior:    genai.BehaviorNonBlocking,
				Name:        "scroll_to",
				Description: "Scroll the numbered element from the latest observe_screen list into view.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"n": {Type: genai.TypeNumber, Description: "Element number"},
					},
					Required: []string{"n"},
				},
			},
			{
				Behavior:    genai.BehaviorNonBlocking,
				Name:        "type_text",
				Description: "Type into whatever has focus; click the field first. All text goes through here, and never a password, card number or other secret.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"text":  {Type: genai.TypeString, Description: "The text to type"},
						"enter": {Type: genai.TypeBoolean, Description: "Press Enter after the text"},
					},
					Required: []string{"text"},
				},
			},
			{
				Behavior:    genai.BehaviorNonBlocking,
				Name:        "press_key",
				Description: "Press a key or chord, for what no listed element can do; type_text is for text, press_key for keys. It lands where the keyboard focus is, so click the field or player first.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"keys": {Type: genai.TypeString, Description: "One key, or one chord. Enter, Escape, Tab, Space, Up, Down, Left, Right, Backspace, Delete, Home, End, PageUp, PageDown, Insert, Print, F1 to F12, a digit, a letter, a punctuation mark, or a chord like Ctrl+L. One press per call: \"Tab Tab\" is not a key, call press_key twice."},
					},
					Required: []string{"keys"},
				},
			},
			{
				Behavior:    genai.BehaviorNonBlocking,
				Name:        "scroll_at",
				Description: "Scroll at a point, for a pane or player with no element in the list.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"x":  {Type: genai.TypeNumber, Description: "x in the look's picture"},
						"y":  {Type: genai.TypeNumber, Description: "y in the look's picture"},
						"dy": {Type: genai.TypeNumber, Description: "steps to scroll, positive is down"},
					},
					Required: []string{"x", "y", "dy"},
				},
			},
			{
				Behavior:    genai.BehaviorNonBlocking,
				Name:        "open_app",
				Description: "Open an installed application by name, starting it if needed, and bring its window to the front. Spotify, Settings, Slack: this, never open_url.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"app": {Type: genai.TypeString, Description: "The application, as named"},
					},
					Required: []string{"app"},
				},
			},
			{
				Behavior:    genai.BehaviorNonBlocking,
				Name:        "open_url",
				Description: "Open a URL in the default browser, only to reach a page not open yet, never a site's root over one of its own pages, never for an installed application (open_app). It opens the page and reaches nothing on it; the screen tools do that.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"url": {Type: genai.TypeString, Description: "The URL to open"},
					},
					Required: []string{"url"},
				},
			},
			{
				Behavior: genai.BehaviorNonBlocking,
				Name:     "query_memory",
				Description: "Topical search over memory (moments, facts, arcs, period summaries). " +
					"It searches June's record of what the user has already done: it is not a web search and it never reaches or opens anything, so it can no more find a page than remember one nobody visited. For anything on the screen now, or anywhere to get to, use observe_screen and the screen tools. " +
					"Moments (screen observations) rank with recency; facts/notes do not expire. " +
					"Use app to restrict to one application (Slack, Firefox, Code). " +
					"Whenever the question is anchored to a time, a day, a part of a day, a range, pass since/until: " +
					"the search then runs and ranks entirely inside that window, whereas without it the best matches can all come from the wrong day, and one busy stretch can drown out the rest of its own day. " +
					"A part of a day gets timestamp bounds, not the whole day: morning is roughly 06:00-12:00, afternoon 12:00-18:00, evening and night after that. " +
					"When a question narrows the time, run a fresh narrower query, do not answer a narrow question from a wider fetch you already have. " +
					"For pure day/timeline questions, or 'what was I just doing', use recall. " +
					"For what a meeting was about, what it decided, or who was in it, pass kind='meeting': that lists the minutes themselves for the window, newest first, instead of ranking, a ranked search finds screens of the user reading minutes before it finds the minutes.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"kind":   {Type: genai.TypeString, Description: "Optional. 'meeting' lists meeting minutes in the window (newest first, query ignored). Omit for a ranked search."},
						"query":  {Type: genai.TypeString, Description: "What to search for, topic, project, show, person, etc. Never put a time word here ('today', 'yesterday', 'last week'), it will match text instead of dates; use since/until for that."},
						"domain": {Type: genai.TypeString, Description: "Optional. Restrict to 'work' or 'personal' memories only. Omit to search everything, weighted toward whichever domain you're currently in."},
						"app":    {Type: genai.TypeString, Description: "Optional. Restrict moments to this application name (case-insensitive substring, e.g. slack, firefox, code)."},
						"since":  {Type: genai.TypeString, Description: "Optional. Start of the time window results must fall in. 'today', 'yesterday', a bare date (2026-07-05) meaning its start, or a timestamp (2026-07-05T09:30:00). You know the current date/time, convert other phrases into a concrete date yourself. Omit for no lower bound."},
						"until":  {Type: genai.TypeString, Description: "Optional. End of the time window (same formats as 'since'; a bare date covers through the end of that day). Omit to mean up to now. For a single day, set since and until to that same date."},
					},
					Required: []string{"query"},
				},
			},
			{
				Behavior: genai.BehaviorNonBlocking,
				Name:     "query_store",
				Description: "Run one read-only SQL query straight against June's sqlite store, for structural and aggregate questions that a relevance-ranked search cannot answer, counts, group-bys, joins, \"which meetings did I attend today\", \"what hour do I usually stop working\". " +
					"query_memory searches by meaning and ranks by relevance; this reads the tables directly, so use it whenever the real answer is a COUNT, a GROUP BY, a MIN/MAX, or a join across tables rather than the ten most-relevant rows. " +
					"The connection itself is read-only, INSERT/UPDATE/DELETE/DROP/ALTER/PRAGMA-writes fail at the database, not by a filter on your text, so only SELECT, PRAGMA table_info(...), and EXPLAIN can do anything. " +
					"Exactly one statement per call, no trailing statements after a semicolon. " +
					"Every *_at/*_time column is UTC text, for anything the user would call \"today\" or an hour of day, wrap it: datetime(created_at,'localtime') BETWEEN ... " +
					"Results render as a header line of column names, then one line per row with values separated by a TAB, window titles routinely contain pipes and spaces, so a tab is the only separator that stays unambiguous. A query matching nothing says \"no rows matched\" plainly. Output is capped in rows and characters, add LIMIT or aggregate rather than pulling raw rows if you hit the cap. " +
					"Meetings are NOT episodes: an episode is a screen capture, so searching episodes for an app called Teams or Zoom finds the window and never the meeting. A meeting's minutes are a note with kind='meeting'. " +
					"An action note carries its state as a [state/priority] prefix at the start of content, so the things still owed are kind='action' AND content LIKE '[open/%'. " +
					"episodes_fts and memory_fts are full-text indexes: use \"episodes_fts MATCH 'word'\" and join its rowid to episodes.id, or \"memory_fts MATCH 'word'\" where source names the table ref_id points into. " +
					"There is no table called \"memory\", and none called \"tasks\". Facts are in notes, captures in episodes, conversations in threads; memory_fts is only the full-text shadow of notes. The schema below is the whole of it, read from the store itself — nothing outside it exists. " +
					"Schema, read from the store itself. A column listed as \"is one of\" holds only those values:\n" + storeSchemaBlock(),
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"query": {Type: genai.TypeString, Description: "One read-only SQL statement, no trailing statements."},
					},
					Required: []string{"query"},
				},
			},
			{
				Behavior: genai.BehaviorNonBlocking,
				Name:     "recall",
				Description: "Timeline or subject recall. Use since/until for chronological periods (yesterday, last Tuesday). " +
					"A part of a day gets timestamp bounds rather than the whole day, morning roughly 06:00-12:00, afternoon 12:00-18:00, evening and night after that, and a question that narrows the time deserves a fresh narrower call, not an answer read off a wider fetch. " +
					"Use subject for an ongoing arc. Use app to keep only that application's moments. " +
					"Returns short content+context lines, not raw screen dumps.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"subject": {Type: genai.TypeString, Description: "Optional. A subject/topic to recall (fuses the matching thread's arc with diverse episode specifics). Cannot be combined with app, since, or until, use one or the other."},
						"since":   {Type: genai.TypeString, Description: "Optional. Start of the timeline window: 'today', 'yesterday', a bare date (2026-07-05), or a timestamp (2026-07-05T00:00:00). You know the current date/time, convert other phrases like 'July 5th' or 'last week' into a concrete date yourself. Defaults to the start of today."},
						"until":   {Type: genai.TypeString, Description: "Optional. End of the timeline window (same formats as 'since'). A bare date covers the whole day. Defaults to now. For a single day, set since and until to that same date."},
						"app":     {Type: genai.TypeString, Description: "Optional. Restrict moments to this application name (case-insensitive substring)."},
					},
				},
			},
			{
				Behavior: genai.BehaviorNonBlocking,
				Name:     "do",
				Description: "Carry out a job of several actions on the user's machine: it plans the whole thing first, takes one step at a time, checks after each step that the change it expected actually happened, and reads what this machine did the last few times it was asked something similar. " +
					"Use it the moment a request needs more than about three actions, or spans more than one application — \"open Spotify and play this, then open Teams and message Vexil, then see if anyone's replied\", \"open a terminal, start Claude and paste this prompt in\" — and for anything you would otherwise drive by calling observe_screen and click over and over. " +
					"Pass the whole job as one goal in the user's own words, with everything the job needs inside it: a message to send, a prompt to type, a song to play, all of it, because the job cannot hear the conversation it came from. " +
					"It runs beside you and takes minutes, so carry on talking while it does; the outcome arrives as this call's result and that is when to say how it went. " +
					"For one action on the window already in front, use the screen tools directly instead. For a question about the world, use branch.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"goal": {Type: genai.TypeString, Description: "The whole job in one sentence or two, in the user's own words, carrying every detail the job needs."},
					},
					Required: []string{"goal"},
				},
			},
			{
				Behavior: genai.BehaviorNonBlocking,
				Name:     "branch",
				Description: "Resolve one open-ended question or research task that needs cross-referencing " +
					"several searches to build a complete answer (e.g. \"catch me up on everything about the " +
					"Riddler project\", or a question spanning multiple topics/timeframes), instead of calling " +
					"query_memory/recall repeatedly yourself. THIS IS ALSO THE ONLY WAY TO REACH THE WEB: it is the one tool with live search, " +
					"so anything outside the user's own life, news, prices, documentation, a fact you are not certain of, goes here. " +
					"Never open a browser to answer a question; opening a page shows it to the user and tells you nothing. " +
					"Each result comes back with its page's URL, so when the user asks to see one, pass that URL straight to open_url; never type out a URL you did not get from a result, since a guessed one lands the user on a missing page. " +
					"Runs an internal multi-step search in the " +
					"background and returns only the final synthesized answer; you will not see, and must not " +
					"need, its intermediate steps. Prefer query_memory/recall directly for a single simple lookup.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"task": {Type: genai.TypeString, Description: "The open-ended question or research task to resolve."},
					},
					Required: []string{"task"},
				},
			},
			{
				Behavior: genai.BehaviorNonBlocking,
				Name:     "add_task",
				Description: "Put something on the user's task list. Use this whenever they ask for a todo, a task, a reminder to do " +
					"something, or say to add something to their list, anything they intend to DO. " +
					"save_note is for a fact to remember and files nothing on the list, so a task saved as a note never appears in " +
					"Tasks: if the user asked for a todo and you called save_note, you did not do what they asked. " +
					"Write the title as the work itself, in the user's own words, short enough to read in a list: " +
					"\"add source pdfs to the excel files and link to the exact pages\", not \"the user wants to...\". " +
					"Only say it is on their list once this has come back saying it was added. It comes back with the task's ref, \"task#N\", which revise takes if they correct or drop it a moment later.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"title": {Type: genai.TypeString, Description: "The work to do, as the user would read it back on their list."},
					},
					Required: []string{"title"},
				},
			},
			{
				Behavior:    genai.BehaviorNonBlocking,
				Name:        "save_note",
				Description: "Save a durable fact the user tells you in conversation, identity, preferences, plans, relationships, projects. This is the only way something said in conversation reaches long-term memory. Not for a todo, that is add_task.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"content": {Type: genai.TypeString, Description: "The fact to remember, as a durable statement, not a command."},
					},
					Required: []string{"content"},
				},
			},
			{
				Behavior: genai.BehaviorNonBlocking,
				Name:     "personal_context",
				Description: "The small store of things known for CERTAIN about the user: who they are, the people in their life " +
					"(family, colleagues, friends), and preferences they have stated. Every entry goes into every conversation you " +
					"have with them, so it stays small and it stays true. " +
					"action \"set\" is only for something the user said about themselves, or confirmed when you asked them. Never " +
					"put in something you inferred, guessed, or read off their screen, an observation belongs in save_note instead. " +
					"Call action \"view\" before writing: subjects you already have come back with it, and if one of them covers what " +
					"you were about to add, edit that subject rather than making a near-duplicate. " +
					"action \"delete\" is for an entry the user says is wrong or no longer true.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"action":  {Type: genai.TypeString, Description: "\"view\" to read everything stored, \"set\" to write or edit one subject, \"delete\" to remove one."},
						"subject": {Type: genai.TypeString, Description: "Short key for the entry, lowercase and hyphenated: \"identity\", \"vexil-quorin\", \"preferences-communication\". Required for set and delete. Reuse an existing subject to edit it."},
						"content": {Type: genai.TypeString, Description: "For set: the whole entry, written as plain prose about the user or that person. It replaces the subject's previous content, so include what still holds, not just the new part."},
					},
					Required: []string{"action"},
				},
			},
			{
				Behavior: genai.BehaviorNonBlocking,
				Name:     "revise",
				Description: "Fix or remove something June remembered wrong, or something on the task list: a note, an action item, a thread, or a task. " +
					"Look it up first with query_memory or recall to get its ref, the \"[note#N]\" or \"[thread#N]\" a result showed you, then call this. A task's ref is the \"task#N\" add_task handed back, or its id in user_tasks. " +
					"Pass content to correct the text. For an action item, pass state (open, done, or dropped) instead, never leave a task the user says is done still open, and priority (high, normal, or low) when they say how much it matters. " +
					"Pass remove to delete a note or a task entirely (a thread cannot be removed, only corrected). A task the user says to drop is deleted, since the list has nowhere to keep a dropped one. " +
					"Use this instead of just apologizing out loud and leaving the wrong fact in memory, and never say you cannot change something on the list.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"ref":      {Type: genai.TypeString, Description: "The reference exactly as a result showed it: \"note#12\", \"thread#3\" or \"task#5\", brackets optional."},
						"content":  {Type: genai.TypeString, Description: "Optional. The corrected text. Omit to leave it unchanged."},
						"state":    {Type: genai.TypeString, Description: "Optional. For an action item or a task: open, done, or dropped."},
						"priority": {Type: genai.TypeString, Description: "Optional. For an action item only: high, normal, or low. Everything starts normal, so set this only when the user says how much something matters."},
						"remove":   {Type: genai.TypeBoolean, Description: "Optional. Delete the row entirely, cannot be combined with content, state, or priority."},
					},
					Required: []string{"ref"},
				},
			},
			{
				Behavior: genai.BehaviorNonBlocking,
				Name:     "action_items",
				Description: "List what the user still owes, the things they agreed to do in a meeting and have not " +
					"closed. Use this for any question about outstanding work, owed tasks, commitments, what is on " +
					"their plate, or what they need to do. Do not use query_memory for those: an action item's text " +
					"is the task itself and shares no words with the question, so searching for it finds meetings " +
					"about meetings instead. This reads the list directly. Each result carries its id, so revise " +
					"can close one straight afterwards.",
				Parameters: &genai.Schema{Type: genai.TypeObject, Properties: map[string]*genai.Schema{}},
			},
			// The delegate tool is declared in delegate.go beside its handler; screen rounds trim to screenRoundTools, so it costs them nothing.
			delegateTool,
		},
	}}
}

// liveTools returns every tool exposed to the Live API session: June's own FunctionDeclarations (shell_exec, query_memory, save_note, etc.) plus Gemini's native GoogleSearch grounding tool, so June can look something up instead of guessing from memory.
// Verified live (2026-07-25) that both tool types work together on config.VoiceModel() (gemini-2.5-flash-native-audio-preview-12-2025) — not guaranteed on every Gemini model/endpoint.
// GoogleSearch calls are grounded server-side by Gemini and never surface as a ToolCall, so they don't show up in ToolActivityChan the way the FunctionDeclarations tools do.
func liveTools() []*genai.Tool {
	return liveToolsFor(config.VoiceModel())
}

// liveToolsFor is liveTools for a named Live model. Google Search grounding rides beside the function tools on the 2.5 model; on the 3.x Live models the same pairing closes the session with "You exceeded your current quota" before the first word (probed on 2026-09-02, every other part of the handshake passes), so there it is left out and the model has no web search.
func liveToolsFor(model string) []*genai.Tool {
	tools := toolDefinitions()
	if !HasApprover() {
		tools = dropApprovalGated(tools)
	}
	// Flip point for gemini-3.8-live: its docs list Google Search grounding as supported (read 2026-09-17), so once someone dials a real session with grounding sent beside the function tools and it survives past the first word, exempt config.Live38Model from this branch the way thinkingConfigFor already does. Until that probe it stays off, because a 429 closes the whole voice session a quarter second after connect while a missing web search only degrades it.
	if strings.HasPrefix(model, "gemini-3") {
		return tools
	}
	return append(tools, &genai.Tool{GoogleSearch: &genai.GoogleSearch{}})
}

// dropApprovalGated returns the tools with every approvalGatedTools declaration taken out, the same shape askTools builds through askAllowedTools. A tool the session cannot actually run must not be declared: the model spends a round finding out, and for these three the finding out is a call that never comes back. Input: the declared tools; the originals are not modified. Output: a copy of each tool that still has a declaration left, plus any entry carrying no declarations at all (Gemini's own search grounding) as it is.
func dropApprovalGated(tools []*genai.Tool) []*genai.Tool {
	out := make([]*genai.Tool, 0, len(tools))
	for _, tool := range tools {
		if len(tool.FunctionDeclarations) == 0 {
			out = append(out, tool)
			continue
		}
		copyTool := *tool
		copyTool.FunctionDeclarations = nil
		for _, d := range tool.FunctionDeclarations {
			if !approvalGatedTools[d.Name] {
				copyTool.FunctionDeclarations = append(copyTool.FunctionDeclarations, d)
			}
		}
		if len(copyTool.FunctionDeclarations) > 0 {
			out = append(out, &copyTool)
		}
	}
	return out
}

// ToolDeclarations returns the function declarations the live session exposes, the same list liveTools builds. Gemini's native search tool is not included because it has no declaration to hand a non-live model. The trajectory eval in evals/ uses it to give a text-mode model the identical tool surface the voice session has. Input: none. Output: the declarations, in the order the live session sends them.
func ToolDeclarations() []*genai.FunctionDeclaration {
	tools := liveTools()
	if len(tools) == 0 {
		return nil
	}
	return tools[0].FunctionDeclarations
}
