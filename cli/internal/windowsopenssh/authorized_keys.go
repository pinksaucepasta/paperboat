package windowsopenssh

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"golang.org/x/crypto/ssh"
)

// ReconcileAuthorizedKeys atomically replaces only PaperboatSshd's dedicated
// authorization file. It never reads or writes a user's .ssh directory.
func ReconcileAuthorizedKeys(stateRoot string, publicKeys []string) (bool, error) {
	return ReconcileAuthorizedKeysContext(context.Background(), stateRoot, publicKeys)
}

func ReconcileAuthorizedKeysContext(ctx context.Context, stateRoot string, publicKeys []string) (bool, error) {
	changed, _, err := ReconcileAuthorizedKeysRevoking(ctx, stateRoot, publicKeys)
	return changed, err
}

// ReconcileAuthorizedKeysRevoking is ReconcileAuthorizedKeysContext that also reports whether a key left the file,
// the one change after which open sessions must end (sshd checks keys only at login).
func ReconcileAuthorizedKeysRevoking(ctx context.Context, stateRoot string, publicKeys []string) (changed, removed bool, err error) {
	if ctx == nil || !filepath.IsAbs(stateRoot) || filepath.Clean(stateRoot) != stateRoot || len(publicKeys) > 256 {
		return false, false, ErrInvalidConfig
	}
	if err := ctx.Err(); err != nil {
		return false, false, err
	}
	unlock, err := lockAuthorizedKeys(ctx, stateRoot)
	if err != nil {
		return false, false, err
	}
	defer unlock()
	lines := make([]string, 0, len(publicKeys))
	seen := make(map[string]bool, len(publicKeys))
	for _, value := range publicKeys {
		key, comment, options, rest, err := ssh.ParseAuthorizedKey([]byte(strings.TrimSpace(value)))
		if err != nil || key == nil || key.Type() != ssh.KeyAlgoED25519 || len(options) != 0 || len(bytes.TrimSpace(rest)) != 0 || strings.ContainsAny(comment, "\x00\r\n") {
			return false, false, ErrQualificationEnrollment
		}
		line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
		if seen[line] {
			continue
		}
		seen[line] = true
		lines = append(lines, line)
	}
	slices.Sort(lines)
	body := []byte(nil)
	if len(lines) > 0 {
		body = []byte(strings.Join(lines, "\n") + "\n")
	}
	directory := filepath.Join(stateRoot, "authorized_keys")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return false, false, err
	}
	path := filepath.Join(directory, "paperboat")
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, false, err
	}
	if bytes.Equal(existing, body) {
		return false, false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, false, err
	}
	if err := atomicfile.Write(path, body, managedKeysFileOptions()); err != nil {
		return false, false, err
	}
	return true, keyLeft(existing, seen), nil
}

// keyLeft reports whether the old file held a key that the new set (normalized lines) does not.
func keyLeft(existing []byte, kept map[string]bool) bool {
	for _, line := range strings.Split(string(existing), "\n") {
		if line = strings.TrimSpace(line); line != "" && !kept[line] {
			return true
		}
	}
	return false
}

// protectManagedKeys gives Paperboat's key file its fixed owner and DACL at
// every setup and every sshd start, creating it empty when missing. An existing
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
