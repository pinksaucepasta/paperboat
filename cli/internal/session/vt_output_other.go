//go:build !windows

package session

// EnableVirtualTerminalOutput is a no-op: Unix terminals interpret VT sequences already.
func EnableVirtualTerminalOutput() (restore func()) { return func() {} }
