//go:build !linux

package hostruntimecmd

import (
	"context"
	"fmt"
	"io"
)

// NoSystemd is false off Linux: macOS and Windows always have their service manager.
var NoSystemd = func() bool { return false }

// ForegroundHint is unused off Linux; it exists so callers need no build tags.
func ForegroundHint(string) string { return "" }

// RunForeground is the Linux container supervisor only.
func RunForeground(_ context.Context, _, stderr io.Writer) int {
	fmt.Fprintln(stderr, "pb daemon --foreground is only available on Linux")
	return 2
}
