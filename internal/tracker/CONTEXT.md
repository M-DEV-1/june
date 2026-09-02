# tracker — context

## status

- **windows**: shipped. `tracker_windows.go` (Win32 active window) +
  `capture_windows.go` + `uia.ps1` (UI Automation text capture). rich
  text from the focused element + descendant tree, deduped, capped at
  50KB. embedded into the binary via `//go:embed`.
- **linux**: tracker shipped, capture shipped, screenshots shipped, tiered
  capture shipped.
  - `tracker_linux.go` — sway / hyprland / X11 / fallback.
  - `capture_linux.go` — `extractText()` walks the AT-SPI tree (godbus) for
    the focused window's text. parity intent with `uia.ps1`. returns "" when
    accessibility is unavailable (GTK3 may need `toolkit-accessibility=true`;
    GTK4 / Qt with `QT_ACCESSIBILITY=1` expose by default).
    Also `extractMeetingWindow()` — same walk, selects a call's window by name
    instead of by STATE_ACTIVE.
  - `screenshot_linux.go` — `grabScreen()`, real pixels, plus `screenLayout()`
    for multi-monitor splitting. See "pixels" below.
  - `daemon.go` — `tieredCapture`: accessibility text first (free), vision
    (screenshot → LLM) only when that text is thin.
- **macos**: not planned (no dev access).

## linux — as-built

### active-window detection

`New()` runs the strategy probe once, caches the chosen function. Order:

1. **sway**: `$SWAYSOCK` set AND the unix socket reachable → sway IPC.
   - frame: magic `i3-ipc` + length u32LE + type u32LE + payload.
   - `GET_TREE` (type 4), walk the JSON for `focused: true`.
   - `app_id` (native wayland) or `window_properties.class` (xwayland).
2. **hyprland**: `$HYPRLAND_INSTANCE_SIGNATURE` set.
   - socket: `/tmp/hypr/$SIG/.socket.sock` or
     `$XDG_RUNTIME_DIR/hypr/$SIG/.socket.sock` (newer builds).
   - send literal `j/activewindow`, read all bytes, parse JSON.
   - fields: `class`, `title`.
3. **X11**: `$XDG_SESSION_TYPE=x11` OR `$DISPLAY` set AND xgb connects.
   - `_NET_ACTIVE_WINDOW` root property → window handle.
   - title: `_NET_WM_NAME` (UTF-8) preferred, fall back to `WM_NAME`.
   - app: `WM_CLASS` second null-separated string (the class hint).
4. **fallback**: returns `Activity{App: "Unknown", Title: "Unknown"}`, no error.

All four fail soft: transient socket / X11 errors return `Unknown / Unknown`
rather than bubbling up. This keeps the daemon's poll loop steady.

### what works today on linux

- builds cleanly: `GOOS=linux go build ./internal/tracker/...`
- vets cleanly: `GOOS=linux go vet ./internal/tracker/...`
- sway / hyprland / X11 paths return real `(app, title)` pairs.
- compiler treats title-only activity the same as Windows activity —
  Gemini summarizes from titles alone (lower signal but it works).
- notes table, FTS5, /note all unaffected by capture gap.

### what doesn't work yet on linux

- **the focused window is not the only window read**. `extractMeetingWindow()`
  (capture_linux.go) walks the same AT-SPI registry but selects by name rather
  than by STATE_ACTIVE, and `Daemon.watchMeetingWindow` calls it once a minute
  on its own goroutine — off the 2-second poll, because one walk is allowed
  2.5s and would otherwise stall window tracking past its own interval.
  Why it exists: on 2026-08-31 a 39-minute meeting produced 27 episodes and not
  one was the call. The user spent it in ClickUp and a terminal, so the
  participant tiles and the window title — the only things on the machine that
  name who is speaking — were never captured. A Teams PWA titles itself
  "Microsoft Teams (PWA) - Chat | <person>", so the title alone carries the
  other participant's name.
  Dedup is on the window's own text and `lastText` only advances after a
  successful channel send; recording it before the send would let one full
  channel retire a meeting's window for the rest of the call.

- **text capture depends on app accessibility**. extractText returns "" when the
  focused app exposes no AT-SPI text (a11y disabled, or a toolkit that needs a flag).
  This is what the vision tier exists to cover — see "pixels" below.
