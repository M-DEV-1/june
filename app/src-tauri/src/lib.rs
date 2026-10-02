mod overlay;

use std::path::PathBuf;
use tauri::{AppHandle, Emitter, Manager};

/// The WM_CLASS instance name the overlay window carries. It has to be the StartupWMClass of the hidden june-overlay.desktop entry the daemon writes (overlayDesktopEntry in cmd/desktop_entry_linux.go), because that match is what files the overlay under an application of its own instead of under June's dock entry.
#[cfg(target_os = "linux")]
const OVERLAY_WM_CLASS_INSTANCE: &str = "june-overlay";

/// Same resolution order as Go's config.DataDir(): JUNE_DATA_DIR env var, else %LOCALAPPDATA%\june on Windows, else XDG_DATA_HOME/june, else ~/.local/share/june. Input: none. Output: the data directory path, or None if neither override nor home directory is available.
pub(crate) fn data_dir() -> Option<PathBuf> {
    if let Ok(dir) = std::env::var("JUNE_DATA_DIR") {
        if !dir.is_empty() {
            return Some(PathBuf::from(dir));
        }
    }
    #[cfg(windows)]
    return std::env::var_os("LOCALAPPDATA").map(|dir| PathBuf::from(dir).join("june"));
    #[cfg(not(windows))]
    {
        if let Ok(dir) = std::env::var("XDG_DATA_HOME") {
            if !dir.is_empty() {
                return Some(PathBuf::from(dir).join("june"));
            }
        }
        std::env::var("HOME").ok().map(|home| {
            PathBuf::from(home)
                .join(".local")
                .join("share")
                .join("june")
        })
    }
}

/// Tauri command: reads the daemon's IPC token file so the window can authenticate its requests. Input: none. Output: the trimmed token string, or an error string if the data dir or file can't be found.
#[tauri::command]
fn ipc_token() -> Result<String, String> {
    let dir = data_dir().ok_or("could not determine june data directory")?;
    let contents = std::fs::read_to_string(dir.join("ipc-token")).map_err(|e| e.to_string())?;
    Ok(contents.trim().to_string())
}

/// Where the desktop's dock sits and how much room the hover has to leave for it. `edge` is "bottom", "left", "right" or "top"; `clearance` is in logical pixels and is 0 when the dock reserves screen space, because a dock that reserves space is already cut out of the monitor's work area and the hover only has to keep clear of the work area's edge.
#[derive(serde::Serialize)]
struct DockAnchor {
    edge: String,
    clearance: f64,
}

/// Reads one gsettings key. Input: the schema and key names. Output: the value with gsettings' quoting stripped, or None when gsettings is missing, the schema is not installed, or the key does not exist.
#[cfg(target_os = "linux")]
fn gsetting(schema: &str, key: &str) -> Option<String> {
    let out = std::process::Command::new("gsettings")
        .args(["get", schema, key])
        .output()
        .ok()?;
    if !out.status.success() {
        return None;
    }
    Some(
        String::from_utf8_lossy(&out.stdout)
            .trim()
            .trim_matches('\'')
            .to_string(),
    )
}

/// How long a dock reading is reused before gsettings is asked again. Reading the dock takes two schemas and three keys, which is up to six forked processes, and it runs at the front of every show and every notice; a dock does not move between one keypress and the next, so the answer is held for this long.
const DOCK_CACHE_FOR: std::time::Duration = std::time::Duration::from_secs(30);

/// The last dock reading and when it was taken, or None before the first one.
static DOCK_CACHE: std::sync::Mutex<Option<(std::time::Instant, String, f64)>> =
    std::sync::Mutex::new(None);

/// Tauri command: the dock anchor, read from gsettings at most once every DOCK_CACHE_FOR. Input: none. Output: as read_dock_anchor, from the cache when the last reading is still fresh. A poisoned lock simply reads gsettings again, because a stale-cache miss costs a few forks and nothing else.
#[tauri::command]
fn dock_anchor() -> DockAnchor {
    let now = std::time::Instant::now();
    if let Ok(cache) = DOCK_CACHE.lock() {
        if let Some((at, edge, clearance)) = cache.as_ref() {
            if now.duration_since(*at) < DOCK_CACHE_FOR {
                return DockAnchor {
                    edge: edge.clone(),
                    clearance: *clearance,
                };
            }
        }
    }
    let fresh = read_dock_anchor();
    if let Ok(mut cache) = DOCK_CACHE.lock() {
        *cache = Some((now, fresh.edge.clone(), fresh.clearance));
    }
    fresh
}

