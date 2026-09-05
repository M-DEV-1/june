# Ora window design system

The rules this window is drawn by, and where each number came from. Every token below exists in `src/next/index.css`; nothing outside that file names a colour, a font size or a radius.

## Where the numbers came from

Four products were measured rather than admired.

- **Linear** publishes its scale: Inter at weights 400/510/590, body 16/1.5, body-small 15/1.6, caption 13/1.2, a 4px spacing base, radii 2/6/12/pill, and a cool neutral ramp from `#08090a` to `#ffffff` with `#8a8f98` as the muted text. Its headings carry negative tracking that goes to zero below 16px. ([refero](https://styles.refero.design/style/90ce5883-bb24-4466-93f7-801cd617b0d1))
- **Raycast** uses an 8px spacing scale, radii 6/8/16/pill, Inter for text and a mono face reserved for machine values, and it aims at "a regular Mac app": tight rows, controlled line height, no poster headings. ([refero](https://styles.refero.design/style/3b6a17f0-3bdf-418c-a95e-0b89e5a8b2f8), [Raycast blog](https://www.raycast.com/blog/a-fresh-look-and-feel))
- **Reading typography**, the settled advice: body line-height about 1.5, a measure of 60–75 characters, no letter-spacing on body text; a longer measure wants 1.6. ([Made Good](https://madegooddesigns.com/line-height-letter-spacing/))
- **Vertical rhythm**: the space above a heading should be 1.25–2 line-heights and the space below it 0.375–0.75, so a heading sits nearer the text it introduces than the text it follows. List items want at least 0.5em between them. ([webtypography.net](http://webtypography.net/2.2.2))
- **Dark mode**: never `#000000` and never `#ffffff` text; build depth by making raised surfaces lighter, and desaturate the accent so it does not vibrate on a dark ground. ([atmos](https://atmos.style/blog/dark-mode-ui-best-practices))

## Type

Geist Variable, bundled with the app so nothing is fetched at run time, with the desktop's own face behind it. It is a neutral grotesque in the Inter family of shapes, which is what Linear and Raycast both use; it is deliberately not Cantarell, because Cantarell is what makes a page look like a GTK dialog.

| Token | Size | Line height | Weight | Tracking | What it is for |
|---|---|---|---|---|---|
| `text-micro` | 11px | 1.3 | 500 | +0.02em | keycaps and count chips, nothing else |
| `text-meta` | 12px | 1.4 | 400 | 0 | the time or the count beside a row, a table's column names |
| `text-ui` | 13px | 1.45 | 400 | 0 | every control, list row, menu item and header |
| `text-read` | 15px | 1.62 | 400 | -0.003em | everything a person reads a sentence of |
| `text-lead` | 17px | 1.55 | 400 | -0.008em | the opening paragraph of a day's page |
| `text-doc` | 16px | 1.35 | 560 | -0.008em | a heading inside a document |
| `text-title` | 22px | 1.25 | 560 | -0.016em | a document's own title |
| `text-figure` | 26px | 1.15 | 500 | -0.02em | the three numbers at the head of the ledger |

Weights are 400 to read, 500 to pick out, 560 to announce. There is no bold body text.

**The measure is 68 characters at 15px on 1.62 in a pane narrower than 1500px, and 72 characters at 16px on 1.6 in one wider than that.** One step, and only one: a window twice as wide gets the same step as a window a pixel over the line, because the point of a measure is that it stops. Interface text does not change size with the window at all — a control that is hard to read at 1440 is hard to read at 900, and the fix is the control, not the viewport.

The width that decides this is the **pane**, not the window: collapsing the sidebar widens the pane without changing the window, and the layout should follow what the text actually has to live in. `useWide` in `parts.tsx` measures the screen's own root element and re-measures on every resize.

## The wide layout

Past 1500px of pane, a page that is a document grows a **rail of 280px** beside it, and the leftover space falls evenly on both sides. The rail carries what was sitting inside the document or was missing from it, never a second copy of what is already being read:

| Screen | What moves into the rail |
|---|---|
| Meetings | when it ran and how long, who was there, what it left you to do, and an outline of the minutes' own headings |
| Days | the daemon's line about the day, the morning brief, the evening close, and an outline of the page's parts |
| Chats | what the reply being read called and what it read, so the thread itself stays prose and nothing folds |
| Tasks, Settings | nothing — one column, widened to the same 72 characters |

The **outline** marks the section being read and scrolls to one when it is clicked. "Being read" is the last heading whose top has passed a line a fifth of the way down the scrolling region (`useReading`), which is also what picks the reply whose sources the Chats rail is showing.

Four rules keep the rail honest.

1. Anything the rail says is taken *out* of the document — the line under a meeting's title, the tool names under a reply — so nothing is printed twice.
2. A rail with nothing to put in it takes no column. A thread whose replies read nothing and called nothing, a day the daemon wrote no line, brief or close about: the rail is not drawn, and the single measure is centred in the pane as if the wide layout had no rail at all. An empty 280px column beside the words is worse than no column, because it pushes them off centre for nothing.
3. The composer spans exactly the edges of the thread above it. Both ask `sourcedTurns` the same question before either is drawn (`ChatsScreen` decides once and hands the answer to both), so there is no case where the thread drops the rail and the composer still reserves its width — which is what put the box 200px out of line with the words.

4. The header is laid out by the same `Reading` as the page under it — one box, not a width of its own — so the screen's name, its picker and the brain control start where the first word of the document does, rail reserved when there is one. At 1920 on Chats that is 702–1466 with no rail and 534–1300 with one; the header, the document and the composer all read the same pair of numbers.

A thread starts at the top of the pane under the header and grows down, the way every other chat window works. Two turns in a 900px pane sit at the top with the space below them. A thread too long to fit **opens at its newest turn**, and keeps following it while the reader is within 120px of the end (`atBottom`); a reader who has scrolled up to read something is left where they are when the next answer lands. The composer stays fixed at the foot.

Below 1500px none of this exists: one column, 68 characters, everything stacked inside it.

## Spacing

A 4px base, and only these steps: 4, 8, 12, 16, 20, 24, 32, 40, 48, 64.

- Inside a control: 8px across, 4–6px down.
- Between rows of a list: nothing. A row is 30–34px tall and is told from its neighbour by its own hover shape.
- Between a heading and the text under it: 8px. Above that heading: 24px — three times below, which sits inside the 1.25–2 line-heights the rhythm rule asks for.
- Between two sections of a page: 40px.
- A page starts 32px down and ends 96px above the bottom edge, so the last line is never against the frame.

## Radii

4px on a keycap or chip, 6px on a list row, 8px on a button, input or menu item, 12px on a card, popover or settings group, 16px on the composer and a dialog, and a full pill on the search field.

## Colour

One accent, indigo, and it appears at most twice on a screen: the focus ring, and one action. It never fills a list row.

| | Light | Dark |
|---|---|---|
| canvas | `#ffffff` | `#1b1c20` |
| rail | `#f7f7f9` | `#141518` |
| card | `#ffffff` | `#26282d` |
| popover, settings group | `#ffffff` | `#2d3036` |
| sunken (a quote, a code block) | `rgba(0,0,0,.035)` | `rgba(255,255,255,.05)` |
| text | `#17181c` | `#e8e9ec` |
| muted text | `#61646d` | `#979ba4` |
| hairline | `rgba(0,0,0,.075)` | `rgba(255,255,255,.09)` |
| hover | `rgba(0,0,0,.045)` | `rgba(255,255,255,.055)` |
| selected | `rgba(0,0,0,.075)` | `rgba(255,255,255,.14)` |
| accent | `#5a45ea` | `#9d90ff` |
| work in flight | `#996009` | `#e0b341` |
| something wrong | `#c3372b` | `#f08279` |

Muted text is `#61646d` on white (6.3:1) and `#979ba4` on `#1b1c20` (6.1:1), so the grey line under a row is still readable rather than decorative. Dark is built by lightness: the rail is darker than the canvas, a card lighter again, and a popover lighter still — four steps rather than two, which is what says a menu floats clear of the card it sits over rather than merely clear of the canvas behind both.

### Measured contrast

Every text colour against every surface it sits on, in both themes — WCAG 2.1 AA needs 4.5:1 for body text and 3:1 for text at 24px or above (only `text-figure` qualifies; everything else here is checked at the stricter 4.5:1). Computed from the hex values above with the standard relative-luminance formula; the script is not kept in the repo, only the numbers it produced. Dark used to have one "raised" surface for both a card and a popover; now that a popover sits lighter than a card, each gets its own column, and both are checked separately since a popover is where the tightest pair below actually lands.

| Text | Canvas | Rail | Card | Popover |
|---|---|---|---|---|
| text (light) | 17.74:1 | 16.58:1 | 17.74:1 | 17.74:1 |
| text (dark) | 14.02:1 | 15.04:1 | 12.15:1 | 10.90:1 |
| muted text (light) | 5.91:1 | 5.53:1 | 5.91:1 | 5.91:1 |
| muted text (dark) | 6.11:1 | 6.56:1 | 5.30:1 | 4.75:1 |
| work in flight (light) | 5.21:1 | 4.87:1 | 5.21:1 | 5.21:1 |
| work in flight (dark) | 8.67:1 | 9.30:1 | 7.51:1 | 6.74:1 |
| something wrong (light) | 5.38:1 | 5.03:1 | 5.38:1 | 5.38:1 |
| something wrong (dark) | 6.63:1 | 7.11:1 | 5.74:1 | 5.15:1 |
| accent, as text (light) | 6.04:1 | 5.65:1 | 6.04:1 | 6.04:1 |
| accent, as text (dark) | 6.38:1 | 6.84:1 | 5.53:1 | 4.96:1 |

One pair failed on first measurement: light-mode `work in flight` against the rail's `#f7f7f9` read 4.40:1, under the 4.5:1 floor — the "still writing" line and a tool step's status text are both set at `text-meta`/`text-work`, ordinary body-sized text with no exemption. `--work` in light moved from `#a3660a` to `#996009`, the same amber darkened until it cleared 4.5:1 (4.87:1) against the lightest surface it could sit on; every other pair in the table above already cleared AA without a change. `primary-foreground` on the accent fill — white-on-indigo, the one place the accent is a background rather than a ring or a line, e.g. the Send button — is 6.04:1 in light and 6.69:1 in dark, both comfortably past 4.5:1. Widening the dark ramp (canvas `#191a1d`→`#1b1c20`, a card at `#26282d`, a popover lifted clear of the card at `#2d3036`) moved every dark pair against a raised surface down a little, since the raised surface itself moved up: muted text on a popover is now the tightest pair in the table at 4.75:1, still 0.25 clear of the 4.5:1 floor, and accent-as-text on a popover is next-tightest at 4.96:1.

## Shadows

Low and soft, and only on something that actually floats.

- `sm`: `0 1px 2px rgba(0,0,0,.06)` — a card at rest.
- `md`: `0 4px 12px -2px rgba(0,0,0,.10)` plus a hairline ring — a popover or a menu.
- `lg`: `0 16px 40px -12px rgba(0,0,0,.20)` plus a hairline ring — a dialog and the composer.

In dark the alphas double, because a shadow on a dark ground has less to darken.

## The rules

1. **One reading size per pane width.** Prose is 15/1.62 at 68ch, or 16/1.6 at 72ch once the pane passes 1500px. Those are the only two, on every screen.
2. **One interface size.** Controls, rows and headers are 13px at every width. There are two sizes below it, both for metadata, and neither is ever a heading.
3. **No small grey capitals.** A section heading is the same colour as the text beside it and is told apart by weight and by the space above it. Uppercase tracking is for a keycap.
4. **Space above a heading is three times the space below it.** The heading belongs to what follows it.
5. **A list is a list.** Real `<ul>`/`<li>` with a hanging indent and the marker in the margin — a bullet is never a character typed into the front of a paragraph. A top-level bullet longer than 300 characters is not a list item at all: the daemon writes whole briefings into one, the real ones run to 1,300 characters, so past that it is drawn as a paragraph with ordinary paragraph spacing. An indented bullet is exempt however long it runs, because it belongs to the label above it and one long item must not break that list in two. A bullet with items indented under it — the daemon's “You now owe” — is the label of that list rather than the first item of it: it is drawn with no marker in medium weight, and its items follow indented under it.
6. **A lead phrase is set apart, not left running into the sentence.** The daemon opens some lines with a bold phrase and a dash — “You said”, “Said to you”, a person's name. That phrase is medium weight in the text colour; the sentence after it is normal. A dash in the middle of a sentence is punctuation and is left alone.
7. **Nothing is printed twice.** The minutes open with the title as a heading and then repeat it as a bold line carrying the date, and the page prints both above the document already, so any opening line starting with the title is dropped. The same rule governs the rail: whatever it says comes out of the document.
8. **Whitespace separates; a rule is drawn only where two kinds of thing meet** — under a page header, between rows of a table, between the rows inside one settings group.
9. **Hover is a neutral wash, selection is a stronger neutral wash plus weight.** The accent never fills a row, because then every list looks like a form.
10. **The accent appears at most twice on a screen:** the focus ring, and one action.
11. **Every string that can clip carries a `title`,** so the truncated thing can be read without opening it.
12. **An empty state is three parts:** one line in the text colour saying what is not there, one muted sentence saying what to do, and where there is something to do, a button.
13. **A page never scrolls sideways.** A table that is too wide scrolls inside its own box.
14. **Dark mode:** elevation by lightness, text at `#e8e9ec` rather than white, accent desaturated.
15. **Movement is decoration.** A desktop set to want less of it gets none: every transition and animation is cut, because each one sits on top of a state that has already changed.
16. **Keys:** Escape gives up whatever is half-done, arrows walk the list on screen, Enter sends, Shift+Enter starts a line, Ctrl+K opens the palette over chats and pages.
17. **The dot grid means Ora is listening or working, and nothing else.** It is the braille bar the terminal client draws for audio (`src/waveform.ts`): four rows for live voice in the hover, one muted row while an answer is on its way in either window (`workingRow`), and nowhere else — not a header, not an empty state, not a loading skeleton. No spinner and no amber "working" words stand in for it. It may breathe a little while there is nothing to measure; it never runs while nothing is happening.
18. **A notice is one card with its answers on it.** It is the hover's own surface (no green, no speech-bubble tail), "Ora" in the meta size over the title, up to three lines of body, then Done, the three snoozes and Open on one row. Shown on its own it sits flush right under the top bar (`noticePlacement`), beside the tray where Ora's own indicator is, which is where the user asked for it; over an open hover it stacks above the card. While a window is up the daemon posts no desktop banner beside it, and the confirmation after a press is one quiet line in the same card.

## Two things worth knowing about the components

The shadcn components vendored in `src/components/ui` write their state variants as `data-checked`, `data-unchecked` and `data-open`. Radix sets `data-state="checked"` and `data-state="open"` instead, so none of those rules ever matched: the switch never turned its track on, and the menu, dialog and tooltip open animations never ran. `index.css` registers all three names as custom variants that also match the `data-state` spelling, which fixes every component at once rather than editing each of them.

`?mock=1` runs the whole window against the fake daemon in `src/next/mock.ts` — the same fixtures the tests use — so any page can be opened in a plain browser tab with nothing running behind it. `?place=` and `?theme=` are read on a mock page only, which is how the screenshots of every screen in both themes are taken without a click.
