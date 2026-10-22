package shorttemp

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDirIsShortSymlinkFreeAndRemoved(t *testing.T) {
	var dir string
	t.Run("inner", func(t *testing.T) {
		dir = Dir(t)
		if real, err := filepath.EvalSymlinks(dir); err != nil || real != dir {
			t.Fatalf("dir %q is not its own real path: %q, %v", dir, real, err)
		}
		if runtime.GOOS != "windows" && len(dir) > 40 { // Windows has no unix socket path limit to protect
			t.Fatalf("dir %q is too long for socket paths", dir)
		}
	})
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("dir %q was not removed: %v", dir, err)
	}
}