- **pixel capture is a no-op on WINDOWS, not linux**. `capture_windows.go`'s
  `grabScreen()` returns an error; linux has the real implementation. An earlier
  version of this file said the opposite, naming a `captureScreen()` that no
  longer exists — it was written before the screenshot work landed.
- **GNOME / KDE wayland** without sway/hyprland: falls back to X11 if
  xwayland is active, otherwise → `Unknown / Unknown`. compositor IPC
  for mutter / kwin is per-DE, fragile. ship as a known limitation.
- **GNOME extension (`org.gnome.Shell.Extensions.Windows`)** is the only
  reliable GNOME path. Requires installing the extension. Out of scope.

## as-built — linux AT-SPI text capture

`capture_linux.go::extractText()` returns the focused app's accessibility
text — parity intent with `uia.ps1` on windows. Implemented pure-go via
godbus (active-poll walk):

1. session bus → `org.a11y.Bus.GetAddress` → dial the a11y bus, Auth, Hello.
2. root `org.a11y.atspi.Registry` `/org/a11y/atspi/accessible/root` →
   `GetChildren` → app accessibles.
3. per app: find the window with `STATE_ACTIVE`; DFS its subtree (depth ≤ 6,
   ≤ 400 nodes); for nodes implementing `org.a11y.atspi.Text` call
   `GetText(0,-1)`. dedupe lines, cap 50KB.
4. hard ~750ms timeout; errors on any node are skipped; top-level failure
   returns `("", nil)` — never fails the tracker tick.

verified extracting ~10KB from a live focused window. yields "" when the
focused app exposes no a11y text.

## as-built — linux pixels (`screenshot_linux.go`)

652 frames on disk as of 2026-09-01, from 2026-08-18 onward. Stored as
`frames/{episode-id}.jpg` (and `-b`, `-c` for extra monitors), path in
`episodes.image_path`.

Two paths, tried in order:

1. **gnome-shell direct** (`screenshotShell`). Claims the well-known bus name
   `org.gnome.Screenshot` and calls `org.gnome.Shell.Screenshot.Screenshot`
   with `flash=false`. gnome-shell only accepts this call from a short list of
   bus names — settings-daemon media keys, the GNOME/GTK portal backends, and
   `org.gnome.Screenshot`, which belongs to the standalone `gnome-screenshot`
   tool that a default GNOME 46 desktop does not install. So we can claim it.
   **No flash, no shutter animation, no consent dialog.**
2. **xdg-desktop-portal fallback.** Used on any non-GNOME desktop or when
   `org.gnome.Screenshot` is already owned. The portal calls the same shell
   method with `flash=true`, which is where the full-screen white flash on
   every capture comes from — the reason path 1 exists at all. The user's
   stored allow/deny decision lives in the portal `PermissionStore`, under the
   empty app-id because we run unsandboxed.

`screenLayout()` returns the monitor rectangles and the pointer position, so
one whole-canvas grab is split per monitor with the monitor the user is on
first. `Activity.ImageJPEG` is that one; `ExtraJPEG` holds the rest. A meeting
on one screen while notes sit on the other is one activity, not two.

## as-built — tiered capture (`daemon.go::tieredCapture`)

Accessibility text is free; a screenshot plus a vision call is not. So:

1. `extractText()` first, every capture.
2. Vision fires only when that text is **below `thinTextThreshold` (200 runes)**
   — accessibility is treated as blind — and no more often than
   `minVisionInterval` (90s), and only for a real foreground app
   (`isVisionWorthy`), never the bare desktop.
3. `lastA11yText` and `lastVisionText` are tracked **separately**, so a tier
   switch on an unchanged screen does not compare one tier's text against the
   other's and read as a change.

Vision is injected via `Daemon.SetVisionFn`; nil disables the tier and the
tracker is text-only. `Sight` carries `UserActivity` plus `VisibleText`.

A future windows polish may also move `uia.ps1` to pure-go go-ole COM
(same pattern as the WASAPI mic in `internal/audio/capture_windows.go`)
to drop the ~200-400ms PowerShell cold start per capture. Separate
project, not blocking on this AT-SPI work.

### dbus shape

AT-SPI runs over the session bus. Service: `org.a11y.atspi.Registry`.
Connect via godbus (already in deps).

