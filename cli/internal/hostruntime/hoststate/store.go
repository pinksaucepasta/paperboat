package hoststate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
)

const (
	primaryFile = "state.json"
	backupFile  = "state.backup.json"
	stagingFile = "state.next.json"
	lockFile    = "state.lock"
)

var (
	ErrLocked       = errors.New("host state is locked by another process")
	ErrConflict     = errors.New("host state revision conflict")
	ErrCorrupt      = errors.New("host state is corrupt")
	ErrIncompatible = errors.New("host state schema is newer than this runtime")
	ErrClosed       = errors.New("host state store is closed")
	ErrUncertain    = errors.New("host state commit outcome is uncertain")
)

// safeStoreError keeps typed causes available to internal callers while its
// public text contains no filesystem paths, state contents, or OS error text.
type safeStoreError struct {
	message string
	causes  []error
}

func (err safeStoreError) Error() string { return err.message }

func (err safeStoreError) Unwrap() []error {
	causes := make([]error, len(err.causes))
	copy(causes, err.causes)
	return causes
}

func safeStoreFailure(message string, causes ...error) error {
	retained := make([]error, 0, len(causes))
	for _, cause := range causes {
		if cause != nil {
			retained = append(retained, cause)
		}
	}
	return safeStoreError{message: message, causes: retained}
}

const maxAbsenceCauseNodes = 16

// pureNotExist reports absence only when every bounded leaf in the error tree
// is a platform-recognized missing-file error. This prevents a joined close,
// validation, or probe failure from being hidden by errors.Is(os.ErrNotExist).
func pureNotExist(err error) bool {
	if nilError(err) {
		return false
	}
	pending := []error{err}
	visited := 0
	hasLeaf := false
	for len(pending) > 0 {
		if visited >= maxAbsenceCauseNodes {
			return false
		}
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if nilError(current) {
			return false
		}
		visited++

		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			causes := wrapped.Unwrap()
			if len(causes) == 0 || len(causes) > maxAbsenceCauseNodes-visited-len(pending) {
				return false
			}
			for _, cause := range causes {
				if nilError(cause) {
					return false
				}
			}
			pending = append(pending, causes...)
		case interface{ Unwrap() error }:
			cause := wrapped.Unwrap()
			if nilError(cause) || len(pending)+1 > maxAbsenceCauseNodes-visited {
				return false
			}
			pending = append(pending, cause)
		default:
			hasLeaf = true
			if !os.IsNotExist(current) {
				return false
			}
		}
	}
	return hasLeaf
}

