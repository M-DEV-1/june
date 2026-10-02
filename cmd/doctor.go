package cmd

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/spf13/cobra"

	"june/internal/config"
	"june/internal/recorder"
	"june/internal/util"
	"june/internal/window"
)

// doctorCheck is one line of the readiness report: what was checked, whether it is ready, what was found, and what to do when it is not.
type doctorCheck struct {
	Name, Detail, Fix string
	OK                bool
}

// doctorCmd is "june doctor": one report over everything computer use stands on and every piece June runs locally, with the first blocker and its fix named at the end, so a desk where clicks do not land or a meeting that never transcribes can be read in one go instead of from the log.
var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Report whether this desk is ready for June to see and drive the screen, and which local pieces are missing",
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

// runDoctor runs every check against the live desk. Input: a context bounding the bus calls. Output: the checks, in the order the report prints them. The desk checks (the session bus, accessibility, screenshot, pointer consent) are about a Linux desktop and run only there; window frames, the brain, the local pieces and the daemon are checked everywhere. Windows reports window frames through Win32 itself, with no extension to install.
func runDoctor(ctx context.Context) []doctorCheck {
	var out []doctorCheck
	if runtime.GOOS == "linux" {
		desk, ok := deskChecks(ctx)
		out = append(out, desk...)
		if !ok {
			return out
		}
	} else {
		out = append(out, windowFramesCheck(ctx))
	}
	home, _ := os.UserHomeDir()
	out = append(out, brainCheck(home, os.Getenv("GEMINI_API_KEY")))
	out = append(out, localPieceChecks(config.DataDir(), os.Getenv("XDG_RUNTIME_DIR"), config.LoadConfig().Embed)...)
	// Daemon: everything above is driven by it.
	if resp, err := http.Get("http://127.0.0.1:" + DaemonPort + "/ping"); err != nil {
		out = append(out, doctorCheck{Name: "daemon", Detail: "not answering on " + DaemonPort, Fix: "run june"})
	} else {
		resp.Body.Close()
		out = append(out, doctorCheck{Name: "daemon", Detail: "answering", OK: true})
	}
	return out
}

