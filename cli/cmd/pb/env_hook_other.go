//go:build !windows

package main

import "path/filepath"

const envHookConsole = "/dev/tty"

// envHookDocumentsDir is where PowerShell 7 keeps profiles on Linux and macOS is
// elsewhere; this only serves tests and the hook line's Windows layout.
func envHookDocumentsDir(home string) string { return filepath.Join(home, "Documents") }
