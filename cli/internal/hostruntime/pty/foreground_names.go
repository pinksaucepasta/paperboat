package pty

import "strings"

// handlesInterruptItself reports whether a program owns a prompt and handles Ctrl-C itself: it cancels what it is running and stays at its
// prompt. The Windows fallback that terminates a foreground program which ignored the pipe ETX (cmd.exe running ping -t) must leave
// these alone, or one Ctrl-C ends a PowerShell started from cmd.exe instead of the command running in it.
func handlesInterruptItself(image string) bool {
	switch strings.ToLower(image) {
	case "cmd.exe", "powershell.exe", "pwsh.exe":
		return true
	}
	return false
}
