package util

import "golang.org/x/sys/windows/registry"

// PersistentEnvScope says where Windows keeps name as a lasting environment variable, the kind set in "Edit environment variables" that every program started from Explorer inherits. Input: the variable's name. Output: "user" or "system", or "" when it is in neither and so came from whatever started this process.
func PersistentEnvScope(name string) string {
	scopes := []struct {
		root  registry.Key
		path  string
		scope string
	}{
		{registry.CURRENT_USER, `Environment`, "user"},
		{registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\Session Manager\Environment`, "system"},
	}
	for _, s := range scopes {
		k, err := registry.OpenKey(s.root, s.path, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		// Registry value names ignore case, as the environment does.
		_, _, err = k.GetValue(name, nil)
		k.Close()
		if err == nil {
			return s.scope
		}
	}
	return ""
}
