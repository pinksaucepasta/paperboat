//go:build !windows

package inbox

import (
	"os"
	"testing"
)

// requirePrivateFile fails unless the delivered file is owner-only.
func requirePrivateFile(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("%s: %v %v", path, info, err)
	}
}
