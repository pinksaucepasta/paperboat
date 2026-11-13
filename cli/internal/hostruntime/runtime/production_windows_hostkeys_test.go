//go:build windows

package runtime

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func writeHostKey(t *testing.T, dir string) string {
	t.Helper()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	line := string(ssh.MarshalAuthorizedKey(key))
	if err := os.WriteFile(filepath.Join(dir, "ssh_host_ed25519_key.pub"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	return line
}

// pb doctor --repair recreates deleted managed sshd host keys while hostd runs: the reader must return the new key and
// the watch must say so, or the server keeps the old key and every client refuses the machine (winpool, 2026-11-13).
func TestWindowsHostKeysFollowRecreatedFiles(t *testing.T) {
	dir := t.TempDir()
	first := writeHostKey(t, dir)
	read := windowsHostPublicKeys(dir)
	before, err := read()
	if err != nil || len(before) != 1 {
		t.Fatalf("read %v %v", before, err)
	}
	signals, err := watchHostKeyFiles(t.Context(), dir, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "ssh_host_ed25519_key.pub")); err != nil {
		t.Fatal(err)
	}
	second := writeHostKey(t, dir)
	select {
	case <-signals:
	case <-time.After(5 * time.Second):
		t.Fatal("recreating the host key did not signal")
	}
	after, err := read()
	if err != nil || len(after) != 1 || slices.Equal(before, after) || first == second {
		t.Fatalf("after recreation %v %v, before %v", after, err, before)
	}
}
