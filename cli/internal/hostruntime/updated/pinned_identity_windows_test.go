//go:build windows

package updated

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowsRollbackNativeIdentityUsesOldPinNotFeatureVersion(t *testing.T) {
	pin := filepath.Join(`C:\Program Files\Paperboat`, "releases", "versions", "2026.10.08.10", "pb.exe")
	if got := windowsPinnedTargetVersion(windowsServiceTarget{Executable: pin}, "2026.10.08.20"); got != "2026.10.08.10" {
		t.Fatalf("rollback expected native pin version, got %q", got)
	}
	if got := windowsPinnedTargetVersion(windowsServiceTarget{Executable: `C:\Program Files\Paperboat\bin\pb.exe`}, "2026.10.08.20"); got != "2026.10.08.20" {
		t.Fatalf("initial canonical owner version=%q", got)
	}
	custom := filepath.Join(`C:\Program Files\Paperboat`, "releases", "versions", "custom-"+strings.Repeat("a", 64), "pb.exe")
	if got := windowsPinnedTargetVersion(windowsServiceTarget{Executable: custom}, "dev-update.1"); got != "dev-update.1" {
		t.Fatalf("custom content pin became display version: %q", got)
	}

}
