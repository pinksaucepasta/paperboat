package connectorrotation

import (
	"errors"
	"os"
	"reflect"
	"syscall"
)

var (
	ErrInvalidConfig      = errors.New("invalid connector rotation configuration")
	ErrOperationConflict  = errors.New("connector rotation operation conflicts with durable state")
	ErrOperationNotFound  = errors.New("connector rotation operation was not found")
	ErrRecoveryRequired   = errors.New("connector rotation requires recovery")
	ErrKeyUnavailable     = errors.New("connector rotation key is unavailable")
	ErrInstallationFailed = errors.New("replacement credential installation failed")
	ErrRevocationFailed   = errors.New("old credential revocation failed")
	ErrStaleMessage       = errors.New("connector rotation message is stale")
	ErrAlreadyTerminal    = errors.New("connector rotation is already terminal")
	ErrJournalCorrupt     = errors.New("connector rotation journal is corrupt")
	ErrJournalUncertain   = errors.New("connector rotation journal outcome is uncertain")
	errJournalSecurity    = errors.New("connector rotation journal security is invalid")
)

const (
	journalVersion       = 1
	maxJournalRecords    = 64
	maxJournalBytes      = 1 << 20
	maxJournalErrorNodes = 16
)

type safeRotationCause struct {
	message string
	cause   error
}

func (e *safeRotationCause) Error() string { return e.message }

func (e *safeRotationCause) Unwrap() error { return e.cause }

func safeRotationFailure(message string, cause error) error {
	if cause == nil {
		return nil
	}
	return &safeRotationCause{message: message, cause: cause}
}

func journalOperationFailure(cause error) error {
	if cause == nil {
		return nil
	}
	return safeRotationFailure("connector rotation journal operation failed", errors.Join(ErrJournalUncertain, cause))
}

// errorLeavesMatch authorizes an expected state only when every bounded leaf
// in its cause tree proves that state. In particular, os.ErrNotExist joined
// with an I/O or cleanup failure is not absence. The bounded walk also rejects
// cycles, typed-nil causes, and unexpectedly large custom error trees.
func errorLeavesMatch(err error, expected ...error) bool {
	if err == nil || len(expected) == 0 {
		return false
	}
	pending := []error{err}
	visited := 0
	for len(pending) > 0 {
		if visited >= maxJournalErrorNodes {
			return false
		}
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		visited++
		if current == nil || typedNilError(current) {
			return false
		}
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			causes := wrapped.Unwrap()
			if len(causes) == 0 || len(pending)+len(causes) > maxJournalErrorNodes-visited {
				return false
			}
			for _, cause := range causes {
				if cause == nil || typedNilError(cause) {
					return false
				}
				pending = append(pending, cause)
			}
			continue
		case interface{ Unwrap() error }:
			cause := wrapped.Unwrap()
			if cause != nil {
				if typedNilError(cause) || len(pending)+1 > maxJournalErrorNodes-visited {
					return false
				}
				pending = append(pending, cause)
				continue
			}
		}
		matched := false
	forTarget:
		for _, target := range expected {
			if target == nil || typedNilError(target) {
				continue
			}
			currentType, targetType := reflect.TypeOf(current), reflect.TypeOf(target)
			if currentType.Comparable() && targetType.Comparable() && current == target {
				matched = true
				break forTarget
			}
			if errno, ok := current.(syscall.Errno); ok && nativeStateTarget(target) && errors.Is(errno, target) {
				matched = true
				break forTarget
			}
		}
		if !matched {
			return false
		}
	}
	return visited > 0
}

func nativeStateTarget(target error) bool {
	return errorIdentityMatches(target, os.ErrNotExist) || errorIdentityMatches(target, os.ErrPermission) || errorIdentityMatches(target, os.ErrExist)
}

func errorIdentityMatches(left, right error) bool {
	if left == nil || right == nil || typedNilError(left) || typedNilError(right) {
		return false
	}
	leftType, rightType := reflect.TypeOf(left), reflect.TypeOf(right)
	return leftType.Comparable() && rightType.Comparable() && left == right
}

func typedNilError(err error) bool {
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
	PhaseChallenged    Phase = "challenged"
	PhaseProofAccepted Phase = "proof_accepted"
	PhaseInstalled     Phase = "installed"
	PhaseReady         Phase = "ready"
	PhaseRevoking      Phase = "revoking"
	PhaseRevoked       Phase = "revoked"
)

func phaseAtLeast(value, threshold Phase) bool {
	order := map[Phase]int{
		PhaseChallenged:    1,
		PhaseProofAccepted: 2,
		PhaseInstalled:     3,
		PhaseReady:         4,
		PhaseRevoking:      5,
		PhaseRevoked:       6,
	}
	return order[value] >= order[threshold]
}

func validPhase(p Phase) bool {
	switch p {
	case PhaseChallenged, PhaseProofAccepted, PhaseInstalled, PhaseReady, PhaseRevoking, PhaseRevoked:
		return true
	default:
		return false
	}
}
