//go:build windows

package session

// HoldInput does nothing on Windows: a console delivers Enter as CR whatever its line mode.
func HoldInput() (restore func()) { return func() {} }
