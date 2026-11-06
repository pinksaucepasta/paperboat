//go:build linux

package sessionowner

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// F14-04: the owner an update replaced was published minutes before the update, so it must retire
// once its last session closes; only a generation published after the active one is a staged candidate.
func TestStagingProtectsOnlyAGenerationPublishedAfterTheActiveOne(t *testing.T) {
	root := t.TempDir()
	c := Catalog{Root: root}
	publish := func(n uint64, age time.Duration) Descriptor {
		path := filepath.Join(root, generation(n)+".json")
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(-age)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
		return Descriptor{Generation: generation(n)}
	}
	replaced := publish(1, 3*time.Minute)
	active := publish(2, 2*time.Minute)
	candidate := publish(3, time.Minute)
	orphan := publish(4, 2*stagingWindow)
	for _, tc := range []struct {
		name string
		d    Descriptor
		want bool
	}{
		{"the generation the update replaced retires at once", replaced, false},
		{"a candidate published after the active one is protected", candidate, true},
		{"a crashed install's orphan retires once the window passed", orphan, false},
	} {
		if got := c.staging(tc.d, active); got != tc.want {
			t.Errorf("%s: staging=%v, want %v", tc.name, got, tc.want)
		}
	}
	if !c.staging(candidate, Descriptor{Generation: generation(9)}) {
		t.Error("an active descriptor that cannot be read keeps the conservative window")
	}
}
