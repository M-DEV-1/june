package cmd

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/spf13/cobra"

	"ora/internal/config"
	"ora/internal/window"
)

// doctorCheck is one line of the readiness report: what was checked, whether it is ready, what was found, and what to do when it is not.
type doctorCheck struct {
	Name, Detail, Fix string
	OK                bool
}

// doctorCmd is "ora doctor": one report over everything computer use stands on, with the first blocker and its fix named at the end, so a desk where clicks do not land can be read in one go instead of from the log.
var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Report whether this desk is ready for Ora to see and drive the screen",
	Run: func(cmd *cobra.Command, args []string) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		fmt.Print(doctorReport(runDoctor(ctx)))
		if show, _ := cmd.Flags().GetBool("windows"); show {
			fmt.Print(windowReport(ctx))
		}
	},
}

func init() {
	doctorCmd.Flags().Bool("windows", false, "Also list every open window as the shell extension reports it: focus, class, title and frame")
	rootCmd.AddCommand(doctorCmd)
}

// windowReport lists the open windows as the extension reports them, one per line, which is the view every click is placed against. Output: the lines, or one line saying why there are none.
func windowReport(ctx context.Context) string {
	r, err := window.New()
	if err != nil {
		return "\nwindows: " + err.Error() + "\n"
	}
	defer r.Close()
	windows, err := r.List(ctx)
	if err != nil {
		return "\nwindows: " + err.Error() + "\n"
	}
	var b strings.Builder
	b.WriteString("\nwindows:\n")
	for _, w := range windows {
		mark := "  "
		if w.Focused {
			mark = "* "
		}
		fmt.Fprintf(&b, "%s%-18s %4d,%-4d %4dx%-4d %s\n", mark, w.WmClass, w.X, w.Y, w.W, w.H, w.Title)
	}
	return b.String()
}

// runDoctor runs every check against the live desk. Input: a context bounding the bus calls. Output: the checks, in the order the report prints them.
func runDoctor(ctx context.Context) []doctorCheck {
	var out []doctorCheck
	conn, err := dbus.SessionBus()
	if err != nil {
		return append(out, doctorCheck{Name: "session bus", Detail: err.Error(), Fix: "run from a graphical login"})
	}
	// Accessibility: the bus must exist and applications must be told to build their trees, or observe_screen lists nothing.
	var enabled dbus.Variant
	if err := conn.Object("org.a11y.Bus", "/org/a11y/bus").CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0, "org.a11y.Status", "IsEnabled").Store(&enabled); err != nil {
		out = append(out, doctorCheck{Name: "accessibility bus", Detail: err.Error(), Fix: "the daemon sets org.a11y.Status.IsEnabled at start; start it once"})
	} else if on, _ := enabled.Value().(bool); !on {
		out = append(out, doctorCheck{Name: "accessibility bus", Detail: "reachable, IsEnabled is false", Fix: "start the daemon once; it turns IsEnabled on"})
	} else {
		out = append(out, doctorCheck{Name: "accessibility bus", Detail: "reachable, IsEnabled is true", OK: true})
	}
	// Screenshot: the shell's own screenshot call is what look and the press check use.
	var owned bool
	if err := conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.NameHasOwner", 0, "org.gnome.Shell").Store(&owned); err != nil || !owned {
		out = append(out, doctorCheck{Name: "screenshot", Detail: "org.gnome.Shell is not on the bus", Fix: "looks fall back to the portal, which asks for consent"})
	} else {
		out = append(out, doctorCheck{Name: "screenshot", Detail: "gnome-shell screenshot available", OK: true})
	}
	// Window frames: the only source of window positions on Wayland is the bundled extension, and only a build that reports frames places clicks.
	if r, err := window.New(); err != nil {
		out = append(out, doctorCheck{Name: "window frames", Detail: err.Error(), Fix: "install the GNOME extension (packaging/gnome-extension) and log in again"})
	} else {
		defer r.Close()
		windows, err := r.List(ctx)
		switch {
		case err != nil:
			out = append(out, doctorCheck{Name: "window frames", Detail: "extension not answering: " + err.Error(), Fix: "enable the ora@ora.local extension and log in again"})
		case !anyFrame(windows):
			out = append(out, doctorCheck{Name: "window frames", Detail: fmt.Sprintf("%d windows listed, none with a frame", len(windows)), Fix: "the installed extension predates window frames: log out and in to load the new one"})
		default:
			out = append(out, doctorCheck{Name: "window frames", Detail: fmt.Sprintf("%d windows listed with frames", len(windows)), OK: true})
		}
	}
	// Pointer: a saved restore token means the RemoteDesktop consent was given once and the session opens silently.
	if _, err := os.Stat(filepath.Join(config.DataDir(), "portal-input-token")); err != nil {
		out = append(out, doctorCheck{Name: "pointer and keyboard", Detail: "no saved consent", Fix: "the first press will show the portal's consent dialog once; allow it"})
	} else {
		out = append(out, doctorCheck{Name: "pointer and keyboard", Detail: "consent saved; how many monitors it covers is only known once the session opens", OK: true})
	}
	// Daemon: everything above is driven by it.
	if resp, err := http.Get("http://127.0.0.1:" + DaemonPort + "/ping"); err != nil {
		out = append(out, doctorCheck{Name: "daemon", Detail: "not answering on " + DaemonPort, Fix: "run ora"})
	} else {
		resp.Body.Close()
		out = append(out, doctorCheck{Name: "daemon", Detail: "answering", OK: true})
	}
	return out
}

// anyFrame reports whether at least one listed window carries a frame rectangle.
func anyFrame(windows []window.Window) bool {
	for _, w := range windows {
		if w.W > 0 && w.H > 0 {
			return true
		}
	}
	return false
}

// doctorReport renders the checks as one line each, then a readiness line naming every blocker and the fix for the first. Input: the checks. Output: the text.
func doctorReport(checks []doctorCheck) string {
	var b strings.Builder
	var blockers []doctorCheck
	for _, c := range checks {
		mark := "ok  "
		if !c.OK {
			mark = "FAIL"
			blockers = append(blockers, c)
		}
		fmt.Fprintf(&b, "%s  %-22s %s\n", mark, c.Name, c.Detail)
	}
	if len(blockers) == 0 {
		b.WriteString("\nready: everything computer use needs is in place\n")
		return b.String()
	}
	names := make([]string, len(blockers))
	for i, c := range blockers {
		names[i] = c.Name
	}
	fmt.Fprintf(&b, "\nnot ready: %s\nnext: %s\n", strings.Join(names, ", "), blockers[0].Fix)
	return b.String()
}
