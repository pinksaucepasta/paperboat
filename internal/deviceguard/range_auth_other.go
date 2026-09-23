//go:build !linux && !darwin && !windows

package deviceguard

func canReconfigureRange(controlConn, string) bool { return false }