/// Works out which edge the dock is on so the hover can be anchored against it instead of floating in the middle of the screen. Off Linux there are no dock settings to read, and the answer is always the fallback below, which on Windows puts the hover along the bottom of the work area, above the taskbar. Input: none; it reads the dash-to-dock and ubuntu-dock GNOME extension settings, which are where a GNOME desktop keeps the dock's position. Output: the edge and the clearance to leave. An auto-hiding dock reserves no screen space, so its own thickness is reported as clearance and estimated from the configured icon size plus the padding dash-to-dock draws around it. When no dock extension answers — a plain GNOME session, a different desktop, or no gsettings at all — this falls back to a bottom edge with no clearance, which puts the hover along the bottom of the work area, clear of whatever panel the desktop did reserve space for.
fn read_dock_anchor() -> DockAnchor {
    #[cfg(target_os = "linux")]
    for schema in [
        "org.gnome.shell.extensions.dash-to-dock",
        "org.gnome.shell.extensions.ubuntu-dock",
    ] {
        let Some(position) = gsetting(schema, "dock-position") else {
            continue;
        };
        let edge = match position.to_ascii_lowercase().as_str() {
            "left" => "left",
            "right" => "right",
            "top" => "top",
            _ => "bottom",
        };
        let fixed = gsetting(schema, "dock-fixed")
            .map(|v| v == "true")
            .unwrap_or(false);
        let icon = gsetting(schema, "dash-max-icon-size")
            .and_then(|v| v.parse::<f64>().ok())
            .unwrap_or(48.0);
        let clearance = if fixed { 0.0 } else { icon + 16.0 };
        return DockAnchor {
            edge: edge.to_string(),
            clearance,
        };
    }
    DockAnchor {
        edge: "bottom".to_string(),
        clearance: 0.0,
    }
}

/// Tauri command: reads GNOME's dark-mode preference so the webview can match the desktop theme, because WebKitGTK's own prefers-color-scheme media query does not follow the desktop's gsettings value. Input: none. Output: on Linux, "dark" or "light", read from `gsettings get org.gnome.desktop.interface color-scheme` when that subprocess succeeds, otherwise from the GTK gtk-application-prefer-dark-theme setting, otherwise "light". Elsewhere an error, which sends the page to its prefers-color-scheme fallback (systemTheme in app/src/shared/theme.ts); WebView2 on Windows answers that query from the Windows app theme.
#[tauri::command]
fn system_theme() -> Result<String, String> {
    #[cfg(not(target_os = "linux"))]
    return Err("the webview's prefers-color-scheme follows this desktop".into());
    #[cfg(target_os = "linux")]
    {
        Ok(linux_theme())
    }
}

/// The Linux half of system_theme. Input: none. Output: "dark" or "light".
#[cfg(target_os = "linux")]
fn linux_theme() -> String {
    if let Ok(output) = std::process::Command::new("gsettings")
        .args(["get", "org.gnome.desktop.interface", "color-scheme"])
        .output()
    {
        if output.status.success() {
            let value = String::from_utf8_lossy(&output.stdout);
            return if value.contains("prefer-dark") {
                "dark"
            } else {
                "light"
            }
            .to_string();
        }
    }
    use gtk::prelude::GtkSettingsExt;
    if let Some(settings) = gtk::Settings::default() {
        return if settings.is_gtk_application_prefer_dark_theme() {
            "dark"
        } else {
            "light"
        }
        .to_string();
    }
    "light".to_string()
}

