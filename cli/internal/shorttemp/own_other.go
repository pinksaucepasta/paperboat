//go:build !windows

package shorttemp

import "testing"

// OwnByUser is a no-op off Windows: the creating user always owns what it creates.
func OwnByUser(testing.TB, string) {}

// MachineDir is Dir off Windows.
func MachineDir(t testing.TB) string { t.Helper(); return Dir(t) }
