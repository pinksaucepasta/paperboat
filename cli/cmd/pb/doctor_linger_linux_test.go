//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	doctorpkg "github.com/pinksaucepasta/paperboat/internal/doctor"
)

func TestDoctorWarnsWhenTheDaemonStopsAtLogout(t *testing.T) {
	root := t.TempDir()
	systemd, linger := filepath.Join(root, "systemd"), filepath.Join(root, "linger")
	for _, directory := range []string{systemd, linger} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	name := func() (string, error) { return "tester", nil }
	off := doctorLingerCheck(systemd, linger, name)
	if off.Status != doctorpkg.StatusWarning || !strings.Contains(off.Summary, "stops when you log out") || !strings.Contains(off.Recovery, "enable-linger tester") {
		t.Fatalf("lingering off was not explained: %+v", off)
	}
	if err := os.WriteFile(filepath.Join(linger, "tester"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if on := doctorLingerCheck(systemd, linger, name); on.Status != doctorpkg.StatusPass {
		t.Fatalf("lingering on reported %+v", on)
	}
	if err := os.Remove(systemd); err != nil {
		t.Fatal(err)
	}
	if other := doctorLingerCheck(systemd, linger, name); other.Status != doctorpkg.StatusPass {
		t.Fatalf("a host without systemd reported %+v", other)
	}
}
