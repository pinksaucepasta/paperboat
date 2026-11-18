package containers

import (
	"os"
	"path/filepath"
	"testing"
)

// images.json as 2026.11.16.2 wrote it (testdata from that release's SaveImages) loads after an update.
func TestLoadsImagesWrittenByPreviousRelease(t *testing.T) {
	raw, err := os.ReadFile("testdata/images-2026.11.16.2.json")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "images.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	images, err := LoadImages(dir)
	if err != nil || images.Relay == "" || images.Edge == "" {
		t.Fatalf("previous release's images = %+v, %v", images, err)
	}
}