/// Tells the window manager that the overlay is not asking for the user's attention, by removing the _NET_WM_STATE_DEMANDS_ATTENTION state from it. Input: the GTK window behind the overlay. Output: nothing; there is nothing to do before the window has been realised, or when the session is not on X11.
/// Mapping a window that refuses focus while another window has it is what makes mutter mark that window as demanding attention, and the mark never comes off by itself, because a window stops demanding attention when it is focused and this one never is. The urgency hint in WM_HINTS is a different flag, and clearing that leaves this state set, so the state itself is removed the way the EWMH spec says to: a _NET_WM_STATE message to the root window carrying _NET_WM_STATE_REMOVE, sent once the window is mapped and the window manager has had its say.
/// Everything here goes through Xlib and GDK's ffi rather than the gdkx11 crate's wrappers, because those wrappers assert that the process called gtk::init itself, which a Tauri app does not.
#[cfg(target_os = "linux")]
fn drop_attention_demand(gtk_win: &gtk::ApplicationWindow) {
    use gtk::glib::translate::ToGlibPtr;
    use gtk::glib::Cast;
    use gtk::prelude::WidgetExt;
    use x11::xlib;

    let Some(gdk_win) = gtk_win.window() else {
        return;
    };
    if gdk_win.clone().downcast::<gdkx11::X11Window>().is_err() {
        return;
    }
    let raw: *mut gtk::gdk::ffi::GdkWindow = gdk_win.to_glib_none().0;
    let gdk_display = unsafe { gtk::gdk::ffi::gdk_window_get_display(raw) };
    if gdk_display.is_null() {
        return;
    }
    let xdisplay = unsafe {
        gdkx11::ffi::gdk_x11_display_get_xdisplay(gdk_display as *mut gdkx11::ffi::GdkX11Display)
    };
    if xdisplay.is_null() {
        return;
    }
    let xid = unsafe { gdkx11::ffi::gdk_x11_window_get_xid(raw as *mut gdkx11::ffi::GdkX11Window) };
    let Ok(state) = std::ffi::CString::new("_NET_WM_STATE") else {
        return;
    };
    let Ok(demands) = std::ffi::CString::new("_NET_WM_STATE_DEMANDS_ATTENTION") else {
        return;
    };
    unsafe {
        let root = xlib::XDefaultRootWindow(xdisplay);
        let mut event: xlib::XEvent = std::mem::zeroed();
        let client = &mut event.client_message;
        client.type_ = xlib::ClientMessage;
        client.send_event = xlib::True;
        client.display = xdisplay;
        client.window = xid;
        client.message_type = xlib::XInternAtom(xdisplay, state.as_ptr(), xlib::False);
        client.format = 32;
        // The five words of a _NET_WM_STATE message: remove the state, which state, no second state, and 1 for a message sent by the application that owns the window rather than by a pager.
        client.data = xlib::ClientMessageData::from([
            0,
            xlib::XInternAtom(xdisplay, demands.as_ptr(), xlib::False) as std::os::raw::c_long,
            0,
            1,
            0,
        ]);
        // The message goes to the root window with the substructure masks set, because that is the only place the window manager listens for requests about the windows it manages.
        xlib::XSendEvent(
            xdisplay,
            root,
            xlib::False,
            xlib::SubstructureRedirectMask | xlib::SubstructureNotifyMask,
            &mut event,
        );
        xlib::XFlush(xdisplay);
    }
}

/// Tauri command: brings the main window to the front with real input focus, not just visible. Input: the app handle. Output: nothing; a missing window only means the app is shutting down. On X11 this calls present_with_time with a timestamp fetched fresh from the X server, because GNOME's focus-stealing prevention ignores a present or set_focus call that carries no user-interaction time, which is exactly what happens when the window is shown from a SIGHUP relayed from the desktop's own keybinding rather than from a click inside the window itself. Anywhere else (Wayland, or the GTK/X11 calls unavailable) it falls back to set_focus.
/// This is also the only thing that puts the hover in front, because the hover carries no always-on-top flag: mutter refuses focus to any newly opened window that an always-on-top window overlaps, so with that flag set every app the user opened while the hover was up came up unfocused (window_would_be_covered in mutter's window.c looks at _NET_WM_STATE_ABOVE alone, whatever type the covering window is).
/// The overlay's dock type hint is not the answer here, because the hover is where the user types and mutter never focuses a dock of its own accord: measured against mutter 46.2 on 2026-09-05, a dock window is focused only when it asks (present_with_time still works on one), a click on it moves no focus at all ("Don't focus panels--they must explicitly request focus" in meta_window_handle_ungrabbed_event), and when the window that held the focus closes the focus falls to nothing rather than to the dock, so every key typed after that is dropped.
#[tauri::command]
fn raise(app: AppHandle) {
    let Some(w) = app.get_webview_window("main") else {
        return;
    };
    #[cfg(target_os = "linux")]
    {
        use gtk::glib::Cast;
        use gtk::prelude::{GtkWindowExt, WidgetExt};
        if let Ok(gtk_win) = w.gtk_window() {
            if let Some(gdk_win) = gtk_win.window() {
                if let Ok(x11_win) = gdk_win.downcast::<gdkx11::X11Window>() {
                    let time = gdkx11::functions::x11_get_server_time(&x11_win);
                    gtk_win.present_with_time(time);
                    return;
                }
            }
        }
    }
    let _ = w.set_focus();
}

/// Asks the page to run its show/hide sequence, the same event the hotkey's SIGHUP sends. Showing the window from here instead would skip the page's placement and paint the hover wherever the window manager last left it, so every way of showing the hover deliberately shares one path. Input: the app handle. Output: nothing; a failed emit only means the page has not loaded yet.
fn toggle(app: &AppHandle) {
    let _ = app.emit("june://toggle", ());
}

