package gnome

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// gsettings, and the two shapes of value this package reads: a list of strings and a
// list of (type, id) pairs. A GVariant parser would be a dependency for two formats.

func run(name string, args ...string) (string, error) {
	command := exec.Command(name, args...)
	out, err := command.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(exit.Stderr) > 0 {
			return "", fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), strings.TrimSpace(string(exit.Stderr)))
		}
		return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

func run0(name string, args ...string) error {
	_, err := run(name, args...)
	return err
}

func quote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `\'`) + "'"
}

// parseList pulls the quoted strings out of a GVariant value such as
// `[('xkb', 'us'), ('ibus', 'pinyin')]` or `@as []`, in order.
func parseList(raw string) []string {
	raw = strings.TrimSpace(raw)
	for _, prefix := range []string{"@as ", "@a(ss) ", "@a(sss) "} {
		raw = strings.TrimSpace(strings.TrimPrefix(raw, prefix))
	}
	var out []string
	for {
		start := strings.Index(raw, "'")
		if start < 0 {
			return out
		}
		end := strings.Index(raw[start+1:], "'")
		if end < 0 {
			return out
		}
		out = append(out, raw[start+1:start+1+end])
		raw = raw[start+1+end+1:]
	}
}
