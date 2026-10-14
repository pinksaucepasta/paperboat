//go:build linux || darwin

package atomicfile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteReplacesRegularFileWithExactMode(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "state.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Write(path, []byte("new\n"), Options{Mode: 0o640, OwnerUID: os.Geteuid(), OwnerGID: os.Getegid()}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "new\n" {
		t.Fatalf("data=%q err=%v", data, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("mode=%v err=%v", info.Mode().Perm(), err)
	}
}

func TestWriteOwnerFailureKeepsCauseAndFreshReplacementRecovers(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires an unprivileged process for an actual denied ownership change")
	}
	root := t.TempDir()
	path := filepath.Join(root, "private-destination")
	if err := os.WriteFile(path, []byte("PRIVATE-ORIGINAL-CONTENT"), 0o600); err != nil {
		t.Fatal(err)
	}
	groups, err := os.Getgroups()
	if err != nil {
		t.Fatal(err)
	}
	unownedGroup := os.Getegid() + 1000
	for _, group := range groups {
		if group >= unownedGroup {
			unownedGroup = group + 1000
		}
	}
	err = Write(path, []byte("replacement"), Options{Mode: 0o600, OwnerUID: os.Geteuid(), OwnerGID: unownedGroup})
	var failure *Error
	if !errors.As(err, &failure) || failure.Stage != StageOwner || !errors.Is(err, os.ErrPermission) {
		t.Fatal("denied ownership change lost its stage or original permission cause")
	}
	if strings.Contains(err.Error(), root) || strings.Contains(err.Error(), "private-destination") || strings.Contains(err.Error(), "PRIVATE-ORIGINAL-CONTENT") {
		t.Fatal("atomic write error exposed private destination or content")
	}
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != "PRIVATE-ORIGINAL-CONTENT" {
		t.Fatal("failed ownership change altered the existing destination")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != "private-destination" {
		t.Fatal("failed atomic write left a staging file")
	}
	if err := Write(path, []byte("recovered"), CurrentOwnerOptions(0o600)); err != nil {
		t.Fatal(err)
	}
	contents, err = os.ReadFile(path)
	if err != nil || string(contents) != "recovered" {
		t.Fatal("fresh atomic replacement did not recover")
	}
}

func TestWriteRejectsSymlinkAndWrongOwner(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	link := filepath.Join(directory, "link")
	if err := os.WriteFile(target, []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		path  string
		owner int
	}{
		{path: link, owner: os.Geteuid()},
		{path: target, owner: os.Geteuid() + 1},
	} {
		err := Write(test.path, []byte("unsafe"), Options{Mode: 0o600, OwnerUID: test.owner, OwnerGID: -1})
		var typed *Error
		if !errors.As(err, &typed) || typed.Stage != StageValidate {
			t.Fatalf("error=%v", err)
		}
	}
	data, _ := os.ReadFile(target)
	if string(data) != "safe" {
		t.Fatalf("target changed: %q", data)
	}
}
