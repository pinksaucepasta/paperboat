//go:build linux || darwin

package hostinstall

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/sessionowner"
)

// InventorySources is what one account's install may keep after an install,
// upgrade or rollback: the version in its fixed slot and the older versions
// the session-owner catalogue still references (live PTYs and retained history).
type InventorySources struct {
	// Active is the SHA-256 of the bytes in the fixed slot; empty when none is installed.
	Active string
	// Activated is when the fixed slot was last written (its activation).
	Activated time.Time
	// Retained maps each catalogue-referenced source digest to when its
	// descriptor was written.
	Retained map[string]time.Time
	// Pending names the record of an unfinished transaction: the installation journal, or a signed
	// update's native-install receipt. Its release slots and candidate are still in use.
	Pending string
}

// LoadInventorySources reads the account's sources from the files the install owns.
func LoadInventorySources(ctx context.Context, uid int) (InventorySources, error) {
	if os.Geteuid() != 0 {
		return InventorySources{}, ErrNotPrivileged
	}
	paths := platformPaths(uid)
	sources := InventorySources{Retained: map[string]time.Time{}}
	for _, record := range []string{paths.journal, filepath.Join(paths.updateState, "native-install.json")} {
		if _, err := os.Lstat(record); err == nil {
			sources.Pending = record
			break
		} else if !installNotExistOnly(err) {
			return sources, err
		}
	}
	digest, activated, err := fileDigest(paths.worker)
	if installNotExistOnly(err) {
		return sources, nil
	}
	if err != nil {
		return sources, err
	}
	sources.Active, sources.Activated = digest, activated
	request, err := loadInstallMetadata(paths.metadata, uid)
	if installNotExistOnly(err) || err == nil && request.EnrollmentPending {
		return sources, nil
	}
	if err != nil {
		return sources, err
	}
	catalog, instance, err := sessionOwnerCatalog(request)
	if err != nil {
		return sources, err
	}
	descriptors, err := catalog.ReadAll(ctx, instance)
	if installNotExistOnly(err) {
		return sources, nil
	}
	for _, d := range descriptors {
		path, findErr := sessionowner.Find(catalog.Root, d.Generation)
		info, statErr := os.Lstat(path)
		if err = errors.Join(err, findErr, statErr); findErr == nil && statErr == nil {
			sources.Retained[d.Source.SHA256] = info.ModTime()
		}
	}
	return sources, err
}

func fileDigest(path string) (string, time.Time, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", time.Time{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", time.Time{}, err
	}
	if !info.Mode().IsRegular() {
		return "", time.Time{}, errors.New("installed executable is not a regular file")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(file, installsource.MaxBytes+1)); err != nil {
		return "", time.Time{}, err
	}
	return hex.EncodeToString(hash.Sum(nil)), info.ModTime(), nil
}
