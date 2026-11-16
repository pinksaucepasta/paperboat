package transfername

import (
	"strings"
	"testing"
)

func TestFolderPathsAreHeldToTheRulesOfASingleFile(t *testing.T) {
	for _, rel := range []string{"CON.txt", "docs/CON", "a/b/trail.", "a/b:c", "x/sp /y", strings.Repeat("d/", 600) + "f"} {
		if FolderPathRefusal("windows", rel) == "" {
			t.Errorf("windows accepted %q", rel)
		}
	}
	for _, rel := range []string{"", "a//b", "a/../b", "a/\x00"} {
		if FolderPathRefusal("linux", rel) == "" {
			t.Errorf("linux accepted %q", rel)
		}
	}
	for _, rel := range []string{"CON.txt", "docs/café/ünï.txt", "a b/c d", "emptydir"} {
		if reason := FolderPathRefusal("linux", rel); reason != "" {
			t.Errorf("linux refused %q: %s", rel, reason)
		}
	}
	if reason := FolderPathRefusal("windows", "docs/readme.md"); reason != "" {
		t.Errorf("windows refused a plain path: %s", reason)
	}
}

func TestFolderCollisionOnlyWhereNamesIgnoreCase(t *testing.T) {
	paths := []string{"a", "a/Readme.txt", "a/readme.TXT"}
	for _, platform := range []string{"windows", "darwin"} {
		if reason := FolderCollision(platform, paths); !strings.Contains(reason, "Readme.txt") || !strings.Contains(reason, "readme.TXT") {
			t.Errorf("%s: collision not named: %q", platform, reason)
		}
	}
	if reason := FolderCollision("linux", paths); reason != "" {
		t.Errorf("linux is case-sensitive: %q", reason)
	}
}
