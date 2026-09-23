package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestManualInstallerCommandWorksWithoutPreferences(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "config.json")
	if err := os.WriteFile(filepath.Join(dir, "config.preferences.json"), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	manual := filepath.Join(dir, "share", "man")
	for _, remove := range []bool{false, true} {
		args := []string{"--no-customization", "--config", config, "__man-pages", "--directory", manual}
		if remove {
			args = append(args, "--remove")
		}
		var out, stderr bytes.Buffer
		if code := runWithReporter(context.Background(), args, &out, &stderr, nil); code != 0 {
			t.Fatalf("exit %d: %s", code, stderr.String())
		}
		_, err := os.Stat(filepath.Join(manual, "man1", "pb.1"))
		if (!remove && err != nil) || (remove && !os.IsNotExist(err)) {
			t.Fatalf("remove=%v: %v", remove, err)
		}
	}
}
