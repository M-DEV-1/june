// Package window brings an already-running window belonging to another application to the front, on GNOME/Wayland, where the shell itself refuses this to any ordinary unprivileged caller. It talks over D-Bus to a small GNOME Shell extension (packaging/gnome-extension/ora@ora.local) that runs inside the shell's own process and therefore is not subject to that restriction.
package window

import (
	"context"

	"github.com/godbus/dbus/v5"
)

// busName, objectPath and ifaceName identify the ora@ora.local extension's exported D-Bus object once it is loaded and enabled in the running shell.
const (
	busName    = "org.gnome.Shell"
	objectPath = dbus.ObjectPath("/org/gnome/Shell/Extensions/Ora")
	ifaceName  = "org.gnome.Shell.Extensions.Ora"
)

// Raiser calls the ora@ora.local GNOME Shell extension to activate an already-running window. It is safe for concurrent use, since the underlying dbus.Conn is.
type Raiser struct {
	conn *dbus.Conn
}

// New dials the caller's session bus. It always dials fresh rather than reusing godbus's process-wide cached connection, so a caller that changes DBUS_SESSION_BUS_ADDRESS between calls (as the tests in this package do) reliably gets the bus that variable names. Input: none. Output: a Raiser ready to use, or an error if the session bus could not be reached.
func New() (*Raiser, error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, err
	}
	return &Raiser{conn: conn}, nil
}

// Close drops the session-bus connection New dialed. Input: none. Output: the connection's own close error, or nil. Calls made after it fail rather than reconnecting, so it belongs in a shutdown sequence and nowhere else.
func (r *Raiser) Close() error {
	return r.conn.Close()
}

func (r *Raiser) object() dbus.BusObject {
	return r.conn.Object(busName, objectPath)
}

// Available reports whether the extension is loaded and enabled in the running shell right now. Input: a context bounding the D-Bus round trip. Output: true if the extension answered, false plus the call's error if it did not (not loaded, not enabled, or no session bus).
func (r *Raiser) Available(ctx context.Context) (bool, error) {
	call := r.object().CallWithContext(ctx, ifaceName+".List", 0)
	if call.Err != nil {
		return false, call.Err
	}
	return true, nil
}

// ByPid asks the extension to activate the window belonging to process pid. Input: the target process's pid. Output: true if a matching window was found and activated, false if none matched, or an error if the D-Bus call itself failed.
func (r *Raiser) ByPid(ctx context.Context, pid uint32) (bool, error) {
	return r.call(ctx, "ActivateByPid", pid)
}

// ByTitle asks the extension to activate a window whose title contains substring. Input: the substring to match. Output: true if a matching window was found and activated, false if none matched, or an error if the D-Bus call itself failed.
func (r *Raiser) ByTitle(ctx context.Context, substring string) (bool, error) {
	return r.call(ctx, "ActivateByTitle", substring)
}

// ByWmClass asks the extension to activate a window with the given WM_CLASS. Input: the WM_CLASS to match. Output: true if a matching window was found and activated, false if none matched, or an error if the D-Bus call itself failed.
func (r *Raiser) ByWmClass(ctx context.Context, wmClass string) (bool, error) {
	return r.call(ctx, "ActivateByWmClass", wmClass)
}

// call invokes a single-string-argument, bool-returning method on the extension's D-Bus interface. Input: the bare method name and its one argument. Output: the method's bool result, or an error if the D-Bus call itself failed.
func (r *Raiser) call(ctx context.Context, method string, arg any) (bool, error) {
	var ok bool
	call := r.object().CallWithContext(ctx, ifaceName+"."+method, 0, arg)
	if call.Err != nil {
		return false, call.Err
	}
	if err := call.Store(&ok); err != nil {
		return false, err
	}
	return ok, nil
}
