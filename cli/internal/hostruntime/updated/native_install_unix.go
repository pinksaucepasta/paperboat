//go:build darwin || linux

package updated

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/autoupdate"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updateflow"
)

type nativeInstallSnapshot struct {
	Journal []byte `json:"journal"`
}

func nativeInstallPath(root string) string { return filepath.Join(root, "native-install.json") }
func nativeInstallPending(root string) (bool, error) {
	_, err := os.Lstat(nativeInstallPath(root))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// SupersedeUntouchedUnixActivation cancels only a queued, unmodified activation
// for an explicit supplied installation. The caller holds the activation lock.
// Cancellation commits independently of the subsequent installation: rollback
// must not resurrect approval for the superseded signed candidate.
func SupersedeUntouchedUnixActivation(ctx context.Context, root, executable string, current installsource.Source, controller UnixActivationController) error {
	if ctx == nil || controller == nil || current.Validate() != nil {
		return ErrInvalidConfig
	}
	h, err := readUnixHandoff(root)
	if err != nil || h == nil {
		return err
	}
	if h.Started || h.ParticipantsTouched || h.Previous.Version != current.Version || h.Previous.SHA256 != current.SHA256 || h.Previous.Length != current.Length {
		return ErrActivationPending
	}
	if err := current.Verify(executable); err != nil {
		return errors.Join(ErrActivationPending, err)
	}
	path := filepath.Join(root, "transaction.json")
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return errors.Join(ErrActivationPending, err)
	}
	f := os.NewFile(uintptr(fd), path)
	var st syscall.Stat_t
	statErr := syscall.Fstat(fd, &st)
	body, readErr := io.ReadAll(io.LimitReader(f, (64<<10)+1))
	closeErr := f.Close()
	if statErr != nil || readErr != nil || closeErr != nil || st.Uid != 0 || st.Nlink != 1 || st.Mode&syscall.S_IFMT != syscall.S_IFREG || st.Mode&0777 != 0600 || len(body) > 64<<10 {
		return errors.Join(ErrActivationPending, ErrInvalidConfig, statErr, readErr, closeErr)
	}
	var j updateflow.Journal
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if d.Decode(&j) != nil || d.Decode(new(any)) != io.EOF || j.Validate() != nil || j.ActiveVersion != current.Version || j.ActiveDigest != current.SHA256 || j.ActiveLength != current.Length {
		return ErrActivationPending
	}
	queued := j.Stage == updateflow.StageAwaitingApproval && j.CandidateVersion == h.Candidate && j.CandidateID == h.ApprovalID && j.ApprovedCandidateID == h.ApprovalID
	// Idle is the durable retry point after cancellation but before handoff
	// cleanup. It cannot carry an approval, candidate, or cutover receipt.
	cleared := j.Stage == updateflow.StageIdle && j.CandidateID == "" && j.ApprovedCandidateID == "" && j.CandidateVersion == "" && j.CandidateDigest == "" && j.CandidateLength == 0 && j.ArtifactDigest == "" && j.ArtifactLength == 0 && j.CandidatePolicy == nil && j.StagedPath == "" && j.WorkerID == "" && j.WorkerEpoch == 0 && !j.NativeActivation
	if !queued && !cleared {
		return ErrActivationPending
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Disable reboot/restart activation before clearing approval. A helper
	// waiting for this lock subsequently sees no handoff and retires itself.
	if err := controller.Retire(ctx); err != nil {
		return err
	}
	if queued {
		idle := updateflow.Journal{Schema: j.Schema, TransactionID: j.TransactionID, BootID: j.BootID, Stage: updateflow.StageIdle, StageUpdatedAt: time.Now().UTC(), ActiveVersion: j.ActiveVersion, ActiveSource: j.ActiveSource, ActiveDigest: j.ActiveDigest, ActiveLength: j.ActiveLength, ActiveHostdAPIMin: j.ActiveHostdAPIMin, ActiveHostdAPIMax: j.ActiveHostdAPIMax, ActiveRuntimeAPIMin: j.ActiveRuntimeAPIMin, ActiveRuntimeAPIMax: j.ActiveRuntimeAPIMax, RollbackCount: j.RollbackCount}
		if err := updateflow.Write(path, idle, 0, 0); err != nil {
			return err
		}
	}
	// Keep the handoff until its matching announcement is cleared, so a crash
	// or failed cleanup can retry with the same protected cancellation proof.
	noticePath := filepath.Join(root, "owner-maintenance.json")
	notice, err := autoupdate.LoadOwnerMaintenance(noticePath)
	if err != nil {
		return err
	}
	if notice != nil && notice.CandidateID == h.ApprovalID && notice.Version == h.Candidate {
		if err := autoupdate.ClearOwnerMaintenance(noticePath); err != nil {
			return err
		}
	}
	return removeUnixHandoff(root)
}

// LockUnixNativeInstall excludes update activation before native crash recovery.
func LockUnixNativeInstall(ctx context.Context, root string) (io.Closer, error) {
	// Metadata checks share this lock with activation. Retry contention without
	// weakening secure ownership checks or treating a transient check as recovery.
	bounded, cancel := context.WithTimeout(ctx, maxUpdateControlTimeout)
	defer cancel()
	for {
		if err := bounded.Err(); err != nil {
			return nil, fmt.Errorf("waiting for the update operation before installing: %w", errors.Join(ErrActivationPending, err))
		}
		lock, err := unixActivationLock(root)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, ErrActivationPending) {
			return nil, fmt.Errorf("acquiring the update operation before installing: %w", err)
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-bounded.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
}

// PrepareUnixNativeInstall requires the caller to hold the activation lock.
func PrepareUnixNativeInstall(root string) error {
	if pending, err := nativeInstallPending(root); err != nil || pending {
		return errors.Join(ErrActivationPending, err)
	}
	if handoff, err := readUnixHandoff(root); err != nil || handoff != nil {
		return fmt.Errorf("finish or recover the pending update before installing: %w", errors.Join(ErrActivationPending, err))
	}
	path := filepath.Join(root, "transaction.json")
	journal, err := updateflow.Load(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// An unapproved download has not changed the installation. Snapshot it so
	// an explicit native install can supersede it, or restore it on failure.
	if err == nil && !nativeInstallMaySupersede(journal) {
		return fmt.Errorf("finish or recover the pending update before installing: %w", ErrActivationPending)
	}
	var snapshot nativeInstallSnapshot
	if err == nil {
		snapshot.Journal, err = os.ReadFile(path)
		if err != nil {
			return err
		}
	}
	body, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	if err = atomicfile.Write(nativeInstallPath(root), body, atomicfile.Options{Mode: 0600, OwnerUID: 0, OwnerGID: 0}); err != nil {
		return err
	}
	if err = os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.Join(err, RollbackUnixNativeInstall(root))
	}
	if err = syncUnixDirectory(root); err != nil {
		return errors.Join(err, RollbackUnixNativeInstall(root))
	}
	return nil
}

// CommitUnixNativeInstall runs under the native installer's operation lock.
func CommitUnixNativeInstall(root string) error {
	body, readErr := os.ReadFile(nativeInstallPath(root))
	if errors.Is(readErr, os.ErrNotExist) {
		return nil
	}
	if readErr != nil {
		return readErr
	}
	if err := os.Remove(nativeInstallPath(root)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := syncUnixDirectory(root); err != nil {
		if readErr == nil {
			return errors.Join(err, atomicfile.Write(nativeInstallPath(root), body, atomicfile.Options{Mode: 0600, OwnerUID: 0, OwnerGID: 0}))
		}
		return err
	}
	return nil
}

// RollbackUnixNativeInstall restores the previous non-activating transaction before lifting
// the pending marker. Updater startup and activation honor that marker.
func RollbackUnixNativeInstall(root string) error {
	info, err := os.Lstat(nativeInstallPath(root))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := secureRoot(root); err != nil {
		return err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 256<<10 {
		return ErrInvalidConfig
	}
	body, err := os.ReadFile(nativeInstallPath(root))
	if err != nil {
		return err
	}
	var snapshot nativeInstallSnapshot
	if len(body) > 256<<10 || json.Unmarshal(body, &snapshot) != nil {
		return ErrInvalidConfig
	}
	path := filepath.Join(root, "transaction.json")
	if len(snapshot.Journal) > 0 {
		var previous updateflow.Journal
		if json.Unmarshal(snapshot.Journal, &previous) != nil || previous.Validate() != nil || !nativeInstallMaySupersede(previous) {
			return ErrInvalidConfig
		}
		if err = atomicfile.Write(path, snapshot.Journal, atomicfile.Options{Mode: 0600, OwnerUID: 0, OwnerGID: 0}); err != nil {
			return err
		}
	} else if err = os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return CommitUnixNativeInstall(root)
}

func nativeInstallMaySupersede(journal updateflow.Journal) bool {
	return journal.Stage == updateflow.StageIdle || journal.Stage == updateflow.StageAwaitingApproval && journal.ApprovedCandidateID == ""
}
