//go:build !windows

package cmd

import (
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// clockTicks is the kernel's USER_HZ, the unit /proc/<pid>/stat counts a process's start in. It is 100 on every architecture Linux runs a desktop on, and reading it properly needs sysconf, which a build without cgo does not have.
const clockTicks = 100

// processStarted is when pid started, from its start in clock ticks since boot (/proc/<pid>/stat) and the boot time (/proc/stat). Output: the time, and false when it cannot be told, as on a system without /proc.
func processStarted(pid uint32) (time.Time, bool) {
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.FormatUint(uint64(pid), 10), "stat"))
	if err != nil {
		return time.Time{}, false
	}
	// The fields after the command name, which is in parentheses and may itself hold spaces or parentheses, start at the state, the third field; the start time is the twenty-second.
	s := string(stat)
	fields := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
	if len(fields) < 20 {
		return time.Time{}, false
	}
	ticks, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	system, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}, false
	}
	for _, line := range strings.Split(string(system), "\n") {
		if rest, found := strings.CutPrefix(line, "btime "); found {
			boot, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
			if err != nil {
				return time.Time{}, false
			}
			return time.Unix(boot, 0).Add(time.Duration(ticks) * time.Second / clockTicks), true
		}
	}
	return time.Time{}, false
}

// portHeldByOtherAccount reports whether the socket listening on port belongs to another user of this computer, as it does when someone else signed in alongside this user has June running: loopback ports are shared by every user, so their June holds June's port for everyone, and this user's requests to it are refused, its token being another's. It reads the socket's owner from /proc/net/tcp and /proc/net/tcp6. Input: the port. Output: that user's name, or their uid when the name cannot be found; the program's name, which cannot be told here, since another user's processes cannot be looked into, so it is always ""; and false when the socket is this user's, or a system account's (see personUID), or there is none.
func portHeldByOtherAccount(port string) (account, program string, ok bool) {
	want, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return "", "", false
	}
	for _, file := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		table, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(table), "\n") {
			// sl, local_address as hex ADDR:PORT, rem_address, st (0A is LISTEN), the queues and timers, then the owner's uid.
			f := strings.Fields(line)
			if len(f) < 10 || f[3] != "0A" {
				continue
			}
			_, hexPort, found := strings.Cut(f[1], ":")
			if p, err := strconv.ParseUint(hexPort, 16, 16); !found || err != nil || p != want {
				continue
			}
			if f[7] == strconv.Itoa(os.Getuid()) || !personUID(f[7]) {
				return "", "", false
			}
			account = f[7]
			if u, err := user.LookupId(f[7]); err == nil {
				account = u.Username
			}
			return account, "", true
		}
	}
	return "", "", false
}

// personUID reports whether uid can be a person who signs in, and so whose June this could be. Root and the system accounts, such as docker-proxy for a container publishing the port or a system service, cannot; for them the ordinary message, about a program that is not answering as June, is the honest one. A uid with a login session of its own (/run/user/<uid>) is a person, as is one in the range useradd gives people by default (UID_MIN to UID_MAX in /etc/login.defs), which leaves out systemd's dynamic service users and nobody above it.
func personUID(uid string) bool {
	n, err := strconv.ParseUint(uid, 10, 32)
	if err != nil || n == 0 {
		return false
	}
	if _, err := os.Stat(filepath.Join("/run/user", uid)); err == nil {
		return true
	}
	return n >= 1000 && n <= 60000
}
