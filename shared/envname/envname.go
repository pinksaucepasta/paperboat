// Package envname is the one rule for an ENV variable name: ASCII letters, digits and underscores, not
// starting with a digit, 1 to 128 bytes, and not a name that changes how a process starts. The server, the
// pb CLI, the browser vault engine and the host runtime all check with it; the console repeats the same rule
// in TypeScript and tests it against testdata/names.json, which this package's test also reads.
package envname

import (
	"errors"
	"strings"
)

// MaxBytes is the longest variable name.
const MaxBytes = 128

// Check says why name is not a usable ENV variable name, in words a person can act on; nil when it is.
func Check(name string) error {
	if name == "" || len(name) > MaxBytes {
		return errors.New("environment variable name must be 1-128 characters")
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		letter := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_'
		if i == 0 && !letter {
			return errors.New("environment variable name must start with a letter or underscore")
		}
		if !letter && (c < '0' || c > '9') {
			return errors.New("environment variable name may contain only letters, numbers, and underscores")
		}
	}
	upper := strings.ToUpper(name)
	if strings.HasPrefix(upper, "PAPERBOAT_") || strings.HasPrefix(upper, "LD_") || strings.HasPrefix(upper, "DYLD_") {
		return errors.New("environment variable name is reserved")
	}
	switch upper {
	case "NODE_OPTIONS", "PYTHONPATH", "PYTHONHOME", "GOTRACEBACK":
		return errors.New("environment variable name is reserved")
	}
	return nil
}

// Valid reports whether Check accepts name.
func Valid(name string) bool { return Check(name) == nil }
