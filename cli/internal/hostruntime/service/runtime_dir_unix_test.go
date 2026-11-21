//go:build !windows

package service

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOwnRuntimeDirRefusesLinksAndFiles(t *testing.T) {
	dir := t.TempDir()
	if !ownRuntimeDir(dir, os.Getuid()) {
		t.Fatal("own directory refused")
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if ownRuntimeDir(link, os.Getuid()) || ownRuntimeDir(dir, os.Getuid()+1) {
		t.Fatal("link or foreign owner accepted")
	}
}
