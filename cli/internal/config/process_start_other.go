//go:build !linux && !darwin && !windows

package config

import "time"

// processStartedAfter is unknown on this platform; the PID is trusted.
func processStartedAfter(int, time.Time) bool { return false }
