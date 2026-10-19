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
		if err := validateSharedLockDirectory(l.path); err != nil {
			return safeConfigCause("shared credential lock could not be validated", err)
		}
		_, ownerErr := os.Lstat(filepath.Join(l.path, "owner.json"))
		stale, err := sharedLockIsStale(l.path, hostname)
		lastOwnerErr = err
		if err == nil && stale {
			if os.IsNotExist(ownerErr) {
				// Empty-only removal is atomic: if a creator publishes ownership
				// after this check, removal fails without touching its record.
				if cleanupNewSharedLock(l.path) == nil {
					continue
				}
			} else if ownerErr == nil {
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
		return false, safeConfigCause("shared credential lock owner is invalid", err)
	}
	if owner.Hostname != hostname {
		return time.Since(owner.CreatedAt) > sharedLockRemoteStaleAfter, nil
	}
	return !processAlive(owner.PID), nil
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
