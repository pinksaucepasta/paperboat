//go:build !unix && !windows

package configsync

import "path/filepath"

func mappedPlatformPathsEqual(a, b string) bool { return a == b }

func checkMappedPlatformPath(string) error { return nil }
func lockMappedParents(target string, create bool) (func(), error) {
	if create {
		if err := ensurePrivateParent(filepath.VolumeName(target)+string(filepath.Separator), filepath.Dir(target)); err != nil {
			return nil, err
		}
	}
	return func() {}, checkSafeAbsolutePath(target)
}
