//go:build linux || darwin

package hostinstall

func secureUpdateState(path string, request Request) error { return secureRootDirectory(path, 0700) }
