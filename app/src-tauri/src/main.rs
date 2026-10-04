// Prevents additional console window on Windows in release, DO NOT REMOVE!!
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

/// Picks GTK's backend before anything has had a chance to initialise GTK, which is why this is the first thing main does rather than something lib.rs sets up. Wayland gives a client no way to place its own window — the protocol has no request for it — so GTK's move calls are dropped on the floor, outer_position() reads back 0,0 and the compositor puts the hover where it pleases, which under GNOME is the middle of the screen. June has to put the hover against the dock, and its raise command needs X11's present_with_time to take focus, so it asks for the X11 backend (XWayland, when the session itself is Wayland) whenever an X display exists and nothing in the environment has already chosen a backend. Input: none; it reads the GDK_BACKEND and DISPLAY environment variables. Output: nothing, but GDK_BACKEND may be set for this process.
#[cfg(target_os = "linux")]
fn prefer_x11_backend() {
    if std::env::var_os("GDK_BACKEND").is_some() {
        return;
    }
    if std::env::var("DISPLAY")
        .map(|d| !d.is_empty())
        .unwrap_or(false)
    {
        std::env::set_var("GDK_BACKEND", "x11");
    }
}

/// Gives this process the same AppUserModelID the installer's Start menu and desktop shortcuts carry (packaging/windows/june.iss). The window that shows on the taskbar belongs to june-window.exe, which the daemon starts itself, so no shortcut's ID reaches it: without this, pinning June's taskbar button pinned june-window.exe, which on its own opens nothing and grabs the hotkey. With the ID set, the taskbar groups the window under the shortcut and a pin relaunches junew.exe through it. It has to run before any window exists, because the taskbar reads the ID when a window first appears. Input: none. Output: nothing; a failure leaves the taskbar on its old exe-based grouping, which is no worse than before.
#[cfg(windows)]
fn set_app_user_model_id() {
    // Declared here rather than through the windows crate: one call does not justify a new dependency in a lockfile CI checks with --locked.
    #[allow(non_snake_case)]
    #[link(name = "shell32")]
    extern "system" {
        fn SetCurrentProcessExplicitAppUserModelID(app_id: *const u16) -> i32;
    }
    let id: Vec<u16> = "M-DEV-1.June"
        .encode_utf16()
        .chain(std::iter::once(0))
        .collect();
    // SAFETY: id is a NUL-terminated UTF-16 string that outlives the call, and shell32 copies it.
    let _ = unsafe { SetCurrentProcessExplicitAppUserModelID(id.as_ptr()) };
}

fn main() {
    #[cfg(windows)]
    set_app_user_model_id();
    #[cfg(target_os = "linux")]
    prefer_x11_backend();
    june_lib::run()
}
