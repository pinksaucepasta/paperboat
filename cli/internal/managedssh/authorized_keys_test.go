//go:build darwin || linux

package managedssh

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestReconcileAuthorizedKeysPreservesUnrelatedBytesAndIsIdempotent(t *testing.T) {
	home := managedSSHTestHome(t)
	directory := filepath.Join(home, ".ssh")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	unrelated := "# user comment\nfrom=\"10.0.0.0/8\" " + authorizedPublicLine(t) + " existing-key\n\n"
	path := filepath.Join(directory, "authorized_keys")
	if err := os.WriteFile(path, []byte(unrelated), 0o600); err != nil {
		t.Fatal(err)
	}
	firstKey, secondKey := authorizedPublicLine(t), authorizedPublicLine(t)
	result, err := ReconcileAuthorizedKeys(home, uint32(os.Getuid()), []string{secondKey, firstKey})
	if err != nil || !result.Changed || result.Count != 2 {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(updated), unrelated) || strings.Count(string(updated), managedAuthorizedKeyMarker) != 2 {
		t.Fatalf("updated authorized_keys=%q", updated)
	}
	replay, err := ReconcileAuthorizedKeys(home, uint32(os.Getuid()), []string{firstKey, secondKey})
	if err != nil || replay.Changed || replay.Count != 2 {
		t.Fatalf("replay=%+v error=%v", replay, err)
	}
	removed, err := ReconcileAuthorizedKeys(home, uint32(os.Getuid()), nil)
	if err != nil || !removed.Changed || removed.Count != 0 {
		t.Fatalf("removed=%+v error=%v", removed, err)
	}
	final, err := os.ReadFile(path)
	if err != nil || string(final) != unrelated {
		t.Fatalf("final=%q error=%v", final, err)
	}
}

func TestReconcileAuthorizedKeysRejectsMarkerConflictSymlinkAndPermissions(t *testing.T) {
	home := managedSSHTestHome(t)
	directory := filepath.Join(home, ".ssh")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "authorized_keys")
	original := "# " + managedAuthorizedKeyMarker + "forged\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReconcileAuthorizedKeys(home, uint32(os.Getuid()), []string{authorizedPublicLine(t)}); !errors.Is(err, ErrAuthorizedKeysConflict) {
		t.Fatalf("marker conflict error=%v", err)
	}
	unchanged, _ := os.ReadFile(path)
	if string(unchanged) != original {
		t.Fatal("conflicting authorized_keys was modified")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(home, "target")
	if err := os.WriteFile(target, []byte("unrelated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := ReconcileAuthorizedKeys(home, uint32(os.Getuid()), nil); err == nil {
		t.Fatal("symlinked authorized_keys was accepted")
	}
	targetValue, _ := os.ReadFile(target)
	if string(targetValue) != "unrelated\n" {
		t.Fatal("symlink target was modified")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("unrelated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReconcileAuthorizedKeys(home, uint32(os.Getuid()), nil); err == nil {
		t.Fatal("permissive authorized_keys was accepted")
	}
}

func TestReconcileAuthorizedKeysSerializesConcurrentRepair(t *testing.T) {
	home := managedSSHTestHome(t)
	keys := []string{authorizedPublicLine(t), authorizedPublicLine(t)}
	var group sync.WaitGroup
	errorsFound := make(chan error, 12)
	for range 12 {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := ReconcileAuthorizedKeys(home, uint32(os.Getuid()), keys)
			errorsFound <- err
		}()
	}
	group.Wait()
	close(errorsFound)
	for err := range errorsFound {
		if err != nil {
			t.Fatal(err)
		}
	}
	value, err := os.ReadFile(filepath.Join(home, ".ssh", "authorized_keys"))
	if err != nil || strings.Count(string(value), managedAuthorizedKeyMarker) != 2 {
		t.Fatalf("authorized_keys=%q error=%v", value, err)
	}
}

func managedSSHTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if err := os.Chmod(home, 0o755); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestReconcileAuthorizedKeysRestrictsManagedLinesToLoopback(t *testing.T) {
	home := managedSSHTestHome(t)
	directory := filepath.Join(home, ".ssh")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "authorized_keys")
	own := "command=\"/bin/true\" " + authorizedPublicLine(t) + " own\n"
	key := authorizedPublicLine(t)
	// A managed line whose option was edited out is rewritten with it; the user's own line is kept.
	bare := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(mustParseKey(t, key)))) + " " + managedAuthorizedKeyMarker + fingerprintHex(t, key) + "\n"
	if err := os.WriteFile(path, []byte(own+bare), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := ReconcileAuthorizedKeys(home, uint32(os.Getuid()), []string{key})
	if err != nil || !result.Changed {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	value, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(value)), "\n")
	if len(lines) != 2 || lines[0]+"\n" != own || !strings.HasPrefix(lines[1], `from="127.0.0.1,::1" ssh-ed25519 `) || !strings.HasSuffix(lines[1], managedAuthorizedKeyMarker+fingerprintHex(t, key)) {
		t.Fatalf("authorized_keys=%q", value)
	}
	parsed, _, options, _, err := ssh.ParseAuthorizedKey([]byte(lines[1]))
	if err != nil || parsed == nil || len(options) != 1 || options[0] != managedAuthorizedKeyFrom {
		t.Fatalf("managed line options=%q error=%v", options, err)
	}
	if replay, err := ReconcileAuthorizedKeys(home, uint32(os.Getuid()), []string{key}); err != nil || replay.Changed {
		t.Fatalf("replay=%+v error=%v", replay, err)
	}
	// Any other option on a managed line is a conflict, never rewritten silently.
	widened := strings.Replace(lines[1], `from="127.0.0.1,::1"`, `from="*"`, 1) + "\n"
	if err := os.WriteFile(path, []byte(widened), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReconcileAuthorizedKeys(home, uint32(os.Getuid()), []string{key}); !errors.Is(err, ErrAuthorizedKeysConflict) {
		t.Fatalf("widened option error=%v", err)
	}
	if after, _ := os.ReadFile(path); string(after) != widened {
		t.Fatal("conflicting authorized_keys was modified")
	}
}

func mustParseKey(t *testing.T, line string) ssh.PublicKey {
	t.Helper()
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func fingerprintHex(t *testing.T, line string) string {
	t.Helper()
	sum := sha256.Sum256(mustParseKey(t, line).Marshal())
	return hex.EncodeToString(sum[:])
}
