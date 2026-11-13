package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

const sharedLockRemoteStaleAfter = 30 * time.Minute

// A creator normally writes owner.json immediately. Allow its full acquisition
// budget before reclaiming an empty directory left by an interrupted process.
const sharedLockOwnerlessStaleAfter = 30 * time.Second

// processStartSlack covers the coarse (one second) process start times some platforms report.
const processStartSlack = 2 * time.Second

type sharedLockOwner struct {
	PID       int       `json:"pid"`
	Hostname  string    `json:"hostname"`
	CreatedAt time.Time `json:"created_at"`
	Token     string    `json:"token"`
}

type sharedLock struct {
	path  string
	token string
	local *sync.Mutex
}

var sharedLockMu sync.Map

// sharedLockInspectGrace is how long a waiter keeps polling a lock directory it cannot validate: a release races the
// inspection for milliseconds, a directory with a foreign owner or ACL stays invalid.
const sharedLockInspectGrace = 2 * time.Second

// inspectSharedLock validates another process's lock directory; a variable so a test can release it mid-inspection.
var inspectSharedLock = validateSharedLockDirectory

// SharedLockDirectorySuffix ends the directory of every shared credential lock (newSharedLock adds ".d" to a ".lock" path).
const SharedLockDirectorySuffix = ".lock.d"

func newSharedLock(path string) *sharedLock {
	key := path + ".d"
	local, _ := sharedLockMu.LoadOrStore(key, &sync.Mutex{})
	return &sharedLock{path: key, local: local.(*sync.Mutex)}
}

func (l *sharedLock) Lock() error {
	if l.local != nil {
		l.local.Lock()
	}
	locked := false
	defer func() {
		if !locked && l.local != nil {
			l.local.Unlock()
		}
	}()
	if err := prepareSharedLockParent(filepath.Dir(l.path)); err != nil {
		return safeConfigCause("shared credential lock parent could not be prepared", err)
	}
	hostname, _ := os.Hostname()
	l.token = strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	owner := sharedLockOwner{PID: os.Getpid(), Hostname: hostname, CreatedAt: time.Now().UTC(), Token: l.token}
	encoded, err := json.Marshal(owner)
	if err != nil {
		return safeConfigCause("shared credential lock owner could not be encoded", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	var lastOwnerErr error
	var invalidSince time.Time // since when every inspection of the other process's lock directory failed
	for {
		if err := createSharedLockDirectory(l.path); err == nil {
			if err := writeSharedLockOwner(filepath.Join(l.path, "owner.json"), append(encoded, '\n')); err != nil {
				writeErr := safeConfigCause("shared credential lock owner could not be written", err)
				if cleanupErr := cleanupNewSharedLock(l.path); cleanupErr != nil {
					writeErr = errors.Join(writeErr, safeConfigCause("failed shared credential lock could not be cleaned up", cleanupErr))
				}
				return writeErr
			}
			locked = true
			return nil
		} else if !os.IsExist(err) {
			return safeConfigCause("shared credential lock could not be acquired", err)
		}
		// The directory is another process's lock, and that process may release it while it is inspected. On Windows it
		// then vanishes before the open (ERROR_FILE_NOT_FOUND) or under the open handle (its final path no longer
		// resolves): "not acquired yet", not a failure. Ten parallel pb.exe and the daemon failed their credential reads
		// on it (victus 2026-11-10). Only a directory that stays invalid is refused.
		if err := inspectSharedLock(l.path); err != nil {
			if invalidSince.IsZero() {
				invalidSince = time.Now()
			}
			if time.Since(invalidSince) > sharedLockInspectGrace {
				return safeConfigCause("shared credential lock could not be validated", err)
			}
			//paperboat:allow-source-policy sleep owner=runtime-auth reason=bounded-cross-process-lock-poll
			time.Sleep(25 * time.Millisecond)
			continue
		}
		invalidSince = time.Time{}
		_, ownerErr := os.Lstat(filepath.Join(l.path, "owner.json"))
		stale, err := sharedLockIsStale(l.path, hostname)
		lastOwnerErr = err
		if err == nil && stale {
			// Empty-only removal is atomic: if a creator publishes ownership after this check, removal fails
			// without touching its record. A directory holding only a dead creator's temp file is quarantined.
			if os.IsNotExist(ownerErr) && cleanupNewSharedLock(l.path) == nil {
				continue
			}
			if ownerErr == nil || os.IsNotExist(ownerErr) {
				stalePath := l.path + ".stale-" + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 10)
				//paperboat:allow-source-policy atomic-replacement owner=runtime-auth reason=stale-lock-quarantine
				if quarantineSharedLock(l.path, stalePath) == nil {
					continue
				}
			}
		}
		if time.Now().After(deadline) {
			return errors.Join(errors.New("timed out waiting for shared credential lock"), lastOwnerErr)
		}
		//paperboat:allow-source-policy sleep owner=runtime-auth reason=bounded-cross-process-lock-poll
		time.Sleep(25 * time.Millisecond)
	}
}

func sharedLockIsStale(path, hostname string) (bool, error) {
	b, err := os.ReadFile(filepath.Join(path, "owner.json"))
	if credentialAbsenceOnly(err) {
		info, statErr := os.Stat(path)
		if statErr != nil {
			return false, safeConfigCause("shared credential lock directory could not be inspected", statErr)
		}
		return time.Since(info.ModTime()) > sharedLockOwnerlessStaleAfter, nil
	}
	if err != nil {
		return false, safeConfigCause("shared credential lock owner could not be read", err)
	}
	var owner sharedLockOwner
	if err := json.Unmarshal(b, &owner); err != nil {
		// An empty or truncated owner (disk full or a crash while it was written) never names a live holder; waiting
		// on it forever took a machine's carrier down (canary-a 2026-11-13). Same age rule as an ownerless directory,
		// so an owner being written right now is not taken. Prior art: gofrs/flock and lockedfile hold no owner file
		// and rely on the kernel dropping the lock with the process.
		info, statErr := os.Stat(filepath.Join(path, "owner.json"))
		if statErr != nil {
			return false, safeConfigCause("shared credential lock owner could not be inspected", statErr)
		}
		return time.Since(info.ModTime()) > sharedLockOwnerlessStaleAfter, nil
	}
	if owner.Hostname != hostname {
		return time.Since(owner.CreatedAt) > sharedLockRemoteStaleAfter, nil
	}
	// A recycled PID belongs to a process that started after the lock was created; its owner is gone.
	return !processAlive(owner.PID) || processStartedAfter(owner.PID, owner.CreatedAt), nil
}

func (l *sharedLock) Unlock() error {
	defer func() {
		if l.local != nil {
			l.local.Unlock()
		}
	}()
	if err := validateSharedLockDirectory(l.path); err != nil {
		if credentialAbsenceOnly(err) {
			return nil
		}
		return safeConfigCause("shared credential lock could not be validated during release", err)
	}
	b, err := os.ReadFile(filepath.Join(l.path, "owner.json"))
	if credentialAbsenceOnly(err) {
		return nil
	}
	if err != nil {
		return safeConfigCause("shared credential lock owner could not be read", err)
	}
	var owner sharedLockOwner
	if err := json.Unmarshal(b, &owner); err != nil {
		return safeConfigCause("shared credential lock owner is invalid", err)
	}
	if owner.Token != l.token {
		return nil
	}
	if err := removeSharedLock(l.path); err != nil {
		return safeConfigCause("shared credential lock could not be removed", err)
	}
	return nil
}
