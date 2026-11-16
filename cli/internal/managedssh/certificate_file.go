package managedssh

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
	"golang.org/x/crypto/ssh"
)

// CertificateTarget names one machine's certificate on this computer.
type CertificateTarget struct {
	Home, Alias, OSUser string
}

// EnsureCertificate returns this computer's certificate for target: the one in its file while it is valid for at
// least renewWithin more, else a new one from issue, written to the file. A certificate lives to its expiry
// (revocation refuses its key ID on the host, touching no one else's), so a connection asks the control plane
// nothing while it has one (F5-07). When issue fails (the control plane is unreachable), the file's certificate is
// used for as long as it is valid (F5-08).
func EnsureCertificate(ctx context.Context, target CertificateTarget, publicKey []byte, issue func(context.Context) (string, error), now time.Time, renewWithin time.Duration) ([]byte, error) {
	path := ManagedCertificatePath(target.Home, target.Alias)
	existing, readErr := os.ReadFile(path)
	usable := readErr == nil && UsableCertificate(existing, publicKey, target.OSUser, now, 0)
	if usable && UsableCertificate(existing, publicKey, target.OSUser, now, renewWithin) {
		return existing, nil
	}
	issued, err := issue(ctx)
	if err != nil {
		if usable {
			return existing, nil
		}
		return nil, err
	}
	value := []byte(strings.TrimSpace(issued) + "\n")
	if !UsableCertificate(value, publicKey, target.OSUser, now, 0) {
		return nil, errors.New("the server's SSH certificate does not match this computer's key, user or clock")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return value, atomicfile.Write(path, value, atomicfile.CurrentOwnerOptions(0o600))
}

// UsableCertificate reports whether value is a user certificate of publicKey for principal that is valid now and for
// at least margin more.
func UsableCertificate(value, publicKey []byte, principal string, now time.Time, margin time.Duration) bool {
	certificate, ok := parseCertificate(value)
	key, _, _, _, keyErr := ssh.ParseAuthorizedKey(publicKey)
	if !ok || keyErr != nil || certificate.CertType != ssh.UserCert || !bytes.Equal(certificate.Key.Marshal(), key.Marshal()) || !slices.Contains(certificate.ValidPrincipals, principal) {
		return false
	}
	return uint64(now.Unix()) >= certificate.ValidAfter && uint64(now.Add(margin).Unix()) < certificate.ValidBefore
}

func parseCertificate(value []byte) (*ssh.Certificate, bool) {
	parsed, _, _, _, err := ssh.ParseAuthorizedKey(value)
	if err != nil {
		return nil, false
	}
	certificate, ok := parsed.(*ssh.Certificate)
	return certificate, ok
}

// MarkCertificateUsed records that a connection to alias used its certificate (RecentlyUsedCertificates).
func MarkCertificateUsed(home, alias string) error {
	path := strings.TrimSuffix(ManagedCertificatePath(home, alias), "-cert.pub") + "-used"
	now := time.Now()
	if err := os.Chtimes(path, now, now); err == nil || !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return atomicfile.Write(path, nil, atomicfile.CurrentOwnerOptions(0o600))
}

// RecentlyUsedCertificates lists the aliases whose certificate a connection used since then.
func RecentlyUsedCertificates(home string, since time.Time) []string {
	directory := filepath.Dir(ManagedCertificatePath(home, "x"))
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil
	}
	var aliases []string
	for _, entry := range entries {
		alias, ok := strings.CutSuffix(entry.Name(), "-used")
		info, infoErr := entry.Info()
		if ok && infoErr == nil && info.Mode().IsRegular() && info.ModTime().After(since) {
			aliases = append(aliases, alias)
		}
	}
	return aliases
}
