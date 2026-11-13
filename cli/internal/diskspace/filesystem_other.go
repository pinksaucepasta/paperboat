//go:build !linux && !darwin && !windows

package diskspace

func fileSystem(string) (string, error) { return "other", nil }
