package util

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"github.com/joho/godotenv"
)

// startupEnviron is the environment this process was handed when it started. Package variables are set before main runs, so this is taken before cmd reads June's env files into the environment, and it is what still tells a key the user exported apart from one June loaded from its own file once both sit in os.Environ.
var startupEnviron = os.Environ()

// envKey is name as the environment matches it: Windows variable names ignore case, so a user variable typed "Gemini_Api_Key" is the GEMINI_API_KEY os.Getenv reads, and os.Setenv of either replaces the other. godotenv is the exception: it decides a variable is already set by its exact spelling (see StartupEnvVars).
func envKey(name string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(name)
	}
	return name
}

// StartupEnvVars is the environment the process was started with, before any env file was read, as a fresh map keyed by each name exactly as it was spelled. On Windows the environment also holds entries like "=C:=C:\\", whose name is empty; they are left out.
// Indexed directly, it answers what godotenv asks before it loads a file's line: a variable set under that exact spelling, empty or not, is never overwritten, while on Windows one spelled "Gemini_Api_Key" is, by a file's GEMINI_API_KEY. Looked up with LookupEnvVar, it answers what os.Getenv will see when no file sets the name at all.
func StartupEnvVars() map[string]string {
	env := map[string]string{}
	for _, kv := range startupEnviron {
		if name, value, ok := strings.Cut(kv, "="); ok && name != "" {
			env[name] = value
		}
	}
	return env
}

// StartupEnviron is a copy of the environment the process was started with, before any env file was read, for a program June starts that may in turn start June: handed os.Environ() instead, it would pass on every key June loaded from its env files as if the user had set it, and the June it starts would never read those files' keys afresh. On Windows its PATH also carries any folder added to Windows' lasting Path since (see RefreshPath), so a June restarted after a command line was installed finds it.
func StartupEnviron() []string {
	return withLastingPath(slices.Clone(startupEnviron))
}

// ReadEnvFile reads the variables an env file sets, parsed the way godotenv parses it when June loads it at start. Input: the file. Output: the variables, an empty map and no error when the file does not exist, and the parser's error when it is there but will not parse.
func ReadEnvFile(path string) (map[string]string, error) {
	vars, err := godotenv.Read(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	}
	return vars, err
}

// LookupEnvVar finds name among the variables ReadEnvFile or StartupEnvVars returned, matching names the way the environment does, ignoring case on Windows. Input: the variables and the name. Output: the value, and whether they set it at all, to "" included.
func LookupEnvVar(vars map[string]string, name string) (string, bool) {
	if v, ok := vars[name]; ok {
		return v, true
	}
	for k, v := range vars {
		if envKey(k) == envKey(name) {
			return v, true
		}
	}
	return "", false
}

// envName is what a variable name may be. godotenv takes dots and any letter too, but a name June writes is always a plain shell name.
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// bareEnvValue is a value godotenv reads back unchanged when written with no quotes: no '$', which it would expand, no '#' or space, which can start a comment or be trimmed, and no quote or backslash.
var bareEnvValue = regexp.MustCompile(`^[A-Za-z0-9._\-+/=:@]*$`)

// UpsertEnvFile sets one variable in an env file and leaves every other line as it was. Every line that sets name, with or without "export" (and on Windows in any case, as the environment matches it), is dropped and one "name=value" line takes the place of the first; the file is created when missing. An empty value removes name instead of writing it empty, because godotenv never overwrites a variable that is already set, and an empty one counts as set: it would hide the same name in any file read after this one.
// The file is replaced atomically and private to the user (mode 0600 on Linux, a DACL naming only the user and SYSTEM on Windows, set before the key is written; see WriteFilePrivate), so a crash mid-write leaves the old key or the new one and never half of either, and a key is never readable by other users for the moment between writing and restricting it. CRLF line endings are kept when the file already uses them.
// Input: the file, the variable's name, and its value. Output: an error when the name is not a plain shell name, the value has a line break or both kinds of quote and so cannot be written on one line godotenv reads back, or the file cannot be read or written.
func UpsertEnvFile(path, name, value string) error {
	if !envName.MatchString(name) {
		return fmt.Errorf("%q is not a variable name an env file can hold", name)
	}
	line, err := envLine(name, value)
	if err != nil {
		return err
	}
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	text := string(old)
	eol := "\n"
	if strings.Contains(text, "\r\n") {
		eol = "\r\n"
	}
	var out []string
	placed := false
	if text != "" {
		for _, l := range strings.Split(strings.TrimRight(strings.ReplaceAll(text, "\r\n", "\n"), "\n"), "\n") {
			if envKey(envLineName(l)) != envKey(name) {
				out = append(out, l)
				continue
			}
			if !placed && line != "" {
				out = append(out, line)
			}
			placed = true
		}
	}
	if !placed && line != "" {
		out = append(out, line)
	}
	data := ""
	if len(out) > 0 {
		data = strings.Join(out, eol) + eol
	}
	if err := MkdirPrivate(filepath.Dir(path)); err != nil {
		return err
	}
	return WriteFilePrivate(path, []byte(data))
}

// envLine is the line that sets name to value, "" when value is empty and the name is to be removed. A value that is not safe bare is single-quoted, which godotenv reads literally with no expansion and no escapes.
func envLine(name, value string) (string, error) {
	switch {
	case value == "":
		return "", nil
	case strings.ContainsAny(value, "\r\n"):
		return "", fmt.Errorf("the value for %s has a line break, which an env file cannot hold on one line", name)
	case bareEnvValue.MatchString(value):
		return name + "=" + value, nil
	case !strings.Contains(value, "'"):
		return name + "='" + value + "'", nil
	default:
		return "", fmt.Errorf("the value for %s has a single quote and characters that need quoting, which an env file cannot hold as written", name)
	}
}

// envLineName is the variable a line of an env file sets, read the way godotenv reads it: leading space and an "export " prefix are skipped, and the name runs to the first '=' or ':'. Output: "" for a blank line, a comment, or a line that sets nothing.
func envLineName(line string) string {
	s := strings.TrimLeft(line, " \t")
	if rest, ok := strings.CutPrefix(s, "export"); ok && rest != "" && (rest[0] == ' ' || rest[0] == '\t') {
		s = strings.TrimLeft(rest, " \t")
	}
	if s == "" || s[0] == '#' {
		return ""
	}
	end := strings.IndexAny(s, "=:")
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(s[:end])
}
