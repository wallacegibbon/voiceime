package config

import (
	"fmt"
	"strings"
)

// The settings file is written, but it is also read by a person, and it is the one place a
// setup can carry a note. An endpoint that was switched away from is commented out rather than
// deleted, a number carries the reason next to it, and the order of the keys is the order that
// story is told in. A document generated from a parsed struct loses all of that, so writing the
// settings edits the file that is there: each key this program owns is replaced on its own line
// and everything else is copied through.

// setKeyInDocument replaces the value of one key in an existing TOML document and returns the
// document. A key on a line of its own is the whole shape this file has: flat keys, one value
// each. Anything else in the document - a full-line comment, a commented-out alternative, a
// blank line, the order - is returned exactly as it came.
func setKeyInDocument(document, key, value string) string {
	lines := splitLines(document)
	for index, line := range lines {
		lead, name, rest, found := splitAssignment(line)
		if !found || name != key {
			continue
		}
		// A value written across several lines is not a shape this file writes, and replacing
		// the first line of one leaves the rest behind where the parser will say so.
		lines[index] = lead + key + " = " + value + sameLineComment(rest)
		return joinLines(lines) + "\n"
	}
	// The file does not carry this key. It is added at the end rather than inserted somewhere,
	// because "somewhere" is where the person reading the file would find it moved.
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > 0 {
		if _, _, _, isAssignment := splitAssignment(lines[len(lines)-1]); !isAssignment {
			lines = append(lines, "")
		}
	}
	return joinLines(append(lines, key+" = "+value)) + "\n"
}

// splitAssignment reads one line into what comes before the key, the key, and what follows the
// `=`. A line whose key is commented out is not an assignment: `#model = ""` is a note, and
// rewriting through it is how a commented-out configuration silently comes back.
func splitAssignment(line string) (lead, key, rest string, found bool) {
	trimmed := strings.TrimLeft(line, " \t")
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", "", "", false
	}
	cut := strings.Index(trimmed, "=")
	if cut < 0 {
		return "", "", "", false
	}
	key = strings.TrimRight(trimmed[:cut], " \t")
	if key == "" {
		return "", "", "", false
	}
	return line[:len(line)-len(trimmed)], key, trimmed[cut+1:], true
}

// sameLineComment keeps the comment that followed a value. It starts at the first `#` outside a
// quoted string, which is the only place one can start in a value of the shape this file holds.
func sameLineComment(rest string) string {
	quoted := false
	for index := 0; index < len(rest); index++ {
		switch rest[index] {
		case '\\':
			if quoted && index+1 < len(rest) {
				index++
			}
		case '"':
			quoted = !quoted
		case '#':
			if quoted {
				continue
			}
			comment := strings.TrimRight(rest[index:], " \t\r")
			if comment == "#" {
				return ""
			}
			return " " + comment
		}
	}
	return ""
}

func splitLines(document string) []string {
	if document == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(document, "\n"), "\n")
}

func joinLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// encodeValue renders a value the way this file writes one: a quoted string with the escapes
// TOML needs and no other changes to what is between the quotes, a number or a truth value on
// its own.
func encodeValue(value any) string {
	switch typed := value.(type) {
	case string:
		var quoted strings.Builder
		quoted.WriteByte('"')
		for _, character := range typed {
			switch character {
			case '"':
				quoted.WriteString(`\"`)
			case '\\':
				quoted.WriteString(`\\`)
			case '\n':
				quoted.WriteString(`\n`)
			case '\r':
				quoted.WriteString(`\r`)
			case '\t':
				quoted.WriteString(`\t`)
			default:
				if character < 0x20 {
					fmt.Fprintf(&quoted, `\u%04x`, character)
					continue
				}
				quoted.WriteRune(character)
			}
		}
		quoted.WriteByte('"')
		return quoted.String()
	default:
		return fmt.Sprintf("%v", value)
	}
}