/// Shows the main app window and puts the focus on it, whether it was hidden or merely behind something. Input: the app handle. Output: nothing; a missing window only means the app is shutting down.
fn open_app(app: &AppHandle) {
    let Some(w) = app.get_webview_window("app") else {
        return;
    };
    let _ = w.show();
    let _ = w.unminimize();
    let _ = w.set_focus();
}

/// Where this process records its pid for the desktop hotkey. Input: none. Output: <data dir>/window.pid, or None when no data directory could be resolved.
#[cfg(unix)]
fn pid_path() -> Option<PathBuf> {
    data_dir().map(|d| d.join("window.pid"))
}

/// Writes this process's pid to <data dir>/window.pid and toggles the window whenever SIGHUP arrives. A Wayland session never delivers a global key grab to a hidden client, so the desktop's own keybinding (GNOME custom shortcut on Ctrl+Alt+Space) runs `kill -HUP $(cat window.pid)` instead. SIGHUP because JavaScriptCore inside the webview owns SIGUSR1 and SIGUSR2 for its own thread signalling, and chaining into its handler segfaults. Input: the app handle. Output: nothing; a pid file that cannot be written is logged and the signal thread still runs.
#[cfg(unix)]
fn listen_for_toggle_signal(app: AppHandle) {
    if let Some(path) = pid_path() {
        // 0600: it is only ever read by the user's own hotkey, and it named this process to anyone with an account on the machine at the 0664 the default write gave it.
        use std::io::Write;
        use std::os::unix::fs::OpenOptionsExt;
        let written = std::fs::OpenOptions::new()
            .write(true)
            .create(true)
            .truncate(true)
            .mode(0o600)
            .open(&path)
            .and_then(|mut f| f.write_all(std::process::id().to_string().as_bytes()));
        if let Err(e) = written {
            eprintln!("june: could not write window.pid: {e}");
        }
    }
    let Ok(mut signals) = signal_hook::iterator::Signals::new([signal_hook::consts::SIGHUP]) else {
        eprintln!("june: could not listen for SIGHUP");
        return;
    };
    std::thread::spawn(move || {
        for _ in signals.forever() {
            // Window calls from this thread crash GTK, so the page does the show/hide: it listens for this event and calls the window API, which marshals to the main loop.
            let _ = app.emit("june://toggle", ());
        }
    });
}

/// Registers Ctrl+Alt+Space as a global shortcut that runs the same toggle the SIGHUP does on Linux. Windows delivers a global hotkey to a hidden window, so it needs no desktop keybinding or pid file. Input: the app handle. Output: nothing; a shortcut another program already holds is logged and the window runs without one.
#[cfg(windows)]
fn register_toggle_hotkey(app: &AppHandle) {
    use tauri_plugin_global_shortcut::{
        Code, GlobalShortcutExt, Modifiers, Shortcut, ShortcutState,
    };
    let hotkey = Shortcut::new(Some(Modifiers::CONTROL | Modifiers::ALT), Code::Space);
    let plugin = tauri_plugin_global_shortcut::Builder::new()
        .with_handler(move |app, shortcut, event| {
            // The handler fires on release as well as on press, and toggling on both would show and hide the hover in one keystroke.
            if shortcut == &hotkey && event.state() == ShortcutState::Pressed {
                toggle(app);
            }
        })
        .build();
    if let Err(e) = app.plugin(plugin) {
        eprintln!("june: could not load the global shortcut plugin: {e}");
        return;
    }
    if let Err(e) = app.global_shortcut().register(hotkey) {
        eprintln!("june: could not register Ctrl+Alt+Space: {e}");
    }
}

/// One monitor as the overlay page needs it: where its top-left corner sits on the desktop and how big it is, both in physical pixels, plus its own scale factor.
#[derive(serde::Serialize)]
struct OverlayMonitor {
    x: i32,
    y: i32,
    w: u32,
    h: u32,
    scale: f64,
}

/// Everything the overlay page needs to turn a rectangle given in global desktop pixels into a position inside its own document: where the overlay window's top-left corner sits on the desktop, how many physical pixels one CSS pixel is worth in that window, and where every monitor is.
#[derive(serde::Serialize)]
struct OverlayLayout {
    origin_x: i32,
    origin_y: i32,
    scale: f64,
    monitors: Vec<OverlayMonitor>,
}

/// The smallest rectangle that covers every monitor. Input: one (x, y, width, height) per monitor, in physical desktop pixels. Output: that rectangle as (x, y, width, height), or None when there are no monitors. The width and height are never zero, because a window of no size cannot be shown.
fn union_bounds(rects: &[(i32, i32, u32, u32)]) -> Option<(i32, i32, u32, u32)> {
    if rects.is_empty() {
        return None;
    }
    let mut left = i32::MAX;
    let mut top = i32::MAX;
    let mut right = i32::MIN;
    let mut bottom = i32::MIN;
    for &(x, y, w, h) in rects {
        left = left.min(x);
        top = top.min(y);
        right = right.max(x.saturating_add(w as i32));
        bottom = bottom.max(y.saturating_add(h as i32));
    }
    Some((
        left,
        top,
        (right - left).max(1) as u32,
        (bottom - top).max(1) as u32,
    ))
}

