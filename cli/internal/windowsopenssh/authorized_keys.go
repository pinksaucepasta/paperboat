package windowsopenssh

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	wire "github.com/pinksaucepasta/paperboat/shared/managedssh"
	"golang.org/x/crypto/ssh"
)

// ReconcileAuthorizedKeys atomically replaces only PaperboatSshd's dedicated authorization file, read for the setup
// account only (sshd_config Match User): that account trusts exactly caKeys, for certificates carrying the machine's
// principal, only from the host's loopback bridge (wire.CATrustOptions). It also replaces the KRL PaperboatSshd reads
// (RevokedKeys, revokedKeysFile) so it refuses the revoked certificates' key IDs. It never touches a user's .ssh.
func ReconcileAuthorizedKeys(stateRoot string, caKeys []string, principal string, revoked []string) (bool, error) {
	return ReconcileAuthorizedKeysContext(context.Background(), stateRoot, caKeys, principal, revoked)
}

// revokedKeysFile is the KRL PaperboatSshd's RevokedKeys names. It must always exist: sshd refuses every key of
// every account when it is missing, so setup creates it empty (protectManagedKeys) before sshd reads its config.
func revokedKeysFile(stateRoot string) string {
	return filepath.Join(stateRoot, "authorized_keys", "revoked.krl")
}

func ReconcileAuthorizedKeysContext(ctx context.Context, stateRoot string, caKeys []string, principal string, revoked []string) (bool, error) {
	if ctx == nil || !filepath.IsAbs(stateRoot) || filepath.Clean(stateRoot) != stateRoot || len(caKeys) > wire.MaxUserCAs || len(caKeys) > 0 && !wire.ValidCAPrincipal(principal) || len(revoked) > wire.MaxRevokedKeyIDs {
		return false, ErrInvalidConfig
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	unlock, err := lockAuthorizedKeys(ctx, stateRoot)
	if err != nil {
		return false, err
	}
	defer unlock()
	lines := make([]string, 0, len(caKeys))
	blobs := make([][]byte, 0, len(caKeys))
	for _, value := range caKeys {
		key, comment, options, rest, err := ssh.ParseAuthorizedKey([]byte(strings.TrimSpace(value)))
		if err != nil || key == nil || key.Type() != ssh.KeyAlgoED25519 || len(options) != 0 || len(bytes.TrimSpace(rest)) != 0 || strings.ContainsAny(comment, "\x00\r\n") {
			return false, ErrQualificationEnrollment
		}
		line := wire.CATrustOptions(principal) + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
		if !slices.Contains(lines, line) {
			lines = append(lines, line)
			blobs = append(blobs, key.Marshal())
		}
	}
	for _, id := range revoked {
		if !wire.ValidRevokedKeyID(id) {
			return false, ErrQualificationEnrollment
		}
	}
	// The KRL first: a certificate revoked in this same answer is refused before the CA lines could change.
	if err := os.MkdirAll(filepath.Dir(revokedKeysFile(stateRoot)), 0o700); err != nil {
		return false, err
	}
	krl := wire.KRL(blobs, revoked, uint64(time.Now().UnixNano()), uint64(time.Now().Unix()))
	if err := atomicfile.Write(revokedKeysFile(stateRoot), krl, managedKeysFileOptions()); err != nil {
		return false, err
	}
	slices.Sort(lines)
	body := []byte(nil)
	if len(lines) > 0 {
		body = []byte(strings.Join(lines, "\n") + "\n")
	}
	directory := filepath.Join(stateRoot, "authorized_keys")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return false, err
	}
	path := filepath.Join(directory, "paperboat")
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if bytes.Equal(existing, body) {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := atomicfile.Write(path, body, managedKeysFileOptions()); err != nil {
		return false, err
	}
	return true, nil
}

// protectManagedKeys gives Paperboat's key file and KRL their fixed owner and DACL at
// every setup and every sshd start, creating them empty when missing. An existing
// file keeps its keys: only its security is set, in place, so a concurrent
// reconcile from another session cannot be overwritten.
func protectManagedKeys(ctx context.Context, stateRoot string) error {
	unlock, err := lockAuthorizedKeys(ctx, stateRoot)
	if err != nil {
		return err
	}
	defer unlock()
	directory := filepath.Join(stateRoot, "authorized_keys")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	if _, err := os.Lstat(revokedKeysFile(stateRoot)); errors.Is(err, os.ErrNotExist) {
		if err := atomicfile.Write(revokedKeysFile(stateRoot), wire.KRL(nil, nil, 1, uint64(time.Now().Unix())), managedKeysFileOptions()); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if err := setManagedKeysSecurity(revokedKeysFile(stateRoot)); err != nil {
		return err
	}
	path := filepath.Join(directory, "paperboat")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return atomicfile.Write(path, nil, managedKeysFileOptions())
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return ErrServiceOwnership
	}
	return setManagedKeysSecurity(path)
}
