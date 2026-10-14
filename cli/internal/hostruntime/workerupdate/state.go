package workerupdate

import (
	"errors"
	"os"
	"reflect"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updateflow"
)

const TransactionSchemaV1 = "paperboat.update-transaction/v1"

// TransactionState is the bounded safe projection used by local status,
// diagnostics, and lifecycle events. It never exposes paths, capabilities, or
// artifact URLs.
type TransactionState struct {
	Schema           string             `json:"schema,omitempty"`
	TransactionID    string             `json:"transaction_id,omitempty"`
	Stage            updateflow.Stage   `json:"stage,omitempty"`
	ActiveVersion    string             `json:"active_version,omitempty"`
	ActiveDigest     string             `json:"active_digest,omitempty"`
	ActiveLength     int64              `json:"active_length,omitempty"`
	CandidateVersion string             `json:"candidate_version,omitempty"`
	Failure          updateflow.Failure `json:"failure,omitempty"`
	UpdatedAt        time.Time          `json:"updated_at,omitempty"`
	HealthDeadline   time.Time          `json:"health_deadline,omitempty"`
	RollbackCount    uint32             `json:"rollback_count,omitempty"`
	Quarantined      bool               `json:"quarantined,omitempty"`
}

func (m *Manager) TransactionState() (TransactionState, error) {
	if m == nil {
		return TransactionState{}, ErrInvalidConfig
	}
	journal, err := updateflow.Load(m.config.StatePath)
	if err != nil {
		if journalMissingOnly(err) {
			return TransactionState{Schema: TransactionSchemaV1, Stage: updateflow.StageIdle, ActiveVersion: m.ActiveVersion(), ActiveDigest: m.active.SHA256, ActiveLength: m.active.Length}, nil
		}
		return TransactionState{}, err
	}
	return TransactionState{
		Schema: TransactionSchemaV1, TransactionID: journal.TransactionID, Stage: journal.Stage,
		ActiveVersion: journal.ActiveVersion, CandidateVersion: journal.CandidateVersion,
		ActiveDigest: journal.ActiveDigest, ActiveLength: journal.ActiveLength,
		Failure: journal.LastFailure, UpdatedAt: journal.StageUpdatedAt,
		HealthDeadline: journal.HealthDeadline, RollbackCount: journal.RollbackCount,
		Quarantined: journal.Stage == updateflow.StageIdle && journal.CandidateVersion != "" && journal.LastFailure != updateflow.FailureNone,
	}, nil
}

const maxJournalCauseNodes = 16

// journalMissingOnly returns true only when a bounded error tree has at least
// one leaf and every leaf classifies as a missing-file error. A joined read or
// close failure must never authorize first-run initialization or stale-state
// recovery.
func journalMissingOnly(err error) bool {
	if nilJournalError(err) {
		return false
	}
	pending := []error{err}
	visited, hasLeaf := 0, false
	for len(pending) > 0 {
		if visited >= maxJournalCauseNodes {
			return false
		}
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if nilJournalError(current) {
			return false
		}
		visited++

		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			causes := wrapped.Unwrap()
			if len(causes) == 0 || len(causes) > maxJournalCauseNodes-visited-len(pending) {
				return false
			}
			for _, cause := range causes {
				if nilJournalError(cause) {
					return false
				}
			}
			pending = append(pending, causes...)
		case interface{ Unwrap() error }:
			cause := wrapped.Unwrap()
			if nilJournalError(cause) || len(pending)+1 > maxJournalCauseNodes-visited {
				return false
			}
			pending = append(pending, cause)
		default:
			hasLeaf = true
			if !errors.Is(current, os.ErrNotExist) {
				return false
			}
		}
	}
	return hasLeaf
}

func nilJournalError(err error) bool {
	if err == nil {
		return true
	}
	value := reflect.ValueOf(err)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
