//go:build unix

package configsync

func mappedPlatformPathsEqual(a, b string) bool { return a == b }