/// Reads every monitor as a plain (x, y, width, height) in physical pixels. Input: the app handle. Output: one tuple per monitor, empty when the display cannot be enumerated.
fn monitor_rects(app: &AppHandle) -> Vec<(i32, i32, u32, u32)> {
    app.available_monitors()
        .unwrap_or_default()
        .iter()
        .map(|m| {
            (
                m.position().x,
                m.position().y,
                m.size().width,
                m.size().height,
            )
        })
        .collect()
}

/// Tauri command: tells the overlay page where its own window sits on the desktop and where the monitors are, so it can place a rectangle the daemon gave it in global screen pixels. Input: the app handle. Output: the layout. The origin is the overlay window's own outer position, falling back to the corner of the smallest rectangle covering every monitor when the window cannot be asked; a window sitting anywhere other than that corner, or sized differently from that rectangle, is logged, because either one offsets every shape the page draws. When no monitor can be enumerated the origin is 0,0 at scale 1 and the monitor list is empty, which makes the page draw nothing rather than draw in the wrong place.
#[tauri::command]
fn overlay_layout(app: AppHandle) -> OverlayLayout {
    let monitors = app.available_monitors().unwrap_or_default();
    let rects: Vec<(i32, i32, u32, u32)> = monitors
        .iter()
        .map(|m| {
            (
                m.position().x,
                m.position().y,
                m.size().width,
                m.size().height,
            )
        })
        .collect();
    let (union_x, union_y, union_w, union_h) = union_bounds(&rects).unwrap_or((0, 0, 1, 1));
    let window = app.get_webview_window("overlay");
    let scale = window
        .as_ref()
        .and_then(|w| w.scale_factor().ok())
        .unwrap_or(1.0);
    // The page subtracts this origin from every rectangle the daemon sends, so it has to be where the window actually is, not where it was asked to be. Mutter has moved this window before — the 32 pixel top-bar shift arm_overlay's comment records — and a window put anywhere but the union's corner offsets every shape by the difference.
    let placed = window.as_ref().and_then(|w| w.outer_position().ok());
    let sized = window.as_ref().and_then(|w| w.outer_size().ok());
    if let Some(p) = placed {
        if p.x != union_x || p.y != union_y {
            eprintln!("june: overlay window sits at {},{} but the monitors start at {union_x},{union_y}", p.x, p.y);
        }
    }
    if let Some(size) = sized {
        if size.width != union_w || size.height != union_h {
            eprintln!("june: overlay window is {}x{} but the monitors cover {union_w}x{union_h}", size.width, size.height);
        }
    }
    let (origin_x, origin_y) = placed.map_or((union_x, union_y), |p| (p.x, p.y));
    OverlayLayout {
        origin_x,
        origin_y,
        scale,
        monitors: monitors
            .iter()
            .map(|m| OverlayMonitor {
                x: m.position().x,
                y: m.position().y,
                w: m.size().width,
                h: m.size().height,
                scale: m.scale_factor(),
            })
            .collect(),
    }
}

/// Sizes the overlay window to cover every monitor, makes it ignore the mouse, and shows it. Input: the app handle. Output: nothing; every step is best effort, because a window call that fails only means the app is shutting down.
/// Stacks the hover above ordinary windows and keeps it there, without the flag that breaks focus elsewhere.
/// Clicking anything else used to put that window in front of the hover, which read as the hover vanishing: it carried no stacking hint at all. Always-on-top is not the answer — mutter refuses focus to any newly opened window that an `_NET_WM_STATE_ABOVE` window would cover (window_would_be_covered in window.c reads that state alone), which is what made every app open unfocused. The dock type hint stacks a window over ordinary ones without setting that state, which is why the drawing overlay already uses it (see arm_overlay).
/// Unlike the overlay this one must still take the keyboard, so accept_focus is left alone. Mutter never focuses a dock of its own accord and a click on one moves no focus, so the page calls `raise` on every press inside the hover, and the hotkey path already did; present_with_time works on a dock.
/// The type hint has to be set while the window is unmapped, which it is: the hover is created hidden and the page shows it.
fn arm_hover(app: &AppHandle) {
    let Some(w) = app.get_webview_window("main") else {
        return;
    };
    #[cfg(target_os = "linux")]
    if let Ok(gtk_win) = w.gtk_window() {
        use gtk::prelude::GtkWindowExt;
        gtk_win.set_type_hint(gtk::gdk::WindowTypeHint::Dock);
    }
    // A question asked, then a switch to another window to check on something, then a switch back: the hover is on whichever workspace the user is on rather than the one it opened on.
    let _ = w.set_visible_on_all_workspaces(true);
    // Windows has no dock type hint, so topmost is what keeps the hover above the window the user clicks next. Mutter's reason for avoiding it does not apply: a topmost window on Windows does not stop other windows taking focus.
    #[cfg(windows)]
    let _ = w.set_always_on_top(true);
}

