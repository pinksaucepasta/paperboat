//go:build !linux && !darwin && !windows

package machineguard

func canReconfigureRange(controlConn, string) bool { return false }
