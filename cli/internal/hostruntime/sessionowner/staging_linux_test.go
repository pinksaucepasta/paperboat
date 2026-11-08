//go:build linux

package sessionowner

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
)

// F14-04: the owner an update replaced was published minutes before the update, so it must retire
// once its last session closes; only a generation published after the active one is a staged candidate.
func TestStagingProtectsOnlyAGenerationPublishedAfterTheActiveOne(t *testing.T) {
	root := t.TempDir()
	c := Catalog{Root: root}
	publish := func(n uint64, age time.Duration, sha string) Descriptor {
		path := filepath.Join(root, generation(n)+".json")
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(-age)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
		return Descriptor{Generation: generation(n), Source: installsource.Source{SHA256: sha}}
	}
	replaced := publish(1, 5*time.Hour, "old")
	active := publish(2, 4*time.Hour, "new")
	candidate := publish(3, time.Minute, "next")
	// A slow install: its validation and handover took far longer than any fixed window.
	slow := publish(4, 3*time.Hour, "next")
	// recordRetired and the archive reopen republish the replaced generation on the active code.
	archive := publish(5, 30*time.Second, "new")
	installing := true
	coordinator := Coordinator{Catalog: c, Active: active, Installing: func() bool { return installing }}
	for _, tc := range []struct {
		name string
		d    Descriptor
		want bool
	}{
		{"the generation the update replaced retires at once", replaced, false},
		{"a candidate published after the active one is protected", candidate, true},
		{"a live candidate is never retired because time passed", slow, true},
		{"an archive republished on the active code retires at once", archive, false},
	} {
		if got := coordinator.candidate(tc.d); got != tc.want {
			t.Errorf("%s: candidate=%v, want %v", tc.name, got, tc.want)
		}
	}
	installing = false
	if coordinator.candidate(candidate) {
		t.Error("with no installation running, a newer generation is a crashed install's orphan and retires")
	}
	if !c.staging(candidate, Descriptor{Generation: generation(9)}) {
		t.Error("an active descriptor that cannot be read keeps the candidate")
	}
}