func nilError(err error) bool {
	if err == nil {
		return true
	}
	value := reflect.ValueOf(err)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

type Phase string

const (
	PhaseCommitStaged             Phase = "commit_staged"
	PhaseCommitBackupSynced       Phase = "commit_backup_synced"
	PhaseCommitPrimarySynced      Phase = "commit_primary_synced"
	PhaseCommitCleanupSynced      Phase = "commit_cleanup_synced"
	PhaseMigrationSourcePreserved Phase = "migration_source_preserved"
	PhaseMigrationStaged          Phase = "migration_staged"
	PhaseMigrationPrimarySynced   Phase = "migration_primary_synced"
	PhaseMigrationBackupSynced    Phase = "migration_backup_synced"
	PhaseMigrationCleanupSynced   Phase = "migration_cleanup_synced"
	PhaseRecoveryCorruptionSaved  Phase = "recovery_corruption_preserved"
	PhaseRecoveryBackupRestored   Phase = "recovery_backup_restored"
	PhaseRecoveryIncompleteSaved  Phase = "recovery_incomplete_preserved"
)

type FailureHook func(Phase) error

type Config struct {
	Root        string
	Clock       func() time.Time
	FailureHook FailureHook
}

type StartupStatus struct {
	Degraded       bool     `json:"degraded"`
	Code           string   `json:"code"`
	Source         string   `json:"source"`
	PreservedPaths []string `json:"preserved_paths,omitempty"`
}

type CommitError struct {
	Phase   Phase
	Changed bool
	Err     error
}

func (e *CommitError) Error() string {
	if e == nil {
		return "host state persistence failed"
	}
	outcome := "unchanged"
	if e.Changed {
		outcome = "uncertain"
	}
	phase := string(e.Phase)
	if !knownPhase(e.Phase) {
		phase = "unknown"
	}
	return fmt.Sprintf("host state persistence failed during %s (%s)", phase, outcome)
}

func (e *CommitError) Unwrap() error {
	if e.Changed {
		return errors.Join(ErrUncertain, e.Err)
	}
	return e.Err
}

func knownPhase(phase Phase) bool {
	switch phase {
	case PhaseCommitStaged, PhaseCommitBackupSynced, PhaseCommitPrimarySynced, PhaseCommitCleanupSynced,
		PhaseMigrationSourcePreserved, PhaseMigrationStaged, PhaseMigrationPrimarySynced,
		PhaseMigrationBackupSynced, PhaseMigrationCleanupSynced, PhaseRecoveryCorruptionSaved,
		PhaseRecoveryBackupRestored, PhaseRecoveryIncompleteSaved:
		return true
	default:
		return false
	}
}

type document struct {
	Schema        string    `json:"schema"`
	SchemaVersion int       `json:"schema_version"`
	Revision      uint64    `json:"revision"`
	WrittenAt     time.Time `json:"written_at"`
	State         State     `json:"state"`
	Checksum      string    `json:"checksum"`
}

type unsignedDocument struct {
	Schema        string    `json:"schema"`
	SchemaVersion int       `json:"schema_version"`
	Revision      uint64    `json:"revision"`
	WrittenAt     time.Time `json:"written_at"`
	State         State     `json:"state"`
}

// legacyDocumentV0 is the only bounded development-state migration. It
// predates checksums but already contained reference-only State values.
type legacyDocumentV0 struct {
	Schema        string    `json:"schema"`
	SchemaVersion int       `json:"schema_version"`
	Revision      uint64    `json:"revision"`
	WrittenAt     time.Time `json:"written_at"`
	State         State     `json:"state"`
}

type Store struct {
	mu        sync.RWMutex
	root      string
	primary   string
	backup    string
	staging   string
	lock      *processLock
	now       func() time.Time
	hook      FailureHook
	document  document
	status    StartupStatus
	closed    bool
	uncertain bool
}

func Open(config Config) (_ *Store, status StartupStatus, resultErr error) {
	status = StartupStatus{Code: "ready", Source: "primary"}
	root := filepath.Clean(config.Root)
	if !filepath.IsAbs(root) || root != config.Root {
		return nil, status, ErrInvalidState
	}
	if config.Clock == nil {
		config.Clock = func() time.Time { return time.Now().UTC() }
	}
	if err := ensurePrivateDirectory(root); err != nil {
		return nil, status, safeStoreFailure("host state directory could not be prepared", err)
	}
	lock, err := acquireProcessLock(filepath.Join(root, lockFile))
	if err != nil {
		return nil, status, safeStoreFailure("host state lock could not be acquired", err)
	}
	defer func() {
		if resultErr != nil {
			resultErr = safeStoreFailure("host state could not be opened", resultErr, lock.Close())
		}
	}()
	store := &Store{
		root: root, primary: filepath.Join(root, primaryFile),
		backup: filepath.Join(root, backupFile), staging: filepath.Join(root, stagingFile),
		lock: lock, now: config.Clock, hook: config.FailureHook, status: status,
	}
	if err := store.recoverIncomplete(&status); err != nil {
		return nil, status, err
	}
	primary := store.readCandidate(store.primary)
	backup := store.readCandidate(store.backup)
	if primary.readFailed {
		status.Degraded, status.Code, status.Source = true, "primary_unreadable", "none"
		causes := []error{primary.err}
		if backup.readFailed {
			causes = append(causes, backup.err)
		}
		return nil, status, safeStoreFailure("host state primary could not be read safely", causes...)
	}
	if backup.readFailed {
		status.Degraded, status.Code, status.Source = true, "backup_unreadable", "none"
		return nil, status, safeStoreFailure("host state backup could not be read safely", backup.err)
	}

	if errors.Is(primary.err, ErrIncompatible) {
		return nil, status, primary.err
	}
	if primary.legacy != nil {
		if errors.Is(backup.err, ErrIncompatible) {
			return nil, status, backup.err
		}
		if backup.err == nil && backup.exists && backup.legacy == nil {
			status.Degraded, status.Code, status.Source = true, "legacy_primary_with_current_backup", "none"
			return nil, status, ErrCorrupt
		}
		if backup.err != nil && backup.exists {
			preserved, preserveErr := store.preserve("corrupt-backup", backup.raw, PhaseRecoveryCorruptionSaved)
			if preserveErr != nil {
				return nil, status, preserveErr
			}
			status.Degraded, status.Code = true, "backup_corrupt_before_migration"
			status.PreservedPaths = append(status.PreservedPaths, preserved)
		}
		if err := store.migrate(primary.raw, *primary.legacy, &status); err != nil {
			return nil, status, err
		}
		store.status = status
		return store, status, nil
	}
	if primary.err == nil && primary.exists {
		if errors.Is(backup.err, ErrIncompatible) {
			return nil, status, backup.err
		}
		store.document = primary.document
		backupNeedsRepair := backup.err != nil || !backup.exists || backup.legacy != nil
		if backup.err != nil && backup.exists {
			preserved, preserveErr := store.preserve("corrupt-backup", backup.raw, PhaseRecoveryCorruptionSaved)
			if preserveErr != nil {
				return nil, status, preserveErr
			}
			status.PreservedPaths = append(status.PreservedPaths, preserved)
			status.Degraded, status.Code = true, "backup_corrupt_repaired"
		}
		if backup.legacy != nil {
			preserved, preserveErr := store.preserve("legacy-backup", backup.raw, PhaseRecoveryCorruptionSaved)
			if preserveErr != nil {
				return nil, status, preserveErr
			}
			status.PreservedPaths = append(status.PreservedPaths, preserved)
			status.Degraded, status.Code = true, "backup_legacy_repaired"
		}
		if backup.err == nil && backup.exists && backup.legacy == nil {
			switch {
			case backup.document.Revision > primary.document.Revision:
				status.Degraded, status.Code, status.Source = true, "backup_revision_ahead", "none"
				return nil, status, ErrCorrupt
			case backup.document.Revision == primary.document.Revision && backup.document.Checksum != primary.document.Checksum:
				status.Degraded, status.Code, status.Source = true, "primary_backup_diverged", "none"
				return nil, status, ErrCorrupt
			case backup.document.Revision < primary.document.Revision && primary.document.Revision-backup.document.Revision > 1:
				preserved, preserveErr := store.preserve("stale-backup", backup.raw, PhaseRecoveryCorruptionSaved)
				if preserveErr != nil {
					return nil, status, preserveErr
				}
				status.PreservedPaths = append(status.PreservedPaths, preserved)
				status.Degraded, status.Code = true, "backup_stale_repaired"
				backupNeedsRepair = true
			}
		}
		if !backup.exists {
			status.Degraded, status.Code = true, "backup_missing_repaired"
		}
		if backupNeedsRepair {
			if err := store.writeDurable(store.backup, primary.raw); err != nil {
				return nil, status, err
			}
		}
		store.status = status
		return store, status, nil
	}

	if primary.exists {
		preserved, preserveErr := store.preserve("corrupt-primary", primary.raw, PhaseRecoveryCorruptionSaved)
		if preserveErr != nil {
			return nil, status, preserveErr
		}
		status.PreservedPaths = append(status.PreservedPaths, preserved)
	}
	if errors.Is(backup.err, ErrIncompatible) {
		return nil, status, backup.err
	}
	if backup.legacy != nil {
		status.Degraded, status.Code, status.Source = true, "primary_recovered_from_legacy_backup", "backup"
		if err := store.migrate(backup.raw, *backup.legacy, &status); err != nil {
			return nil, status, err
		}
		store.status = status
		return store, status, nil
	}
	if backup.err == nil && backup.exists {
		if err := store.writeDurable(store.primary, backup.raw); err != nil {
			return nil, status, err
		}
		if err := store.runHook(PhaseRecoveryBackupRestored); err != nil {
			return nil, status, err
		}
		status.Degraded, status.Source = true, "backup"
		if primary.exists {
			status.Code = "primary_corrupt_recovered_from_backup"
		} else {
			status.Code = "primary_missing_recovered_from_backup"
		}
		store.document, store.status = backup.document, status
		return store, status, nil
	}
	if backup.exists {
		preserved, preserveErr := store.preserve("corrupt-backup", backup.raw, PhaseRecoveryCorruptionSaved)
		if preserveErr != nil {
			return nil, status, preserveErr
		}
		status.PreservedPaths = append(status.PreservedPaths, preserved)
		status.Degraded, status.Code, status.Source = true, "primary_and_backup_unusable", "none"
		return nil, status, ErrCorrupt
	}
	if primary.exists {
		status.Degraded, status.Code, status.Source = true, "primary_corrupt_without_backup", "none"
		return nil, status, ErrCorrupt
	}

	doc, raw, err := sealDocument(State{}, 1, config.Clock())
	if err != nil {
		return nil, status, err
	}
	if err := store.publishInitial(raw); err != nil {
		return nil, status, err
	}
	status.Code, status.Source = "initialized", "initial"
	store.document, store.status = doc, status
	return store, status, nil
}

func (s *Store) Snapshot() (State, uint64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return State{}, 0, ErrClosed
	}
	if s.uncertain {
		return State{}, 0, ErrUncertain
	}
	return cloneState(s.document.State), s.document.Revision, nil
}

