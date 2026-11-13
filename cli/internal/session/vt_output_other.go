//go:build !windows

package session

// PrepareConsoleOutput is a no-op: Unix terminals interpret VT sequences already.
func PrepareConsoleOutput() (restore func()) { return func() {} }