/// The order matters. The window is created hidden so it is never mapped at the placeholder size tauri.conf.json gives it, and set_ignore_cursor_events comes after show() because the GTK call behind it needs a realised GDK window; both travel down the same ordered request channel, so by the time the shape is applied the window exists.
fn arm_overlay(app: &AppHandle) {
    let Some(w) = app.get_webview_window("overlay") else {
        return;
    };
    // The type hint has to be set while the window is still unmapped, because the window manager reads it once, when the window is first put on screen.
    #[cfg(target_os = "linux")]
    if let Ok(gtk_win) = w.gtk_window() {
        use gtk::prelude::GtkWindowExt;
        // A window asking for the whole screen is a window mutter maximises, and a maximised window is shrunk to the work area — that is what put the layer 32 pixels down the screen, under the top bar, with every ring 32 pixels low. A dock is placed exactly where it asks, is never maximised, never focused, and is stacked above ordinary windows, which is what a drawing layer wants.
        gtk_win.set_type_hint(gtk::gdk::WindowTypeHint::Dock);
        gtk_win.set_accept_focus(false);
        gtk_win.set_focus_on_map(false);
        set_overlay_wm_class(&gtk_win);
    }
    if let Some((x, y, width, height)) = union_bounds(&monitor_rects(app)) {
        let _ = w.unmaximize();
        let _ = w.set_size(tauri::PhysicalSize::new(width, height));
        let _ = w.set_position(tauri::PhysicalPosition::new(x, y));
    }
    let _ = w.set_skip_taskbar(true);
    // Never always-on-top. Mutter refuses focus to any new window that an always-on-top window would cover, and a full-screen layer covers every window, so with that flag set every app the user opened landed behind whatever was in front with an "is ready" notification (found 2026-09-05). The dock type hint above already stacks this layer over ordinary windows.
    let _ = w.set_visible_on_all_workspaces(true);
    // On Windows topmost stands in for the dock type hint, for the reason given in arm_hover.
    #[cfg(windows)]
    let _ = w.set_always_on_top(true);
    let _ = w.show();
    let _ = w.set_ignore_cursor_events(true);
}

/// Gives the overlay window "june-overlay" as its WM_CLASS instance name, so the desktop files it under the hidden june-overlay.desktop entry the daemon installs rather than under June's own entry. Input: the GTK window behind the overlay. Output: nothing; there is nothing to set off X11, and nothing to set until the window has an X window of its own.
/// This is what stops the dock from drawing a second dot for June. GNOME Shell puts every window of an application in one list whether or not the window asks to skip the taskbar — shell-app.c counts a skip-taskbar window out of the running-or-stopped decision, not out of the list — and a dock that draws one dot per window in that list draws one for this drawing layer, which is mapped the whole time June runs, as well as one for the real app window. Skipping the taskbar alone therefore does not remove the dot; the layer has to stop being one of June's windows.
/// Only the instance half of WM_CLASS changes. GNOME matches a window to a .desktop file by the instance name before anything else (get_app_from_window_wmclass in shell-window-tracker.c tries the instance against StartupWMClass, then the class, then the file names, and only after all four the process id), so the instance is the one field that has to move; the class half stays "June" because June's own activity tracker reads that half to recognise its own windows (IsJuneWindow in internal/tracker/tracker.go).
/// The matched entry is hidden and its only window skips the taskbar, so that application never reaches the running state and never gets a dock entry of its own.
#[cfg(target_os = "linux")]
fn set_overlay_wm_class(gtk_win: &gtk::ApplicationWindow) {
    use gtk::glib::translate::ToGlibPtr;
    use gtk::glib::Cast;
    use gtk::prelude::WidgetExt;
    use x11::xlib;

    // Realising the widget is what creates the X window, and it is not the same as showing it: an unrealised window has no class hint to write on, and a mapped one has already been read by the window manager. Nothing is put on screen here.
    if gtk_win.window().is_none() {
        gtk_win.realize();
    }
    let Some(gdk_win) = gtk_win.window() else {
        return;
    };
    if gdk_win.clone().downcast::<gdkx11::X11Window>().is_err() {
        return;
    }
    let raw: *mut gtk::gdk::ffi::GdkWindow = gdk_win.to_glib_none().0;
    let gdk_display = unsafe { gtk::gdk::ffi::gdk_window_get_display(raw) };
    if gdk_display.is_null() {
        return;
    }
    let xdisplay = unsafe {
        gdkx11::ffi::gdk_x11_display_get_xdisplay(gdk_display as *mut gdkx11::ffi::GdkX11Display)
    };
    if xdisplay.is_null() {
        return;
    }
    let xid = unsafe { gdkx11::ffi::gdk_x11_window_get_xid(raw as *mut gdkx11::ffi::GdkX11Window) };
    let (Ok(instance), Ok(class)) = (
        std::ffi::CString::new(OVERLAY_WM_CLASS_INSTANCE),
        std::ffi::CString::new("June"),
    ) else {
        return;
    };
    unsafe {
        let mut hint = xlib::XClassHint {
            res_name: instance.as_ptr() as *mut std::os::raw::c_char,
            res_class: class.as_ptr() as *mut std::os::raw::c_char,
        };
        xlib::XSetClassHint(xdisplay, xid, &mut hint);
        xlib::XFlush(xdisplay);
    }
}

