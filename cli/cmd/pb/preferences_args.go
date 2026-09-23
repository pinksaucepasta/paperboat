package main

import (
	"errors"
	"strings"
)

// splitPreferenceArgs supports quoting and escaping only. It never expands a
// variable, glob, command substitution, pipeline, or shell statement. Empty
// quoted arguments are significant and must survive an editor round trip.
func splitPreferenceArgs(input string) ([]string, error) {
	var args []string
	var word strings.Builder
	var quote rune
	started, escaped := false, false
	for _, r := range input {
		if escaped {
			if quote == '"' && r != '"' && r != '\\' && r != '$' && r != '`' && r != '\n' {
				word.WriteRune('\\')
			}
			if r != '\n' {
				word.WriteRune(r)
			}
			escaped = false
			continue
		}
		if quote == '\'' {
			if r == '\'' {
				quote = 0
			} else {
				word.WriteRune(r)
			}
			continue
		}
		if r == '\\' {
			escaped = true
			started = true
			continue
		}
		if quote == '"' {
			if r == '"' {
				quote = 0
			} else {
				word.WriteRune(r)
			}
			continue
		}
		switch r {
		case '\'', '"':
			quote = r
			started = true
		case ' ', '\t', '\r', '\n':
			if started {
				args = append(args, word.String())
				word.Reset()
				started = false
			}
		default:
			word.WriteRune(r)
			started = true
		}
	}
	if quote != 0 || escaped {
		return nil, errors.New("close the quote or complete the escape in the argument list")
	}
	if started {
		args = append(args, word.String())
	}
	return args, nil
}
