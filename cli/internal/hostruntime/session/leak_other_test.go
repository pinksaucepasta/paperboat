//go:build !darwin && !linux

package session

import "testing"

// Windows keeps the shutdown but not the goroutine check: its native sessions have their own cleanup tests.
func registerLeakCheck(*testing.T) {}
