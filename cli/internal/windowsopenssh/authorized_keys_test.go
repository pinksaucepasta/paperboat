package windowsopenssh

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	wire "github.com/pinksaucepasta/paperboat/shared/managedssh"

	"golang.org/x/crypto/ssh"
)

func TestReconcileAuthorizedKeysUsesDedicatedPaperboatFile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ssh")
	key := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(testQualificationSigner(t).PublicKey())))
	changed, err := ReconcileAuthorizedKeys(root, []string{key, key}, "pb:machine_1", []string{"paperboat:user_a:session_a:machine_1"})
	if err != nil || !changed {
		t.Fatalf("initial reconcile changed=%t err=%v", changed, err)
	}
	path := filepath.Join(root, "authorized_keys", "paperboat")
	body, err := os.ReadFile(path)
	if err != nil || string(body) != wire.CATrustOptions("pb:machine_1")+" "+key+"\n" {
		t.Fatalf("dedicated authorized keys = %q err=%v", body, err)
	}
	if _, err := os.Stat(filepath.Join(root, ".ssh")); !os.IsNotExist(err) {
		t.Fatalf("reconciliation created a user-style .ssh directory: %v", err)
	}
	// The KRL PaperboatSshd reads (RevokedKeys) lists exactly the revoked key ID; the next answer without it empties it.
	krl, err := os.ReadFile(revokedKeysFile(root))
	if err != nil || !bytes.HasPrefix(krl, []byte("SSHKRL\n\x00")) || !bytes.Contains(krl, []byte("paperboat:user_a:session_a:machine_1")) {
		t.Fatalf("revoked keys file=%q err=%v", krl, err)
	}
	changed, err = ReconcileAuthorizedKeys(root, []string{key}, "pb:machine_1", nil)
	if err != nil || changed {
		t.Fatalf("idempotent reconcile changed=%t err=%v", changed, err)
	}
	changed, err = ReconcileAuthorizedKeys(root, nil, "", nil)
	if err != nil || !changed {
		t.Fatalf("revocation reconcile changed=%t err=%v", changed, err)
	}
	body, err = os.ReadFile(path)
	if err != nil || len(body) != 0 {
		t.Fatalf("revoked authorized keys = %q err=%v", body, err)
	}
	if krl, err := os.ReadFile(revokedKeysFile(root)); err != nil || bytes.Contains(krl, []byte("paperboat:")) {
		t.Fatalf("emptied revoked keys file=%q err=%v", krl, err)
	}
	if _, err := ReconcileAuthorizedKeys(root, []string{key}, "pb:machine_1", []string{"not-a-key-id"}); err == nil {
		t.Fatal("a revoked key ID the server never issues was written")
	}
}

// Setup creates the KRL empty before sshd reads its config: a missing RevokedKeys file refuses every key login.
func TestProtectManagedKeysCreatesAnEmptyRevokedKeysFile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ssh")
	if err := protectManagedKeys(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	krl, err := os.ReadFile(revokedKeysFile(root))
	if err != nil || !bytes.HasPrefix(krl, []byte("SSHKRL\n\x00")) || bytes.Contains(krl, []byte("paperboat:")) {
		t.Fatalf("revoked keys file after setup=%q err=%v", krl, err)
	}
}

func TestReconcileAuthorizedKeysHonorsCanceledContextBeforeWriting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	root := filepath.Join(t.TempDir(), "ssh")
	if _, err := ReconcileAuthorizedKeysContext(ctx, root, nil, "", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want context canceled", err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled reconciliation created state: %v", err)
	}
}

func TestReconcileAuthorizedKeysRejectsInvalidReplacementWithoutChangingFile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ssh")
	key := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(testQualificationSigner(t).PublicKey())))
	if _, err := ReconcileAuthorizedKeys(root, []string{key}, "pb:machine_1", nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "authorized_keys", "paperboat")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReconcileAuthorizedKeys(root, []string{"ssh-ed25519 not-a-key"}, "pb:machine_1", nil); err == nil {
		t.Fatal("invalid replacement was accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatalf("authorized keys changed after invalid replacement: before=%q after=%q err=%v", before, after, err)
	}
}
