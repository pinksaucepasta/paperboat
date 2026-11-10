//go:build linux

package machineservices

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestPidsForSocketInodesUsesFakeProc(t *testing.T) {
	root := t.TempDir()
	mkproc := func(pid int, comm string, links map[string]string) {
		dir := filepath.Join(root, strconv.Itoa(pid), "fd")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for fd, target := range links {
			if err := os.Symlink(target, filepath.Join(dir, fd)); err != nil {
				t.Fatal(err)
			}
		}
		if comm != "" {
			if err := os.WriteFile(filepath.Join(root, strconv.Itoa(pid), "comm"), []byte(comm+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	mkproc(111, "nginx", map[string]string{"0": "socket:[7]", "1": "/dev/null"})
	mkproc(222, "apache2", map[string]string{"0": "socket:[7]"})
	mkproc(333, "", map[string]string{"0": "not-a-socket"})
	// 444/fd is a file, not a directory: ReadDir fails, so 444 is skipped like
	// another user's unreadable process.
	if err := os.MkdirAll(filepath.Join(root, "444"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "444", "fd"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := pidsForSocketInodes(root, map[uint64]bool{7: true})
	if len(got) != 2 || got[0] != 111 || got[1] != 222 {
		t.Fatalf("pids=%v", got)
	}
	if name := commOf(root, 111); name != "nginx" {
		t.Fatalf("comm=%q", name)
	}
	if name := commOf(root, 333); name != "" {
		t.Fatalf("missing comm=%q", name)
	}
	if got := pidsForSocketInodes(root, map[uint64]bool{999: true}); len(got) != 0 {
		t.Fatalf("unknown inode pids=%v", got)
	}
}
