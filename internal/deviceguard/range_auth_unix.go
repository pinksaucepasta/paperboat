//go:build linux || darwin

package deviceguard

func canReconfigureRange(_ controlConn, uid string) bool { return uid == "0" }
