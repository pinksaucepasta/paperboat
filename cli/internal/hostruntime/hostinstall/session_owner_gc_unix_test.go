//go:build linux || darwin

package hostinstall

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"testing"
)

func TestSessionOwnerPinPrunePreservesReferencesAndUntrustedFiles(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires protected root-owned pin fixture")
	}
	root, err := os.MkdirTemp("/var/lib", "paperboat-pin-gc-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	pin := func(body string) string {
		t.Helper()
		hash := sha256.Sum256([]byte(body))
		directory := filepath.Join(root, hex.EncodeToString(hash[:]))
		if err := os.Mkdir(directory, 0755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(directory, "pb")
		if err := os.WriteFile(path, []byte(body), 0755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	retained := pin("current or live catalogue reference")
	removed := pin("unreferenced former owner")
	wrongHash := pin("initial exact bytes")
	if err := os.WriteFile(wrongHash, []byte("changed bytes"), 0755); err != nil {
		t.Fatal(err)
	}
	writableDirectory := pin("untrusted directory")
	if err := os.Chmod(filepath.Dir(writableDirectory), 0777); err != nil {
		t.Fatal(err)
	}
	writableFile := pin("untrusted executable")
	if err := os.Chmod(writableFile, 0777); err != nil {
		t.Fatal(err)
	}
	foreignOwner := pin("other owner executable")
	if err := os.Chown(foreignOwner, 65534, 65534); err != nil {
		t.Fatal(err)
	}
	pipePin := pin("named pipe instead of executable")
	if err := os.Remove(pipePin); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(pipePin, 0600); err != nil {
		t.Fatal(err)
	}
	linked := pin("linked executable")
	if err := os.Remove(linked); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(retained, linked); err != nil {
		t.Fatal(err)
	}
	neighbourPin := pin("valid pin with unrelated neighbour")
	neighbour := filepath.Join(filepath.Dir(neighbourPin), "user-file")
	if err := os.WriteFile(neighbour, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := pruneUnixSessionOwnerPins(context.Background(), root, map[string]bool{retained: true}, 22); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{retained, wrongHash, writableDirectory, writableFile, foreignOwner, pipePin, linked, neighbour} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("preserved path disappeared: %s: %v", filepath.Base(path), err)
		}
	}
	for _, path := range []string{removed, neighbourPin} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("unused protected pin remains: %v", err)
		}
	}
	if _, err := os.Lstat(filepath.Dir(removed)); !os.IsNotExist(err) {
		t.Fatalf("empty old pin directory remains: %v", err)
	}
	if err := os.Chmod(root, 0777); err != nil {
		t.Fatal(err)
	}
	if err := pruneUnixSessionOwnerPins(context.Background(), root, nil, 22); err == nil {
		t.Fatal("accepted writable pin root")
	}
	if _, err := os.Lstat(retained); err != nil {
		t.Fatal("custody rejection changed retained pin")
	}
}

func TestSessionOwnerPinPruneBoundsEachSweep(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires protected root-owned pin fixture")
	}
	root, err := os.MkdirTemp("/var/lib", "paperboat-pin-gc-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"one", "two", "three"} {
		hash := sha256.Sum256([]byte(body))
		directory := filepath.Join(root, hex.EncodeToString(hash[:]))
		if err := os.Mkdir(directory, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "pb"), []byte(body), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := pruneUnixSessionOwnerPins(context.Background(), root, nil, 2); err != nil {
		t.Fatal(err)
	}
	remaining, err := os.ReadDir(root)
	if err != nil || len(remaining) != 1 {
		t.Fatalf("bounded sweep remaining=%d: %v", len(remaining), err)
	}
	if err := pruneUnixSessionOwnerPins(context.Background(), root, nil, 2); err != nil {
		t.Fatal(err)
	}
	remaining, err = os.ReadDir(root)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("next sweep remaining=%d: %v", len(remaining), err)
	}
}
