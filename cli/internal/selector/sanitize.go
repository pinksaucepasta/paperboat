package selector

import (
	"github.com/charmbracelet/x/ansi"
	"unicode"
)

// SanitizeText removes terminal control characters from text that is rendered
// by the selector. Newlines are deliberately removed here because selector
// fields occupy one row; headers use sanitizeHeader when multiline text is
// part of their static layout.
func SanitizeText(value string) string {
	return sanitizeTerminalText(value, false)
}

func sanitizeHeader(value string) string {
	return sanitizeTerminalText(value, true)
}

func sanitizeTerminalText(value string, preserveNewlines bool) string {
	value = ansi.Strip(value)
	clean := make([]rune, 0, len(value))
	for _, r := range value {
		if r == '\n' && preserveNewlines {
			clean = append(clean, r)
			continue
		}
		if unicode.IsControl(r) {
			continue
		}
		clean = append(clean, r)
	}
	return string(clean)
}
