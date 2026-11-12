//go:build !windows

package diskspace

func fullPlatform(error) bool { return false }
