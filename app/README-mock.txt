Ora desktop surface - first mock
================================

This is a Tauri v2 app (vanilla TypeScript, no UI framework) that draws the Ora
popup. It talks to nothing. Every word on the screen comes from src/mock.ts.


How to run
----------

  cd app
  pnpm install
  pnpm tauri dev

The window starts hidden. Two ways to bring it up:

  - the tray icon: click it, then "Show / Hide"
  - the global shortcut Ctrl+Shift+Space

To have it visible the moment it starts, set the env var:

  VITE_ORA_SHOW=1 pnpm tauri dev

Production build:

  pnpm tauri build

Note on the global shortcut: on GNOME Wayland the grab goes through XWayland.
On this machine Ctrl+Shift+Space was already taken by another client, so the
registration fails and the app prints

  ora: global shortcut ctrl+shift+space unavailable: HotKey already registered

and carries on. The failure is not fatal by design - the tray toggle is the
path that always works. To pick a different combination, change the accelerator
string in src-tauri/src/lib.rs.

Note on window placement: Wayland does not let a client place its own window,
so place_near_top() in src-tauri/src/lib.rs is a no-op there and the compositor
decides where the popup lands. It works on X11. Verified size on Wayland: the
window comes up 460x640, visible, undecorated, always on top.


What is mocked
--------------

Everything. src/mock.ts is the only source of data:

  - the two opening transcript lines
  - the morning brief sentence and the three action items
  - the last meeting: title, time, two bullets
  - delegate(): returns "Checking." now and the answer 2700 ms later, tagged
    "query_memory - 77 ms"

Also fake, in the page itself:

  - the level meter next to the state pill is a CSS animation, not audio
  - the state pill only changes because the mock timers say so
  - the mute button toggles a class; no microphone is opened
  - "Open minutes" does nothing
  - the action item checkboxes are not stored anywhere


What the real version replaces
------------------------------

1. src/mock.ts becomes a WebSocket client to the Go daemon. Same shapes:
   a stream of transcript lines with an optional tool tag, a Today payload,
   a last-meeting payload. The 2700 ms delegation timer becomes the real
   round trip, and the tool tag carries the real tool name and duration.

2. The shortcut and tray wiring in src-tauri/src/lib.rs. The accelerator
   should be a setting rather than a constant, the registration failure should
   surface in the UI instead of on stderr, and on Wayland it likely has to go
   through the XDG GlobalShortcuts portal instead of an X11 grab.

3. The daemon ships inside the bundle as a Tauri external binary
   (bundle.externalBin in src-tauri/tauri.conf.json, the Go binary named with
   the target triple suffix), started by the app with the shell plugin's
   sidecar API instead of being run by hand.


Layout
------

  index.html            structure only, no content
  src/main.ts           renders the mock data, runs the delegation timers
  src/mock.ts           all data (this is the module that gets replaced)
  src/styles.css        every style; light and dark via prefers-color-scheme
  src-tauri/src/lib.rs  window placement, tray menu, global shortcut
  src-tauri/tauri.conf.json  460x640, frameless, always on top, hidden at start
