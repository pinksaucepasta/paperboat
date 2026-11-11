//go:build darwin || linux

package hostinstall

import (
	"os"
	"path/filepath"
	"testing"
)

// The hostd token is a 0600 file owned by the user hostd runs as, in a root-owned directory that user cannot write
// (audit C #36: the trust assumption is deliberate, and this pins its exact shape).
func TestHostdTokenIsOwnedByTheUserInARootOwnedDirectory(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("protected installation state requires root")
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	const uid = 2147483002
	request := generationRequest("a")
	request.UID, request.GID = uid, uid
	paths := installPaths{hostdToken: filepath.Join(root, "runtime", "hostd.token")}
	if err := ensureHostdToken(paths, request); err != nil {
		t.Fatal(err)
	}
	file, err := os.Lstat(paths.hostdToken)
	if err != nil || file.Mode().Perm() != 0o600 || ownerUID(file) != uid {
		t.Fatalf("token %v %v, want 0600 owned by the user", file, err)
	}
	directory, err := os.Lstat(filepath.Dir(paths.hostdToken))
	if err != nil || ownerUID(directory) != 0 || directory.Mode().Perm()&0o022 != 0 {
		t.Fatalf("directory %v %v, want root-owned and not writable by others", directory, err)
	}
}
