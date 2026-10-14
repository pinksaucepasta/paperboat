//go:build linux || darwin

package machineguard

func canReconfigureRange(_ controlConn, uid string) bool { return uid == "0" }
