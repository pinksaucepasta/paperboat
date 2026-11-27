//go:build linux || darwin

package hostinstall

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/atomicfile"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/sessionowner"
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
	// Archives holds the generations of retired session owners that stay as archive readers: their descriptor was
	// republished on the active code (it names another source than its own generation) so their session history
	// stays readable. They run no process and belong to no update in progress, whenever they were written.
	Archives map[string]bool
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
	sources := InventorySources{Retained: map[string]time.Time{}, Archives: map[string]bool{}}
	pending, err := pendingInstallRecord(paths)
	if err != nil {
		return sources, err
	}
	sources.Pending = pending
	digest, activated, err := fileDigest(paths.worker)
	if installNotExistOnly(err) {
		return sources, nil
	}
	if err != nil {
		return sources, err
	}
	sources.Active, sources.Activated = digest, activated
	// A restore rewrites the slot (rollback stages the prior version again), so its time is when the rollback ran,
	// not when that version was activated: a failed update's process that started in between passed as draining
	// (device A/B 11-27: the rolled-back build's root service, inventory ok). The commit's own record is the time.
	if recorded, ok := slotActivation(paths, digest); ok {
		sources.Activated = recorded
	}
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
			if len(d.Source.SHA256) >= len(d.Generation) && d.Source.SHA256[:len(d.Generation)] != d.Generation {
				sources.Archives[d.Generation] = true
			}
		}
	}
	return sources, err
}

// slotActivationRecord is when the fixed slot's bytes were committed. Only a commit writes it, so a failed update
// and its rollback leave the prior version's record in place. A separate file, so older builds never read it.
type slotActivationRecord struct {
	SHA256      string    `json:"sha256"`
	ActivatedAt time.Time `json:"activated_at"`
}

func slotActivationPath(paths installPaths) string { return paths.metadata + ".activated" }

func recordSlotActivation(paths installPaths, digest string, at time.Time) error {
	body, err := json.Marshal(slotActivationRecord{SHA256: digest, ActivatedAt: at.UTC()})
	if err != nil {
		return err
	}
	return atomicfile.Write(slotActivationPath(paths), body, atomicfile.Options{Mode: 0o600, OwnerUID: 0, OwnerGID: -1})
}

// slotActivation is the recorded commit time of the bytes digest names; false when none names them.
func slotActivation(paths installPaths, digest string) (time.Time, bool) {
	info, err := os.Lstat(slotActivationPath(paths))
	if err != nil || !info.Mode().IsRegular() || ownerUID(info) != 0 || info.Size() > 4<<10 {
		return time.Time{}, false
	}
	body, err := readProtectedInstallState(slotActivationPath(paths), 4<<10)
	if err != nil {
		return time.Time{}, false
	}
	var record slotActivationRecord
	if decodeInstallJSON(bytes.NewReader(body), &record, 4<<10) != nil || record.SHA256 != digest || record.ActivatedAt.IsZero() {
		return time.Time{}, false
	}
	return record.ActivatedAt, true
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

// pendingInstallRecord names the first record of an unfinished transaction. The service lifecycle journal counts:
// a failed cutover removes the install journal before the lifecycle restore runs, and an unfinished restore still
// needs the previous version's files.
func pendingInstallRecord(paths installPaths) (string, error) {
	lifecycleJournal := filepath.Join(paths.root, "service-lifecycle", "service-lifecycle.json")
	for _, record := range []string{paths.journal, filepath.Join(paths.updateState, "native-install.json"), lifecycleJournal} {
		if _, err := os.Lstat(record); err == nil {
			return record, nil
		} else if !installNotExistOnly(err) {
			return "", err
		}
	}
	return "", nil
}
