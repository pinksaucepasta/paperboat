// Package sessionname defines what a person may call a terminal session.
package sessionname

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

const MaxRunes = 64

var ErrInvalid = errors.New("a session name is 1-64 characters without control characters or surrounding spaces; \"default\" is reserved")

// Validate accepts any text a person can type: spaces and non-ASCII are fine. A name is only ever an argument (a session is
// selected by `--session NAME` or a positional argument, never parsed out of a longer string), so unlike a machine name it
// needs no character set. Like tmux (tmux.c check_name) it requires valid UTF-8; unlike a name shown on one line it also
// refuses control, format and line-separator characters, which would break a terminal or a log line.
func Validate(name string) error {
	if name == "default" || name != strings.TrimSpace(name) || !utf8.ValidString(name) {
		return ErrInvalid
	}
	if n := utf8.RuneCountInString(name); n < 1 || n > MaxRunes {
		return ErrInvalid
	}
	for _, r := range name {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
			return ErrInvalid
		}
	}
	return nil
}
