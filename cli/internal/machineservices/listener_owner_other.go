//go:build !windows && !linux && !darwin

package machineservices

func listenerOwner(uint16) (string, int, bool) { return "", 0, false }
