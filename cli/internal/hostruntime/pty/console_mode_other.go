//go:build !windows

package pty

import "errors"

// The query exists so the hidden pb daemon __query-console-mode command builds everywhere; only Windows uses it.
const (
	ExitInputProcessed = 10
	ExitInputRaw       = 11
)

var QueryConsoleModeArgs = []string{"daemon", "__query-console-mode"}

func QueryConsoleInputProcessed() (bool, error) {
	return false, errors.New("the console mode query exists only on Windows")
}