```
bus, _ := dbus.SessionBus()
registry := bus.Object("org.a11y.atspi.Registry", "/org/a11y/atspi/registry")

# get the address of the AT-SPI bus (it's actually a separate bus)
var addr string
registry.Call("org.a11y.Bus.GetAddress", 0).Store(&addr)
atspi, _ := dbus.Dial(addr)
atspi.Auth(nil)
atspi.Hello()
```

### finding the focused element

Two viable paths:

- **EventListener**: subscribe to `object:state-changed:focused` events on
  the registry. Listener goroutine maintains a `currentFocusedElement`
  pointer. `extractText()` reads it instantly. Lowest latency, but
  bookkeeping (cleanup on app exit, missed events on session restart).
- **Active-poll walk**: at each `extractText` call, enumerate apps via
  registry → for each app's root accessible, walk to find element with
  `STATE_FOCUSED`. Simpler but O(N apps × tree size) each capture.

Recommend EventListener for the v2 daemon.

### extracting text

Once an `AccessibleObject` proxy is held:

- check if it implements `org.a11y.atspi.Text` → call `GetText(0, -1)`.
- check `org.a11y.atspi.Value` → `Value` property (input fields).
- recurse via `GetChildAtIndex` for descendants (cap depth at ~6).

Mirror the UIA walk in `uia.ps1` for behavioural parity.

### testing

AT-SPI behaviour varies wildly by app:

- **GTK**: text exposed by default.
- **Qt**: text exposed if `QT_ACCESSIBILITY=1`.
- **Electron**: needs `--force-renderer-accessibility` flag at launch.
- **Java Swing**: needs `assistive_technologies=org.GNOME.Accessibility.AtkWrapper`
  in `accessibility.properties`.

Test matrix: Firefox, VSCode, GNOME Terminal, Discord/Slack (Electron).
If two or more return empty text, build a fallback hint (return last
known title only).

## file layout — as-built

```
tracker.go              — Activity, Tracker iface, Normalize fn
tracker_windows.go      — //go:build windows
tracker_linux.go        — //go:build linux
tracker_stub.go         — //go:build !windows && !linux
capture_windows.go      — //go:build windows ; UIA path
capture_linux.go        — //go:build linux   ; AT-SPI text + meeting window
capture_stub.go         — //go:build !windows && !linux
screenshot_linux.go     — //go:build linux   ; gnome-shell + portal pixels
screenshot_other.go     — //go:build !linux
jpeg.go                 — frame encoding / monitor splitting
document_text.go        — role-aware text pick (browser chrome vs page)
idle_linux.go           — Mutter IdleMonitor, input-idle probe
lock_linux.go           — GNOME screensaver lock probe
mpris_linux.go          — MPRIS media-playing probe
uia.ps1                 — embedded UIA walker for windows
daemon.go               — dwell state machine, blocklist, tiered capture,
                          meeting-window watcher. OS-agnostic.
```

`tracker_test.go` is build-tagged `windows`. linux tests live in
`tracker_linux_test.go` (build-tag `linux`) and skip when `$DISPLAY`,
`$SWAYSOCK`, `$HYPRLAND_INSTANCE_SIGNATURE` are all unset (CI safety).

## linux dev checklist (moving to a linux box)

Order matters. Tracker-only first, then the bigger blockers.

1. `git clone` repo, `cd voice-agent`.
2. install go 1.25.1+. confirm with `go version`.
3. install system deps for everything ORA pulls in:
   ```
   # debian/ubuntu
   sudo apt install \
     libgtk-3-dev libayatana-appindicator3-dev \  # systray
     libasound2-dev libpulse-dev \                # audio
     libx11-dev libxext-dev libxrandr-dev \       # x11
     dbus-x11 at-spi2-core                        # accessibility (future)
   ```
4. `go build ./internal/tracker/...` — this should succeed standalone.
5. `go test ./internal/tracker/...` — runs the linux test file. Will
   skip the live-window test if no `$DISPLAY` etc.
6. manual smoke: write a tiny `cmd/tracker-smoke/main.go` that calls
   `tracker.New().GetActiveWindow()` in a loop. Open a couple of apps,
   alt-tab, watch the output. Confirm sway/hyprland/X11 path matches
   your session.