func (s *Store) StartupStatus() StartupStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	status := s.status
	status.PreservedPaths = append([]string(nil), status.PreservedPaths...)
	return status
}

func (s *Store) Commit(expectedRevision uint64, next State) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, ErrClosed
	}
	if s.uncertain {
		return 0, ErrUncertain
	}
	if expectedRevision != s.document.Revision {
		return 0, ErrConflict
	}
	next = normalizeState(next)
	if err := next.Validate(); err != nil {
		return 0, safeStoreFailure("host state is invalid", ErrInvalidState, err)
	}
	writtenAt := s.now().UTC()
	if writtenAt.Before(s.document.WrittenAt) {
		writtenAt = s.document.WrittenAt
	}
	doc, raw, err := sealDocument(next, expectedRevision+1, writtenAt)
	if err != nil {
		return 0, err
	}
	if err := s.writeDurable(s.staging, raw); err != nil {
		return 0, &CommitError{Phase: PhaseCommitStaged, Err: err}
	}
	if err := s.runHook(PhaseCommitStaged); err != nil {
		return 0, &CommitError{Phase: PhaseCommitStaged, Err: err}
	}
	currentRaw, err := encodeDocument(s.document)
	if err != nil {
		return 0, &CommitError{Phase: PhaseCommitBackupSynced, Err: err}
	}
	if err := s.writeDurable(s.backup, currentRaw); err != nil {
		return 0, &CommitError{Phase: PhaseCommitBackupSynced, Err: err}
	}
	if err := s.runHook(PhaseCommitBackupSynced); err != nil {
		return 0, &CommitError{Phase: PhaseCommitBackupSynced, Err: err}
	}
	if err := s.writeDurable(s.primary, raw); err != nil {
		changed := atomicWriteMayHaveChanged(err)
		s.uncertain = changed
		return 0, &CommitError{Phase: PhaseCommitPrimarySynced, Changed: changed, Err: err}
	}
	if err := s.runHook(PhaseCommitPrimarySynced); err != nil {
		s.uncertain = true
		return 0, &CommitError{Phase: PhaseCommitPrimarySynced, Changed: true, Err: err}
	}
	if err := s.removeStaging(); err != nil {
		s.uncertain = true
		return 0, &CommitError{Phase: PhaseCommitCleanupSynced, Changed: true, Err: err}
	}
	if err := s.runHook(PhaseCommitCleanupSynced); err != nil {
		s.uncertain = true
		return 0, &CommitError{Phase: PhaseCommitCleanupSynced, Changed: true, Err: err}
	}
	s.document = doc
	return doc.Revision, nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if err := s.lock.Close(); err != nil {
		return safeStoreFailure("host state lock could not be released", err)
	}
	return nil
}