/// Makes the overlay window take no pointer events at all, by giving it an empty input region. Input: the window. Output: nothing; there is nothing to do until the window has been drawn, which is why this runs from the window's own resize event rather than from setup.
/// Tauri's set_ignore_cursor_events leaves a one pixel input region at the window's top left corner, and the overlay window's top left corner is the top left corner of the desk. An empty region leaves nothing at all, so every click on every pixel goes to whatever is underneath.
#[cfg(target_os = "linux")]
fn drop_pointer_input(window: &tauri::Window) {
    use gtk::prelude::WidgetExt;
    let Ok(gtk_win) = window.gtk_window() else {
        return;
    };
    let Some(gdk_win) = gtk_win.window() else {
        return;
    };
    gdk_win.input_shape_combine_region(&gtk::cairo::Region::create(), 0, 0);
    // A resize is also every moment the window manager could have decided the overlay wants something, so the urgency hint and the attention state are dropped again here alongside the input region. This one has nothing to say.
    use gtk::prelude::GtkWindowExt;
    gtk_win.set_urgency_hint(false);
    drop_attention_demand(&gtk_win);
}

/// Keeps the overlay from telling the desktop it wants the user's attention. Input: the app handle. Output: nothing; a missing window only means the app is shutting down.
/// The state is dropped when the window manager reports the window mapped, which is just after the window manager set it, and again a second later because those two are a race with no signal to wait on.
#[cfg(target_os = "linux")]
fn hush_overlay(app: &AppHandle) {
    use gtk::prelude::WidgetExtManual;
    let Some(overlay) = app.get_webview_window("overlay") else {
        return;
    };
    let Ok(gtk_win) = overlay.gtk_window() else {
        return;
    };
    gtk_win.connect_map_event(|w, _| {
        drop_attention_demand(w);
        gtk::glib::Propagation::Proceed
    });
    let later = gtk_win.clone();
    gtk::glib::timeout_add_local_once(std::time::Duration::from_secs(1), move || {
        drop_attention_demand(&later)
    });
}

/// The type of the daemon event that asks this window to show something. The daemon's tray is the only menu June has, so the items that used to sit in this app's own tray reach the window as events on the stream it already reads.
pub(crate) const WINDOW_EVENT: &str = "window";

/// The action a daemon event is asking this window to take. Input: the JSON text of one event off the daemon's stream, in the shape internal/ipc.Event marshals to. Output: the event's text when its type is "window", which names the action, and None for every other event and for text that is not JSON at all.
fn window_action(payload: &str) -> Option<String> {
    let event: serde_json::Value = serde_json::from_str(payload).ok()?;
    if event.get("type").and_then(|v| v.as_str()) != Some(WINDOW_EVENT) {
        return None;
    }
    Some(
        event
            .get("text")
            .and_then(|v| v.as_str())
            .unwrap_or_default()
            .to_string(),
    )
}

