// Package machinename defines the portable name contract used by enrollment.
package machinename

import (
	"errors"
	"strings"
)

var ErrInvalid = errors.New("machine name must be 1-63 ASCII letters, digits or hyphens, not starting or ending with a hyphen; e.g. zoe-build")

// Validate accepts the intersection of hostname rules used by Linux, macOS,
// and Windows. A Paperboat machine name is one DNS label, not a command-line
// fragment, path, flag, or qualified hostname.
func Validate(value string) error {
	if value == "" || strings.TrimSpace(value) != value || len(value) > 63 || value[0] == '-' || value[len(value)-1] == '-' {
		return ErrInvalid
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			continue
		}
		return ErrInvalid
	}
	return nil
}