type candidate struct {
	exists     bool
	raw        []byte
	readFailed bool
	document   document
	legacy     *legacyDocumentV0
	err        error
}

func (s *Store) readCandidate(name string) candidate {
	raw, err := readPrivateFile(name, MaxStateBytes)
	if pureNotExist(err) {
		return candidate{}
	}
	if err != nil {
		return candidate{exists: true, readFailed: true, err: safeStoreFailure("host state file could not be read safely", err)}
	}
	doc, legacy, err := decodeAnyDocument(raw)
	return candidate{exists: true, raw: raw, document: doc, legacy: legacy, err: err}
}

func (s *Store) publishInitial(raw []byte) error {
	if err := s.writeDurable(s.staging, raw); err != nil {
		return &CommitError{Phase: PhaseCommitStaged, Err: err}
	}
	if err := s.runHook(PhaseCommitStaged); err != nil {
		return &CommitError{Phase: PhaseCommitStaged, Err: err}
	}
	if err := s.writeDurable(s.primary, raw); err != nil {
		return &CommitError{Phase: PhaseCommitPrimarySynced, Changed: atomicWriteMayHaveChanged(err), Err: err}
	}
	if err := s.runHook(PhaseCommitPrimarySynced); err != nil {
		return &CommitError{Phase: PhaseCommitPrimarySynced, Changed: true, Err: err}
	}
	if err := s.writeDurable(s.backup, raw); err != nil {
		return &CommitError{Phase: PhaseCommitBackupSynced, Changed: true, Err: err}
	}
	if err := s.runHook(PhaseCommitBackupSynced); err != nil {
		return &CommitError{Phase: PhaseCommitBackupSynced, Changed: true, Err: err}
	}
	if err := s.removeStaging(); err != nil {
		return &CommitError{Phase: PhaseCommitCleanupSynced, Changed: true, Err: err}
	}
	if err := s.runHook(PhaseCommitCleanupSynced); err != nil {
		return &CommitError{Phase: PhaseCommitCleanupSynced, Changed: true, Err: err}
	}
	return nil
}

