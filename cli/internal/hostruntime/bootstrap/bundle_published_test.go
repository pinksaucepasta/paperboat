package bootstrap

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Opt-in check of the real five-asset publication and offline proof boundary.
// This downloads only the executing platform's official artifact.
func TestPublishedUpdateBundle(t *testing.T) {
	if os.Getenv("PAPERBOAT_TEST_PUBLISHED_UPDATE_BUNDLE") != "1" {
		t.Skip("requires explicit live published artifact check")
	}
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	path := filepath.Join(root, "release.pb-update")
	index, err := ExportUpdateBundle(ctx, "https://get.pprbt.dev/tuf", filepath.Join(root, "sender"), path, nil, time.Now())
	if err != nil {
		t.Fatalf("published bundle export: %T: %v", err, err)
	}
	t.Logf("published version=%s platform=%s architecture=%s", index.Version, index.Platform, index.Architecture)
	result, err := ImportUpdateBundle(ctx, "https://get.pprbt.dev/tuf", filepath.Join(root, "receiver"), path, filepath.Join(root, "staged"), time.Now())
	if err != nil {
		t.Fatalf("published bundle offline import: %T: %v", err, err)
	}
	if result.Index.Version != index.Version {
		t.Fatal("import selected another version")
	}
}