/// Does what a daemon event asks of this window. Input: the app handle and the JSON text of one event off the daemon's stream. Output: true when the event was one of ours and has been acted on, false for every other event, which belongs to the pages. {"type": "window", "text": "open"} shows the main app window; {"type": "window", "text": "toggle"} runs the hover's show/hide, the same thing SIGHUP from the desktop's keybinding does.
pub(crate) fn window_command(app: &AppHandle, payload: &str) -> bool {
    match window_action(payload).as_deref() {
        Some("open") => {
            // The stream is read on a thread of its own, and touching a window from anywhere but the main loop crashes GTK, so the work is handed to the main loop rather than done here.
            let handle = app.clone();
            let _ = app.run_on_main_thread(move || open_app(&handle));
            true
        }
        Some("toggle") => {
            toggle(app);
            true
        }
        // The daemon is about to photograph the screen. June's own hover is drawn over whatever the user was looking at, so it steps off the screen for the moment the picture is taken and comes back exactly as it was; a hover that was already hidden stays hidden, which the page decides, not this.
        Some(action @ ("conceal" | "reveal")) => {
            let _ = app.emit(if action == "conceal" { "june://conceal" } else { "june://reveal" }, ());
            true
        }
        _ => false,
    }
}

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    // Wayland lets a client neither place its own window nor grab a global key, so the process asks for the X11 backend in main() before GTK has chosen one; see prefer_x11_backend there. Setting it at this point would be too late.
    tauri::Builder::default()
        .invoke_handler(tauri::generate_handler![
            ipc_token,
            system_theme,
            raise,
            dock_anchor,
            overlay_layout
        ])
        // Closing a window hides it instead of quitting: June keeps running under the daemon, and the same window comes back with its state when the daemon's tray asks for it again.
        .on_window_event(|window, event| {
            if let tauri::WindowEvent::CloseRequested { api, .. } = event {
                api.prevent_close();
                let _ = window.hide();
            }
            // A resize is the first moment the drawing layer has a real window behind it, and it is also every moment GTK could have handed the layer an input region back, so the region is emptied again each time.
            #[cfg(target_os = "linux")]
            if window.label() == "overlay" {
                if let tauri::WindowEvent::Resized(_) = event {
                    drop_pointer_input(window);
                }
            }
        })
        .setup(|app| {
            // There is no tray icon here. The daemon has one of its own (see cmd/tray_linux.go) and it is the supervisor, so a second icon beside it was two icons for one program; the items that used to be here reach this window as daemon events instead, see window_command.
            // The drawing layer covers the whole desk from the moment the app starts and shows nothing until the daemon says otherwise, so a ring never waits on a window being created.
            arm_overlay(app.handle());
            #[cfg(target_os = "linux")]
            hush_overlay(app.handle());
            // The daemon's stream is read here rather than in the page, because this app's WebKit holds a trickle of response body back long enough to lose a three second ring; see overlay.rs.
            overlay::stream_events(app.handle().clone());
            arm_hover(app.handle());
            // Nothing places the window here: it starts hidden and the page positions it against the dock, on the monitor the pointer is on, immediately before every show.
            #[cfg(unix)]
            listen_for_toggle_signal(app.handle().clone());
            #[cfg(windows)]
            register_toggle_hotkey(app.handle());
            Ok(())
        })
        .build(tauri::generate_context!())
        .expect("error while running tauri application")
        .run(|_app, _event| {
            // The pid file is removed on the way out, because the hotkey sends SIGHUP to whatever pid it names and SIGHUP's default disposition is terminate: a file left behind by a window that has gone aims that signal at whichever unrelated process the kernel later gives that number to. A kill -9 still leaves the file behind, so the shortcut should also check /proc/<pid>/cmdline before signalling.
            #[cfg(unix)]
            if let tauri::RunEvent::Exit = _event {
                if let Some(path) = pid_path() {
                    let _ = std::fs::remove_file(path);
                }
            }
        });
}

#[cfg(test)]
mod tests {
    use super::{union_bounds, window_action};

    #[test]
    fn union_bounds_covers_every_monitor() {
        assert_eq!(union_bounds(&[]), None);
        // A screen placed to the left of the primary one has a negative x, and the overlay window has to start there rather than at zero, or every rect on it lands off the window.
        assert_eq!(
            union_bounds(&[(0, 0, 1920, 1080), (-1280, -200, 1280, 1024)]),
            Some((-1280, -200, 3200, 1280))
        );
        assert_eq!(union_bounds(&[(10, 10, 0, 0)]), Some((10, 10, 1, 1)));
    }

    #[test]
    fn only_a_window_event_names_an_action() {
        // The daemon marshals every field of its event, so the real payload carries the rest of them too.
        assert_eq!(
            window_action(
                r#"{"id":"window","type":"window","text":"open","detail":"","evidence":null,"actions":null,"conversation_id":""}"#
            ),
            Some("open".to_string())
        );
        assert_eq!(
            window_action(r#"{"id":"overlay","type":"overlay","text":"{\"kind\":\"ring\"}"}"#),
            None
        );
        assert_eq!(window_action("not json at all"), None);
    }
}
