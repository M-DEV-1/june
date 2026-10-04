package util

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// lastingPathKeys are where Windows keeps the Path every program started from Explorer is given: the machine's first, then the user's, the order Windows joins them in.
var lastingPathKeys = []struct {
	root registry.Key
	path string
}{
	{registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\Session Manager\Environment`},
	{registry.CURRENT_USER, `Environment`},
}

// RefreshPath adds to this process's PATH every folder Windows' lasting Path variables name that it does not have yet. A process keeps the PATH it was started with, so a command line installed while June runs (Claude Code or Antigravity during first-run setup, whose installers add their folder to the user's Path) was not found by June until it was quit and opened again: the brain row stayed "not installed" and every ask to it failed with "executable file not found". Folders are only ever added, at the end, so nothing June was started with is lost or reordered. Output: none; a key that cannot be read adds nothing.
func RefreshPath() {
	current := os.Getenv("PATH")
	if merged := mergePath(current, lastingPath()); merged != current {
		os.Setenv("PATH", merged)
	}
}

// lastingPath is the machine's Path followed by the user's, as the registry holds them now, with %VARIABLE% references expanded the way Explorer expands them.
func lastingPath() []string {
	var dirs []string
	for _, k := range lastingPathKeys {
		key, err := registry.OpenKey(k.root, k.path, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		value, kind, err := key.GetStringValue("Path")
		key.Close()
		if err != nil {
			continue
		}
		if kind == registry.EXPAND_SZ {
			if expanded, err := registry.ExpandString(value); err == nil {
				value = expanded
			}
		}
		dirs = append(dirs, filepath.SplitList(value)...)
	}
	return dirs
}

// mergePath appends to path each of dirs it does not already name. Folders are compared as Windows compares them, ignoring case and a trailing backslash. Input: a PATH value and the folders to add. Output: the PATH with any new ones on the end.
func mergePath(path string, dirs []string) string {
	key := func(dir string) string { return strings.ToLower(strings.TrimRight(strings.TrimSpace(dir), `\/`)) }
	seen := map[string]bool{}
	for _, dir := range filepath.SplitList(path) {
		seen[key(dir)] = true
	}
	for _, dir := range dirs {
		k := key(dir)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		if path != "" && !strings.HasSuffix(path, string(os.PathListSeparator)) {
			path += string(os.PathListSeparator)
		}
		path += strings.TrimSpace(dir)
	}
	return path
}

// withLastingPath is env with its PATH carrying every folder Windows' lasting Path variables name, for a June that this one starts to stand in for it: a restart handed the startup PATH on, so a command line installed since June first started stayed out of reach across every restart June made on its own account. Input: an environment in os.Environ's form. Output: the same environment with the PATH entry extended, or one added when it had none.
func withLastingPath(env []string) []string {
	dirs := lastingPath()
	for i, kv := range env {
		name, value, ok := strings.Cut(kv, "=")
		if ok && strings.EqualFold(name, "PATH") {
			env[i] = name + "=" + mergePath(value, dirs)
			return env
		}
	}
	if merged := mergePath("", dirs); merged != "" {
		env = append(env, "Path="+merged)
	}
	return env
}