func (s *Store) migrate(source []byte, legacy legacyDocumentV0, status *StartupStatus) error {
	if err := legacy.State.Validate(); err != nil {
		return safeStoreFailure("legacy host state is invalid", ErrCorrupt, err)
	}
	preserved, err := s.preserve("migration-v0", source, PhaseMigrationSourcePreserved)
	if err != nil {
		return err
	}
	status.PreservedPaths = append(status.PreservedPaths, preserved)
	revision := legacy.Revision
	if revision == 0 {
		revision = 1
	}
	writtenAt := s.now().UTC()
	if writtenAt.Before(legacy.WrittenAt) {
		writtenAt = legacy.WrittenAt.UTC()
	}
	doc, raw, err := sealDocument(legacy.State, revision, writtenAt)
	if err != nil {
		return err
	}
	if err := s.writeDurable(s.staging, raw); err != nil {
		return &CommitError{Phase: PhaseMigrationStaged, Err: err}
	}
	if err := s.runHook(PhaseMigrationStaged); err != nil {
		return &CommitError{Phase: PhaseMigrationStaged, Err: err}
	}
	if err := s.writeDurable(s.primary, raw); err != nil {
		return &CommitError{Phase: PhaseMigrationPrimarySynced, Changed: atomicWriteMayHaveChanged(err), Err: err}
	}
	if err := s.runHook(PhaseMigrationPrimarySynced); err != nil {
		return &CommitError{Phase: PhaseMigrationPrimarySynced, Changed: true, Err: err}
	}
	if err := s.writeDurable(s.backup, raw); err != nil {
		return &CommitError{Phase: PhaseMigrationBackupSynced, Changed: true, Err: err}
	}
	if err := s.runHook(PhaseMigrationBackupSynced); err != nil {
		return &CommitError{Phase: PhaseMigrationBackupSynced, Changed: true, Err: err}
	}
	if err := s.removeStaging(); err != nil {
		return &CommitError{Phase: PhaseMigrationCleanupSynced, Changed: true, Err: err}
	}
	if err := s.runHook(PhaseMigrationCleanupSynced); err != nil {
		return &CommitError{Phase: PhaseMigrationCleanupSynced, Changed: true, Err: err}
	}
	status.Code, status.Source = "migrated_v0_to_v1", "migration"
	s.document = doc
	return nil
}