// deskChecks checks what computer use needs from a Linux desktop. Input: a context bounding the bus calls. Output: the checks, and false when there is no session bus, in which case nothing after it can be checked either.
func deskChecks(ctx context.Context) ([]doctorCheck, bool) {
	var out []doctorCheck
	conn, err := dbus.SessionBus()
	if err != nil {
		return append(out, doctorCheck{Name: "session bus", Detail: err.Error(), Fix: "run from a graphical login"}), false
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
	out = append(out, windowFramesCheck(ctx))
	// Pointer: a saved restore token means the RemoteDesktop consent was given once and the session opens silently. What it does not say is how many monitors that grant covers, so the desk's live monitor count is read separately and handed alongside it.
	out = append(out, pointerCheck(filepath.Join(config.DataDir(), "portal-input-token"), monitorCount(ctx, conn)))
	return out, true
}

// windowFramesCheck reports whether open windows can be listed with their frames, which every click is placed against. On Wayland the only source of window positions is the bundled GNOME extension, and only a build that reports frames places clicks. Input: a context bounding the call. Output: the check.
func windowFramesCheck(ctx context.Context) doctorCheck {
	r, err := window.New()
	if err != nil {
		return doctorCheck{Name: "window frames", Detail: err.Error(), Fix: "install the GNOME extension (packaging/gnome-extension) and log in again"}
	}
	defer r.Close()
	windows, err := r.List(ctx)
	switch {
	case err != nil:
		return doctorCheck{Name: "window frames", Detail: "extension not answering: " + err.Error(), Fix: "enable the june@june.local extension and log in again"}
	case !anyFrame(windows):
		return doctorCheck{Name: "window frames", Detail: fmt.Sprintf("%d windows listed, none with a frame", len(windows)), Fix: "the installed extension predates window frames: log out and in to load the new one"}
	default:
		return doctorCheck{Name: "window frames", Detail: fmt.Sprintf("%d windows listed with frames", len(windows)), OK: true}
	}
}

// pointerCheck reports whether pointer and keyboard consent is saved, and, when it is, the desk's live monitor count next to the exact file a re-grant needs deleted. A restore token saved from an older, narrower grant keeps restoring that same grant forever — the portal never asks again on its own — so a click on a monitor the grant does not cover fails deep inside the coordinate mapping. Input: the saved token's path, and the desk's monitor count (-1 when it could not be read). Output: the check.
func pointerCheck(tokenPath string, monitors int) doctorCheck {
	if _, err := os.Stat(tokenPath); err != nil {
		return doctorCheck{Name: "pointer and keyboard", Detail: "no saved consent", Fix: "the first press will show the portal's consent dialog once; allow it"}
	}
	if monitors < 0 {
		return doctorCheck{Name: "pointer and keyboard", Detail: "consent saved; this desk's monitor count could not be read", OK: true}
	}
	return doctorCheck{
		Name:   "pointer and keyboard",
		Detail: fmt.Sprintf("consent saved; this desk has %d monitor(s). A grant saved with fewer keeps restoring that narrower one; if a click on another monitor fails, delete %s and the next press will ask again", monitors, tokenPath),
		OK:     true,
	}
}

// monitorCount asks GNOME's own display config for how many monitors this desk has right now, the same count a fresh RemoteDesktop consent would be granted across. Input: a context bounding the call, and the session bus doctor already opened. Output: the monitor count, or -1 when Mutter's DisplayConfig is not reachable (not GNOME, or no monitors attached).
func monitorCount(ctx context.Context, conn *dbus.Conn) int {
	var serial uint32
	var monitors, logical []interface{}
	var props map[string]dbus.Variant
	err := conn.Object("org.gnome.Mutter.DisplayConfig", "/org/gnome/Mutter/DisplayConfig").
		CallWithContext(ctx, "org.gnome.Mutter.DisplayConfig.GetCurrentState", 0).
		Store(&serial, &monitors, &logical, &props)
	if err != nil {
		return -1
	}
	return len(monitors)
}

// localPieceChecks reports every piece June runs locally and a clean machine may not have: whisper-cli and its model, the Silero voice-activity model, the sherpa-onnx diarizer and its models, the embedding llama-server and its GGUF, and what recording and call detection need from the desktop (see audioChecks). Nothing downloads any of them. Each line names the exact path it was looked for at and the feature that is off without it.
// Input: the data directory, $XDG_RUNTIME_DIR, and the embed block of the config. Output: one check per piece, in that order.
func localPieceChecks(dataDir, runtimeDir string, embed config.EmbedConfig) []doctorCheck {
	var out []doctorCheck
	if bin, err := recorder.WhisperCPPBinary(dataDir); err != nil {
		out = append(out, doctorCheck{Name: "meeting transcription", Detail: err.Error() + "; meeting transcription is off", Fix: "install whisper.cpp's whisper-cli" + exeSuffix + " with ggml-medium.bin beside it at the path above, or point JUNE_WHISPER_CPP at one"})
	} else {
		out = append(out, doctorCheck{Name: "meeting transcription", Detail: "whisper-cli at " + bin, OK: true})
	}
	if vad := recorder.WhisperVADModel(dataDir); !util.Exists(vad) {
		out = append(out, doctorCheck{Name: "voice activity model", Detail: "no Silero model at " + vad + "; voice activity detection is off, and a long, mostly quiet meeting transcribes badly without it", Fix: "put ggml-silero-v6.2.0.bin at " + vad})
	} else {
		out = append(out, doctorCheck{Name: "voice activity model", Detail: "installed at " + vad, OK: true})
	}
	if bin, err := recorder.SherpaBinary(dataDir); err != nil {
		out = append(out, doctorCheck{Name: "speaker diarization", Detail: err.Error() + "; splitting the call into speakers is off", Fix: "install sherpa-onnx's diarizer, its lib directory and both models at the paths above, or point JUNE_SHERPA at one"})
	} else {
		out = append(out, doctorCheck{Name: "speaker diarization", Detail: "diarizer at " + bin, OK: true})
	}
	out = append(out, embedCheck(embed))
	out = append(out, audioChecks(runtimeDir)...)
	return out
}

// embedCheck reports whether the local embedding server and its model are where the config says. The daemon starts no embedder unless both paths are set, and then search matches words only. Input: the config's embed block. Output: the check, naming the config file or the missing path.
func embedCheck(embed config.EmbedConfig) doctorCheck {
	const off = "; local memory search is off and search matches words only"
	if !embed.LocalEnabled() {
		return doctorCheck{Name: "local memory search", Detail: "embed.llama_server and embed.model_path are not both set in " + config.ConfigPath() + off, Fix: "set embed.llama_server to a llama.cpp llama-server and embed.model_path to the EmbeddingGemma GGUF in " + config.ConfigPath()}
	}
	for _, p := range []string{embed.LlamaServer, embed.ModelPath} {
		if !util.Exists(p) {
			return doctorCheck{Name: "local memory search", Detail: "nothing at " + p + ", named in " + config.ConfigPath() + off, Fix: "put the file at " + p + " or correct the path in " + config.ConfigPath()}
		}
	}
	return doctorCheck{Name: "local memory search", Detail: "llama-server at " + embed.LlamaServer + ", model at " + embed.ModelPath, OK: true}
}

// brainCheck reports whether June has anything to think with. Everything else doctor checks is about the desk — the buses, the screen, the pointer — and a machine can pass all of it and still not answer a single question, which is exactly what a clean install does before a key or a login is in place. Input: the home directory the CLI login files live under, and the Gemini API key as the environment gives it. Output: the check, naming every brain it found, or saying how to give it one.
func brainCheck(home, apiKey string) doctorCheck {
	var found []string
	if strings.TrimSpace(apiKey) != "" {
		found = append(found, "a Gemini API key")
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", ".credentials.json")); err == nil {
		found = append(found, "a Claude subscription")
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "auth.json")); err == nil {
		found = append(found, "a ChatGPT subscription")
	}
	if len(found) == 0 {
		return doctorCheck{
			Name:   "brain",
			Detail: "no brain: nothing here can answer a question",
			Fix:    "put GEMINI_API_KEY in " + filepath.Join(config.DataDir(), "env") + ", or run claude login, or codex login",
		}
	}
	return doctorCheck{Name: "brain", Detail: strings.Join(found, ", "), OK: true}
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
		b.WriteString("\nready: everything June needs is in place\n")
		return b.String()
	}
	names := make([]string, len(blockers))
	for i, c := range blockers {
		names[i] = c.Name
	}
	fmt.Fprintf(&b, "\nnot ready: %s\nnext: %s\n", strings.Join(names, ", "), blockers[0].Fix)
	return b.String()
}
