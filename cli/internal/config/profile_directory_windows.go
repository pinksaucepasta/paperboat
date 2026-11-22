//go:build windows

package config

// Apply the inheritable owner-only descriptor to each profile namespace before
// creating locks or atomically replacing profile files.
func ensureProfileDirectory(path string) error {
	sddl, err := currentUserDirectorySDDL()
	if err != nil {
		return err
	}
	return ensureDPAPIDirectory(path, sddl)
}
