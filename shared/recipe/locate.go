package recipe

import (
	"bytes"
	"strconv"
	"strings"

	"github.com/tailscale/hujson"
)

// locator maps a path (["preview", "0", "port"]) to its line and columns in the source.
type locator struct {
	src       []byte
	positions map[string][2]int // path → offset of the key (or list item), offset of its value
	duplicate []duplicate       // keys that appear more than once
}

type duplicate struct {
	path   []string
	offset int
}

func newLocator(src []byte, v hujson.Value) *locator {
	v.UpdateOffsets()
	l := &locator{src: src, positions: map[string][2]int{}}
	l.walk(nil, v, v.StartOffset)
	return l
}

func (l *locator) walk(path []string, v hujson.Value, keyAt int) {
	l.positions[strings.Join(path, "\x00")] = [2]int{keyAt, v.StartOffset}
	switch c := v.Value.(type) {
	case *hujson.Object:
		seen := map[string]bool{}
		for _, m := range c.Members {
			name := m.Name.Value.(hujson.Literal).String()
			child := append(append([]string(nil), path...), name)
			if seen[name] {
				l.duplicate = append(l.duplicate, duplicate{child, m.Name.StartOffset})
				continue
			}
			seen[name] = true
			l.walk(child, m.Value, m.Name.StartOffset)
		}
	case *hujson.Array:
		for i, e := range c.Elements {
			l.walk(append(append([]string(nil), path...), strconv.Itoa(i)), e, e.StartOffset)
		}
	}
}

// lineCol turns a byte offset into a 1-based line and column.
func (l *locator) lineCol(offset int) (int, int) {
	offset = min(offset, len(l.src))
	line := 1 + bytes.Count(l.src[:offset], []byte("\n"))
	return line, 1 + offset - (bytes.LastIndexByte(l.src[:offset], '\n') + 1)
}

// key returns the line, the column of the key and the column of its value (0 when it is on another line).
func (l *locator) key(path []string) (line, keyCol, valueCol int) {
	p, ok := l.positions[strings.Join(path, "\x00")]
	if !ok {
		return 1, 1, 0
	}
	line, keyCol = l.lineCol(p[0])
	valueLine, valueCol := l.lineCol(p[1])
	if valueLine != line {
		valueCol = 0
	}
	return line, keyCol, valueCol
}
