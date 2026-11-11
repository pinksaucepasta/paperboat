package main

import (
	"fmt"
	"strings"
)

// entry is one NAME=value assignment of a compose-style env file. start and end are the byte span of the
// whole assignment (NAME through the closing quote or end of line, without the line break).
type entry struct {
	name       string
	value      string
	start, end int
}

// parseEnv reads unquoted, 'single-quoted' (multi-line, literal) and "double-quoted" (multi-line, no escapes)
// values at column 0. Everything else (comments, blank lines) is skipped.
func parseEnv(text string) ([]entry, error) {
	var entries []entry
	for pos := 0; pos < len(text); {
		lineEnd := strings.IndexByte(text[pos:], '\n')
		if lineEnd < 0 {
			lineEnd = len(text)
		} else {
			lineEnd += pos
		}
		name, ok := assignmentName(text[pos:lineEnd])
		if !ok {
			pos = lineEnd + 1
			continue
		}
		valueStart := pos + len(name) + 1
		e := entry{name: name, start: pos}
		if valueStart < len(text) && (text[valueStart] == '\'' || text[valueStart] == '"') {
			quote := text[valueStart]
			closing := strings.IndexByte(text[valueStart+1:], quote)
			if closing < 0 {
				return nil, fmt.Errorf("env file: unterminated quote in the value of %s", name)
			}
			e.value = text[valueStart+1 : valueStart+1+closing]
			if quote == '"' && strings.ContainsRune(e.value, '\\') {
				return nil, fmt.Errorf("env file: %s uses a backslash inside double quotes, which this tool does not interpret", name)
			}
			e.end = valueStart + 1 + closing + 1
			// the closing quote must end its line, else the file is not what this parser assumes
			if e.end < len(text) && text[e.end] != '\n' && text[e.end] != '\r' {
				return nil, fmt.Errorf("env file: text after the closing quote of %s", name)
			}
		} else {
			e.value, e.end = strings.TrimRight(text[valueStart:lineEnd], "\r"), lineEnd
		}
		entries = append(entries, e)
		pos = e.end + 1
	}
	return entries, nil
}

func assignmentName(line string) (string, bool) {
	eq := strings.IndexByte(line, '=')
	if eq <= 0 {
		return "", false
	}
	for i, r := range line[:eq] {
		if !(r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || i > 0 && r >= '0' && r <= '9') {
			return "", false
		}
	}
	return line[:eq], true
}

// lookup returns the single entry called name.
func lookup(entries []entry, name string) (entry, error) {
	var found []entry
	for _, e := range entries {
		if e.name == name {
			found = append(found, e)
		}
	}
	if len(found) != 1 {
		return entry{}, fmt.Errorf("env file: want exactly one %s, found %d", name, len(found))
	}
	return found[0], nil
}

// replaceValue swaps the value of the named entry for a single-quoted one and leaves every other byte alone.
func replaceValue(text string, e entry, value string) (string, error) {
	if strings.ContainsAny(value, "'\x00") {
		return "", fmt.Errorf("env file: new value for %s cannot be single-quoted", e.name)
	}
	return text[:e.start] + e.name + "='" + value + "'" + text[e.end:], nil
}