func (s *Store) recoverIncomplete(status *StartupStatus) error {
	raw, err := readPrivateFile(s.staging, MaxStateBytes)
	if pureNotExist(err) {
		return nil
	}
	if err != nil {
		return safeStoreFailure("incomplete host state could not be read", err)
	}
	preserved, err := s.preserve("incomplete-commit", raw, PhaseRecoveryIncompleteSaved)
	if err != nil {
		return err
	}
	if err := os.Remove(s.staging); err != nil && !pureNotExist(err) {
		return safeStoreFailure("incomplete host state could not be removed", err)
	}
	if err := syncDirectory(s.root); err != nil {
		return safeStoreFailure("host state directory could not be synced after recovery", err)
	}
	status.Degraded, status.Code = true, "incomplete_commit_preserved"
	status.PreservedPaths = append(status.PreservedPaths, preserved)
	return nil
}

func (s *Store) preserve(kind string, raw []byte, phase Phase) (string, error) {
	digest := sha256.Sum256(raw)
	name := filepath.Join(s.root, "state."+kind+"."+hex.EncodeToString(digest[:8])+".preserved.json")
	if existing, err := readPrivateFile(name, MaxStateBytes); err == nil {
		if !bytes.Equal(existing, raw) {
			return "", ErrCorrupt
		}
		if err := s.runHook(phase); err != nil {
			return "", &CommitError{Phase: phase, Err: err}
		}
		return name, nil
	} else if !pureNotExist(err) {
		return "", safeStoreFailure("preserved host state could not be checked", err)
	}
	if err := s.writeDurable(name, raw); err != nil {
		return "", &CommitError{Phase: phase, Err: err}
	}
	if err := s.runHook(phase); err != nil {
		return "", &CommitError{Phase: phase, Err: err}
	}
	return name, nil
}

func (s *Store) writeDurable(name string, raw []byte) error {
	if err := atomicfile.Write(name, raw, atomicfile.CurrentOwnerOptions(0o600)); err != nil {
		return safeStoreFailure("host state file could not be written", err)
	}
	return nil
}

func (s *Store) removeStaging() error {
	if err := os.Remove(s.staging); err != nil && !pureNotExist(err) {
		return safeStoreFailure("host state staging file could not be removed", err)
	}
	if err := syncDirectory(s.root); err != nil {
		return safeStoreFailure("host state directory could not be synced", err)
	}
	return nil
}

func (s *Store) runHook(phase Phase) error {
	if s.hook == nil {
		return nil
	}
	return s.hook(phase)
}

