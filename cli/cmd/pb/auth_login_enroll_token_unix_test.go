//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoginEnrollmentTokenRefusesAPlantedSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "enrollment-token")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := writeLoginEnrollmentToken(path, "token"); err == nil {
		t.Fatal("the token was written through a planted symlink")
	}
	if body, _ := os.ReadFile(target); string(body) != "keep" {
		t.Fatalf("the symlink target was written: %q", body)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := writeLoginEnrollmentToken(path, "token"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("token file %v %v, want a regular 0600 file", info, err)
	}
	if body, _ := os.ReadFile(path); string(body) != "token" {
		t.Fatalf("token file holds %q", body)
	}
}
