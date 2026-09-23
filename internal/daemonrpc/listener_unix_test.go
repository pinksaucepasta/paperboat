//go:build !windows

package daemonrpc

import (
	"os"
	"path/filepath"
	"testing"
)

func testSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "pb-daemonrpc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "daemon.sock")
}

func TestListenerPreservesLiveSocketAndUnrelatedFile(t *testing.T) {
	path := testSocket(t)
	first, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := Listen(path); err == nil {
		second.Close()
		t.Fatal("second daemon acquired live socket")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if listener, err := Listen(path); err == nil {
		listener.Close()
		t.Fatal("removed unrelated file")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "preserve" {
		t.Fatal("existing file changed", err)
	}
}
