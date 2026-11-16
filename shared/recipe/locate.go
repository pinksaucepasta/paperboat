package recipe

import (
	"strconv"
	"strings"
)

// tomlLocator maps a path (["preview", "0", "port"]) to its line and columns by scanning the
// source line by line, since the TOML decoder does not export key positions.
// shortcut: keys inside multi-line strings or inline tables are not indexed; such errors fall
// back to the section header or line 1, upgrade if recipes grow nested values.
type tomlLocator struct {
	keys    map[string][3]int // "table\x00key" → line, key col, value col
	headers map[string]int    // "preview#0" or "env" → line
}

func newTOMLLocator(src []byte) *tomlLocator {
	l := &tomlLocator{keys: map[string][3]int{}, headers: map[string]int{}}
	counts := map[string]int{}
	table := ""
	for i, line := range strings.Split(string(src), "\n") {
		n := i + 1
		trimmed := strings.TrimSpace(line)
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		switch {
		case strings.HasPrefix(trimmed, "[["):
			name := strings.TrimSpace(strings.Trim(strings.SplitN(trimmed, "]]", 2)[0], "[ "))
			table = name + "#" + strconv.Itoa(counts[name])
			counts[name]++
			l.headers[table] = n
		case strings.HasPrefix(trimmed, "["):
			table = strings.TrimSpace(strings.Trim(strings.SplitN(trimmed, "]", 2)[0], "[ "))
			l.headers[table] = n
		case strings.Contains(trimmed, "=") && !strings.HasPrefix(trimmed, "#"):
			eq := strings.Index(line, "=")
			key := strings.Trim(strings.TrimSpace(line[:eq]), `"'`)
			value := eq + 1 + len(line[eq+1:]) - len(strings.TrimLeft(line[eq+1:], " \t"))
			id := table + "\x00" + key
			if _, seen := l.keys[id]; !seen {
				l.keys[id] = [3]int{n, indent + 1, value + 1}
			}
		}
	}
	return l
}

func (l *tomlLocator) key(path []string) (int, int, int) {
	table, rest := "", path
	if len(path) >= 2 {
		if _, err := strconv.Atoi(path[1]); err == nil {
			table, rest = path[0]+"#"+path[1], path[2:]
		} else if _, ok := l.headers[path[0]]; ok {
			table, rest = path[0], path[1:]
		}
	} else if len(path) == 1 {
		if line, ok := l.headers[path[0]]; ok {
			return line, 1, 0
		}
		if line, ok := l.headers[path[0]+"#0"]; ok {
			return line, 1, 0
		}
	}
	if len(rest) == 0 {
		return l.headers[table], 1, 0
	}
	if p, ok := l.keys[table+"\x00"+rest[0]]; ok {
		return p[0], p[1], p[2]
	}
	if line, ok := l.headers[table]; ok {
		return line, 1, 0
	}
	return 1, 1, 0
}
