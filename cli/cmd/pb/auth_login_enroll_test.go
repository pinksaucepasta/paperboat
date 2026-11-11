package main

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/config"
)

// A profile whose config differs from the one the local daemon service runs for is named, never reset over it;
// the same profile, no installed service, and an unreadable definition are not "another profile".
func TestOtherProfileServesNamesOnlyAForeignConfig(t *testing.T) {
	dir := t.TempDir()
	own := filepath.Join(dir, "own", "config.json")
	cfg, err := config.Load(own)
	if err != nil {
		t.Fatal(err)
	}
	previous := serviceProfile
	t.Cleanup(func() { serviceProfile = previous })
	for name, test := range map[string]struct {
		existing string
		err      error
		other    bool
	}{
		"same profile":    {existing: own},
		"nothing running": {existing: ""},
		"unreadable":      {err: errors.New("unreadable")},
		"another profile": {existing: filepath.Join(dir, "other", "config.json"), other: true},
	} {
		serviceProfile = func() (string, error) { return test.existing, test.err }
		got := otherProfileServes(cfg)
		if (got != nil) != test.other || got != nil && (got.Existing != test.existing || got.Requested != own) {
			t.Errorf("%s: %+v", name, got)
		}
	}
}
