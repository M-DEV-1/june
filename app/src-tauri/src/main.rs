// Prevents additional console window on Windows in release, DO NOT REMOVE!!
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

/// Picks GTK's backend before anything has had a chance to initialise GTK, which is why this is the first thing main does rather than something lib.rs sets up. Wayland gives a client no way to place its own window — the protocol has no request for it — so GTK's move calls are dropped on the floor, outer_position() reads back 0,0 and the compositor puts the hover where it pleases, which under GNOME is the middle of the screen. Ora has to put the hover against the dock, and its raise command needs X11's present_with_time to take focus, so it asks for the X11 backend (XWayland, when the session itself is Wayland) whenever an X display exists and nothing in the environment has already chosen a backend. Input: none; it reads the GDK_BACKEND and DISPLAY environment variables. Output: nothing, but GDK_BACKEND may be set for this process.
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

fn main() {
    prefer_x11_backend();
    ora_lib::run()
}
