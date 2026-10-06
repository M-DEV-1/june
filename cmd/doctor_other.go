//go:build !windows

package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"june/internal/util"
)

// platformChecks reports the PipeWire pieces recording and call detection go through: the pulse server and pw-dump. Input: $XDG_RUNTIME_DIR. Output: one check each.
func platformChecks(runtimeDir string) []doctorCheck {
	var out []doctorCheck
	// Recording goes through PipeWire's pulse server, found the way github.com/jfreymuth/pulse finds it: $PULSE_SERVER when set, the socket under $XDG_RUNTIME_DIR otherwise.
	if server := os.Getenv("PULSE_SERVER"); server != "" {
		out = append(out, doctorCheck{Name: "audio server", Detail: "PULSE_SERVER is " + server, OK: true})
	} else if sock := filepath.Join(runtimeDir, "pulse", "native"); !util.Exists(sock) {
		out = append(out, doctorCheck{Name: "audio server", Detail: "no pulse socket at " + sock + "; meeting recording and voice input are off", Fix: "install and start PipeWire with its pulse server (pipewire-pulse)"})
	} else {
		out = append(out, doctorCheck{Name: "audio server", Detail: "pulse socket at " + sock, OK: true})
	}
	if bin, err := exec.LookPath("pw-dump"); err != nil {
		out = append(out, doctorCheck{Name: "call detection", Detail: "no pw-dump on PATH (" + os.Getenv("PATH") + "); noticing a call and offering to record it is off", Fix: "install PipeWire's command-line tools (pipewire-bin on Debian and Ubuntu)"})
	} else {
		out = append(out, doctorCheck{Name: "call detection", Detail: "pw-dump at " + bin, OK: true})
	}
	return out
}

// portHolder finds the process listening on a TCP port, so a message about the port June cannot have names who has it instead of guessing. It reads the listening sockets in /proc/net/tcp and /proc/net/tcp6, the second because a dual-stack listener on [::] holds the IPv4 port as well and is listed only there, and looks for the process whose descriptors hold that socket. Only the user's own processes can be looked into, and a system without /proc finds nothing. Input: the port. Output: the pid, the program's name from /proc/<pid>/comm, and false when no holder was found.
func portHolder(port string) (pid uint32, name string, ok bool) {
	want, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return 0, "", false
	}
	socket := ""
	for _, file := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		table, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(table), "\n") {
			// sl, local_address as hex ADDR:PORT, rem_address, st (0A is LISTEN), the queues and timers, uid, timeout, and the socket's inode tenth.
			f := strings.Fields(line)
			if len(f) < 10 || f[3] != "0A" {
				continue
			}
			_, hexPort, found := strings.Cut(f[1], ":")
			if p, err := strconv.ParseUint(hexPort, 16, 16); found && err == nil && p == want && f[9] != "0" {
				socket = "socket:[" + f[9] + "]"
				break
			}
		}
		if socket != "" {
			break
		}
	}
	if socket == "" {
		return 0, "", false
	}
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return 0, "", false
	}
	for _, p := range procs {
		n, err := strconv.ParseUint(p.Name(), 10, 32)
		if err != nil {
			continue
		}
		fds, err := os.ReadDir(filepath.Join("/proc", p.Name(), "fd"))
		if err != nil {
			continue
		}
		for _, fd := range fds {
			if link, err := os.Readlink(filepath.Join("/proc", p.Name(), "fd", fd.Name())); err == nil && link == socket {
				comm, _ := os.ReadFile(filepath.Join("/proc", p.Name(), "comm"))
				return uint32(n), strings.TrimSpace(string(comm)), true
			}
		}
	}
	return 0, "", false
}