func sealDocument(state State, revision uint64, writtenAt time.Time) (document, []byte, error) {
	state = normalizeState(state)
	if revision == 0 || writtenAt.IsZero() {
		return document{}, nil, ErrInvalidState
	}
	if err := state.Validate(); err != nil {
		return document{}, nil, safeStoreFailure("host state is invalid", ErrInvalidState, err)
	}
	unsigned := unsignedDocument{Schema: Schema, SchemaVersion: SchemaVersion, Revision: revision, WrittenAt: writtenAt.UTC(), State: state}
	canonical, err := json.Marshal(unsigned)
	if err != nil {
		return document{}, nil, err
	}
	digest := sha256.Sum256(canonical)
	doc := document{Schema: unsigned.Schema, SchemaVersion: unsigned.SchemaVersion, Revision: unsigned.Revision, WrittenAt: unsigned.WrittenAt, State: unsigned.State, Checksum: "sha256:" + hex.EncodeToString(digest[:])}
	raw, err := encodeDocument(doc)
	return doc, raw, err
}

func encodeDocument(doc document) ([]byte, error) {
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	if len(raw)+1 > MaxStateBytes {
		return nil, ErrInvalidState
	}
	return append(raw, '\n'), nil
}

func decodeAnyDocument(raw []byte) (document, *legacyDocumentV0, error) {
	if len(raw) == 0 || len(raw) > MaxStateBytes {
		return document{}, nil, ErrCorrupt
	}
	if err := validateSingleJSON(raw); err != nil {
		return document{}, nil, safeStoreFailure("host state document is invalid", ErrCorrupt, err)
	}
	var header struct {
		Schema        string `json:"schema"`
		SchemaVersion int    `json:"schema_version"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return document{}, nil, safeStoreFailure("host state document header is invalid", ErrCorrupt, err)
	}
	if header.Schema != Schema {
		return document{}, nil, ErrCorrupt
	}
	if header.SchemaVersion > SchemaVersion {
		return document{}, nil, ErrIncompatible
	}
	if header.SchemaVersion == 0 {
		var legacy legacyDocumentV0
		if err := decodeStrict(raw, &legacy); err != nil {
			return document{}, nil, safeStoreFailure("legacy host state document is invalid", ErrCorrupt, err)
		}
		if legacy.Schema != Schema || legacy.SchemaVersion != 0 {
			return document{}, nil, ErrCorrupt
		}
		if err := legacy.State.Validate(); err != nil {
			return document{}, nil, safeStoreFailure("legacy host state document is invalid", ErrCorrupt, err)
		}
		return document{}, &legacy, nil
	}
	if header.SchemaVersion != SchemaVersion {
		return document{}, nil, ErrCorrupt
	}
	var doc document
	if err := decodeStrict(raw, &doc); err != nil {
		return document{}, nil, safeStoreFailure("host state document is invalid", ErrCorrupt, err)
	}
	if doc.Schema != Schema || doc.SchemaVersion != SchemaVersion || doc.Revision == 0 || doc.WrittenAt.IsZero() {
		return document{}, nil, ErrCorrupt
	}
	if err := doc.State.Validate(); err != nil {
		return document{}, nil, safeStoreFailure("host state document is invalid", ErrCorrupt, err)
	}
	unsigned := unsignedDocument{Schema: doc.Schema, SchemaVersion: doc.SchemaVersion, Revision: doc.Revision, WrittenAt: doc.WrittenAt, State: doc.State}
	canonical, err := json.Marshal(unsigned)
	if err != nil {
		return document{}, nil, ErrCorrupt
	}
	digest := sha256.Sum256(canonical)
	if doc.Checksum != "sha256:"+hex.EncodeToString(digest[:]) {
		return document{}, nil, ErrCorrupt
	}
	doc.State = normalizeState(doc.State)
	return doc, nil, nil
}

func decodeStrict(raw []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return err
		}
		return ErrCorrupt
	}
	return nil
}

func validateSingleJSON(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if _, err := decodeJSONValue(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err != nil {
			return err
		}
		return ErrCorrupt
	}
	return nil
}

func atomicWriteMayHaveChanged(err error) bool {
	var atomicErr *atomicfile.Error
	return errors.As(err, &atomicErr) && atomicErr.Stage == atomicfile.StageSyncDir
}
