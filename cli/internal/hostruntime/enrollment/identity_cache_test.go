//go:build !windows

package enrollment

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/identity"
)

func writeRuntimeIdentityFile(t *testing.T, root, keyID string, rename bool) {
	t.Helper()
	body, err := json.Marshal(RuntimeIdentity{Version: 1, HelperID: "helper_1", MachineID: "machine_1", EnvironmentID: "env_1", Credential: strings.Repeat("c", 40), ExpiresAt: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC), KeyID: keyID})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "runtime-identity.json")
	if rename {
		temp := path + ".new"
		if err := os.WriteFile(temp, body, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(temp, path); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Each authenticated request loads the identity; a burst of them reuses one load, and any replaced file or the
// TTL forces a full, checked read again.
func TestLoadRuntimeIdentityIsReusedUntilAFileChangesOrTheTTLPasses(t *testing.T) {
	root := t.TempDir()
	store, err := identity.Open(identity.Config{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	writeRuntimeIdentityFile(t, root, store.Current().ID, false)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := time.Now()
	identityCacheNow = func() time.Time { return clock }
	t.Cleanup(func() { identityCacheNow = time.Now })

	if _, err := LoadRuntimeIdentity(root, now); err != nil {
		t.Fatal(err)
	}
	// Corrupt the body but keep inode, size, mode and mtime: only a cached answer survives.
	path := filepath.Join(root, "runtime-identity.json")
	info, _ := os.Lstat(path)
	if err := os.WriteFile(path, []byte(strings.Repeat("x", int(info.Size()))), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRuntimeIdentity(root, now); err != nil {
		t.Fatalf("a second load inside the TTL read the file again: %v", err)
	}
	clock = clock.Add(identityCacheTTL)
	if _, err := LoadRuntimeIdentity(root, now); err == nil {
		t.Fatal("a load after the TTL trusted the cache")
	}
	// A replaced file (new inode, as renewal writes it) is seen at once.
	writeRuntimeIdentityFile(t, root, store.Current().ID, true)
	if _, err := LoadRuntimeIdentity(root, now); err != nil {
		t.Fatal(err)
	}
	writeRuntimeIdentityFile(t, root, "other-key", true)
	if _, err := LoadRuntimeIdentity(root, now); err == nil {
		t.Fatal("a replaced identity file was served from the cache")
	}
}
