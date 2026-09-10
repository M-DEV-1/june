import Gio from 'gi://Gio';
import GLib from 'gi://GLib';
import {Extension} from 'resource:///org/gnome/shell/extensions/extension.js';

// The D-Bus interface Ora's Go daemon calls. It runs inside gnome-shell's own process, so it is exempt from the session-bus allow-list that otherwise refuses window activation to an external, unprivileged caller.
const ORA_IFACE = `
<node>
  <interface name="org.gnome.Shell.Extensions.Ora">
    <method name="ActivateByPid">
      <arg type="u" direction="in" name="pid"/>
      <arg type="b" direction="out" name="success"/>
    </method>
    <method name="ActivateByTitle">
      <arg type="s" direction="in" name="substring"/>
      <arg type="b" direction="out" name="success"/>
    </method>
    <method name="ActivateByWmClass">
      <arg type="s" direction="in" name="wmClass"/>
      <arg type="b" direction="out" name="success"/>
    </method>
    <method name="List">
      <arg type="s" direction="out" name="json"/>
    </method>
  </interface>
</node>`;

// The names of the executables allowed to call this interface, checked against what /proc/<pid>/exe resolves to for the calling process. These are the binaries packaging/install.sh installs and scripts/release.sh stages: the daemon, under either name it has been shipped as. Anything else on the session bus is refused, because List alone enumerates every open window's title.
const ORA_BINARIES = ['ora', 'ora-daemon'];

// Raises a window the same way activate-window-by-title does: hand it to its own workspace with a fresh shell-generated timestamp, which GNOME's focus-stealing prevention accepts because it comes from inside the shell. Input: a Meta.Window, or null if nothing matched. Output: true if a window was raised, false if win was null.
function activate(win) {
    if (!win) {
        return false;
    }
    const now = global.get_current_time();
    const workspace = win.get_workspace();
    if (workspace) {
        workspace.activate_with_focus(win, now);
    } else {
        win.activate(now);
    }
    return true;
}

// Returns the first open window for which predicate is true, or null if none match. Input: a function taking a Meta.Window and returning bool. Output: the matching Meta.Window, or null.
function findWindow(predicate) {
    for (const actor of global.get_window_actors()) {
        const win = actor.meta_window;
        if (win && predicate(win)) {
            return win;
        }
    }
    return null;
}

// Reports whether text contains substring, ignoring case and treating a missing value as no match. Input: the window's own string and the caller's, either of which may be null. Output: whether one contains the other, case-folded.
function containsFolded(text, substring) {
    if (!text || !substring) {
        return false;
    }
    return text.toLowerCase().includes(substring.toLowerCase());
}

// Reads the process id behind a D-Bus sender name by asking the bus itself. Input: a unique sender name, as invocation.get_sender() returns. Output: the pid, or -1 if the bus would not say (the caller has already gone).
function senderPid(sender) {
    try {
        const reply = Gio.DBus.session.call_sync(
            'org.freedesktop.DBus', '/org/freedesktop/DBus', 'org.freedesktop.DBus',
            'GetConnectionUnixProcessID', new GLib.Variant('(s)', [sender]),
            new GLib.VariantType('(u)'), Gio.DBusCallFlags.NONE, 1000, null);
        return reply.deepUnpack()[0];
    } catch (e) {
        return -1;
    }
}

// Answers a call with an access-denied error unless it came from Ora's own daemon, so no other process running as this user can enumerate window titles or steal focus through this interface. Input: the method invocation. Output: true when the call may go ahead; false once it has been refused, and the caller must then do nothing more with it.
function callerIsOra(invocation) {
    const pid = senderPid(invocation.get_sender());
    let name = '';
    if (pid > 0) {
        try {
            name = GLib.path_get_basename(GLib.file_read_link(`/proc/${pid}/exe`));
        } catch (e) {
            name = '';
        }
    }
    if (ORA_BINARIES.includes(name)) {
        return true;
    }
    invocation.return_error_literal(Gio.DBusError, Gio.DBusError.ACCESS_DENIED,
        'this interface answers Ora\'s own daemon and nothing else');
    return false;
}

export default class OraWindowRaiserExtension extends Extension {
    // Exports the D-Bus object. Input: none (called by gnome-shell when the extension is enabled). Output: none.
    enable() {
        this._dbusImpl = Gio.DBusExportedObject.wrapJSObject(ORA_IFACE, this);
        this._dbusImpl.export(Gio.DBus.session, '/org/gnome/Shell/Extensions/Ora');
    }

    // Unexports the D-Bus object and drops the reference to it. Input: none (called by gnome-shell when the extension is disabled). Output: none.
    disable() {
        this._dbusImpl?.unexport();
        this._dbusImpl = null;
    }

    // Activates the window owned by process pid. The Async form is what receives the invocation, which is how the caller can be checked before anything is done for it. Input: the pid (uint32) and the invocation. Output: none; true or false is returned over the bus if the caller is allowed.
    ActivateByPidAsync([pid], invocation) {
        if (!callerIsOra(invocation)) {
            return;
        }
        const raised = activate(findWindow(win => win.get_pid() === pid));
        invocation.return_value(new GLib.Variant('(b)', [raised]));
    }

    // Activates a window whose title contains substring, ignoring case, since the name the caller has is what a person typed and a title is capitalised however the application likes. Input: the substring and the invocation. Output: none; true or false is returned over the bus if the caller is allowed.
    ActivateByTitleAsync([substring], invocation) {
        if (!callerIsOra(invocation)) {
            return;
        }
        const raised = activate(findWindow(win => containsFolded(win.get_title(), substring)));
        invocation.return_value(new GLib.Variant('(b)', [raised]));
    }

    // Activates a window whose class contains wmClass, ignoring case. It is a substring match because what a window reports is rarely what the application is called: Brave reports "brave-browser", and a Wayland-native client reports an app id rather than an X11 WM_CLASS at all. Input: the class and the invocation. Output: none; true or false is returned over the bus if the caller is allowed.
    ActivateByWmClassAsync([wmClass], invocation) {
        if (!callerIsOra(invocation)) {
            return;
        }
        const raised = activate(findWindow(win => containsFolded(win.get_wm_class(), wmClass)));
        invocation.return_value(new GLib.Variant('(b)', [raised]));
    }

    // Lists every open window. Actors whose meta_window is null — one mid-creation, or a helper the compositor owns — are left out, because reading one would throw and take Available, which calls this, down with it. Input: the invocation. Output: none; a JSON string encoding an array of {id, pid, wm_class, title, focused, x, y, width, height}, the last four being the window's frame in logical screen pixels, one entry per open window, is returned over the bus if the caller is allowed.
    ListAsync(params, invocation) {
        if (!callerIsOra(invocation)) {
            return;
        }
        const windows = global.get_window_actors()
            .map(actor => actor.meta_window)
            .filter(win => win)
            .map(win => {
                // The frame rectangle is the one thing nothing else on a Wayland desk can say: a client on the accessibility bus reports its widgets relative to its window and its window at 0,0, so the daemon needs this to turn a listing into screen pixels.
                const frame = win.get_frame_rect();
                return {
                    id: win.get_id(),
                    pid: win.get_pid(),
                    wm_class: win.get_wm_class(),
                    title: win.get_title(),
                    focused: win.has_focus(),
                    x: frame.x,
                    y: frame.y,
                    width: frame.width,
                    height: frame.height,
                };
            });
        invocation.return_value(new GLib.Variant('(s)', [JSON.stringify(windows)]));
    }
}