7. Once tracker is happy, tackle the cross-package blockers below to
   build the full binary.

## OUTSIDE-TRACKER — linux build status

These are NOT tracker concerns but they gate running `ora` end-to-end
on linux. The full binary now builds and runs pure-Go on linux
(`go build -o ora .`, `GOOS=linux go build ./...`). History kept here
because the user asked "is moving to linux enough?".

### audio mic — shipped (jfreymuth/pulse native protocol, pure go)

- `internal/audio/capture_windows.go` — windows-only (WASAPI via
  moutend/go-wca).
- `internal/audio/capture_linux.go` — `github.com/jfreymuth/pulse`, the
  PulseAudio native protocol over the `/run/user/<uid>/pulse/native`
  socket (PipeWire ships a pulse-compatible socket, so one lib covers
  both). 24kHz mono s16le via an `Int16Writer` push callback → micChan.
  No CGO, no runtime CLI tools. Verified capturing real audio.
- **GOTCHA**: `RecordLatency(0.05)` (or `RecordBufferFragmentSize`) is
  REQUIRED — PipeWire's pulse server delivers zero data to a record
  stream with unset buffer attributes (stream reports running, callback
  never fires). Cost ~2h to diagnose.
- `internal/audio/audio_stub.go` — `!windows && !linux`: `NewMic` /
  `NewSpeaker` return "not supported".
- earlier shell-out (pw-record/pw-cat) and PulseAudio-TCP / raw-ALSA
  ideas were dropped in favour of the native-protocol lib: no runtime
  tool dependency, real flush/latency control, still zero CGO.

### speaker — shipped (jfreymuth/pulse on linux, oto on windows)

- `player_windows.go` (//go:build windows) — oto v3, unchanged.
- `player_linux.go` (//go:build linux) — `jfreymuth/pulse` playback, a
  pull-model `Int16Reader` that drains a chunks channel and fills silence
  when idle (keeps the stream alive, mirrors the windows audioStreamer).
  Flush (barge-in) clears the buffer so the next pull returns silence —
  no process to kill. `PlaybackLatency(0.05)`. No CGO.
- oto deliberately NOT used on linux: its `driver_unix.go` carries
  `// #cgo pkg-config: alsa`, so linux needs CGO + `libasound2-dev` to
  build and `libasound2` at runtime, and CGO breaks cross-compile from
  the windows dev box. (oto IS cgo-free on windows/macOS only.)

### system tray — shipped (pure-go SNI over D-Bus on linux)

- `getlantern/systray` needs CGO + GTK + appindicator → gated behind
  `//go:build windows` in `cmd/tray_windows.go`.
- `cmd/tray_linux.go` (//go:build linux) — hand-rolled StatusNotifierItem
  + `com.canonical.dbusmenu` over godbus (same protocol Docker / LM Studio
  / NetBird use). Registers with `org.kde.StatusNotifierWatcher`, exports
  `/StatusNotifierItem` props + `/MenuBar` with a "Quit Ora" item.
  Icon is the ORA logo: `cmd/tray_icon_linux.png` (128×128, downscaled
  from `docs/images/tray_icon.png`) is `//go:embed`-ed and decoded to an
  ARGB32 pixmap (`trayIconPixmaps`, TDD-covered). Falls back to headless
  if SNI registration fails — never crashes the daemon.
- `cmd/tray_other.go` (//go:build !windows && !linux) — headless fallback
  for other unix. daemon service setup shared in
  `cmd/daemon.go::startDaemonServices`.
- verified: item registers with the watcher and the logo icon renders on
  GNOME (shell hosts the watcher).

### daemon spawn

- `cmd/fork_windows.go` hides the spawned daemon via Win32 syscall.
- `cmd/fork_unix.go` is a stub. on linux the daemon process is just
  detached. fine for terminal use. no harm.

## why this matters

linux moving day = DONE, and then some. tracker works, capture pulls
focused-window text via AT-SPI, voice works (pure-go jfreymuth/pulse
mic+speaker over the native protocol), and there's a real SNI tray icon
(ORA logo) with a Quit menu — no headless compromise. memory model +
FTS5 + notes are pure-Go and OS-agnostic. full binary builds and runs on
linux with ZERO CGO and zero runtime CLI-tool dependencies.
