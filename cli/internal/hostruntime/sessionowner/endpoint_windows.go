//go:build windows

package sessionowner

// Windows creates the protected named pipe when its native owner starts; there
// is no filesystem directory to recreate after retirement.
func PrepareNativeEndpoint(d Descriptor) error { return d.Validate() }
