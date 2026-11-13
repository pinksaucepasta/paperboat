package containers

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveRenamedRuntimeRemovesOnlyOwnedContainer(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(map[bool]string{false: "owned", true: "foreign"}[foreign], func(t *testing.T) {
			dir := t.TempDir()
			owner := installationOwner(dir)
			if foreign {
				owner = "someone-else"
			}
			bin := t.TempDir()
			log := filepath.Join(bin, "removed")
			t.Setenv("FIXTURE_OWNER", owner)
			t.Setenv("FIXTURE_LOG", log)
			script := `#!/bin/sh
case "$1" in
 container) echo present ;;
 inspect) printf '{"owner":"%s","running":false,"image_id":"fixture"}\n' "$FIXTURE_OWNER" ;;
 rm) printf '%s\n' "$2" > "$FIXTURE_LOG" ;;
 *) exit 1 ;;
esac
`
			if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			err := RemoveRenamedRuntime(t.Context(), dir)
			if (err != nil) != foreign {
				t.Fatalf("cleanup error = %v", err)
			}
			removed, readErr := os.ReadFile(log)
			if foreign {
				if !os.IsNotExist(readErr) {
					t.Fatal("foreign container was removed")
				}
			} else if readErr != nil || string(removed) != "paperboat-pbh-"+installationOwner(dir)+"-tunnel\n" {
				t.Fatalf("former container was not removed: %q, %v", removed, readErr)
			}
		})
	}
}
