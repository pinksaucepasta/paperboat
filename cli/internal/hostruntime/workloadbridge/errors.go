package workloadbridge

import (
	"context"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/execprocess"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/history"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/store"
	"io"
	"os"
	"reflect"
)

type wireError struct {
	Code              string
	CurrentGeneration uint64
	Requested         uint64
	Earliest          uint64
	Latest            uint64
}

var errorCodes = map[string]error{
	"maintenance_busy": hostdproto.ErrMaintenanceBusy, "input_deadline": os.ErrDeadlineExceeded,
	"fenced": ErrFenced, "eof": io.EOF, "canceled": context.Canceled, "deadline": context.DeadlineExceeded,
	"session.ErrInvalidInput":          session.ErrInvalidInput,
	"session.ErrInputConflict":         session.ErrInputConflict,
	"session.ErrInputSequence":         session.ErrInputSequence,
	"session.ErrInputUncertain":        session.ErrInputUncertain,
	"session.ErrStaleGeneration":       session.ErrStaleGeneration,
	"session.ErrInputUnknown":          session.ErrInputUnknown,
	"session.ErrInputJournalFull":      session.ErrInputJournalFull,
	"session.ErrSessionExists":         session.ErrSessionExists,
	"session.ErrSessionUnknown":        session.ErrSessionUnknown,
	"session.ErrSessionRunning":        session.ErrSessionRunning,
	"session.ErrInvalidSession":        session.ErrInvalidSession,
	"session.ErrManagerStopped":        session.ErrManagerStopped,
	"session.ErrResourceLimit":         session.ErrResourceLimit,
	"session.ErrUpdateBusy":            session.ErrUpdateBusy,
	"session.ErrUpdateInProgress":      session.ErrUpdateInProgress,
	"session.ErrAttachmentExists":      session.ErrAttachmentExists,
	"session.ErrAttachmentUnknown":     session.ErrAttachmentUnknown,
	"session.ErrAttachmentEvicted":     session.ErrAttachmentEvicted,
	"session.ErrOutputOrder":           session.ErrOutputOrder,
	"session.ErrInvalidQueueLimit":     session.ErrInvalidQueueLimit,
	"execprocess.ErrInvalid":           execprocess.ErrInvalid,
	"execprocess.ErrConflict":          execprocess.ErrConflict,
	"execprocess.ErrCapacity":          execprocess.ErrCapacity,
	"execprocess.ErrUpdateInProgress":  execprocess.ErrUpdateInProgress,
	"execprocess.ErrUpdateUnavailable": execprocess.ErrUpdateUnavailable,
	"execprocess.ErrNotFound":          execprocess.ErrNotFound,
	"execprocess.ErrReplayUnavailable": execprocess.ErrReplayUnavailable,
	"history.ErrInvalidLimit":          history.ErrInvalidLimit,
	"history.ErrInvalidCursor":         history.ErrInvalidCursor,
	"history.ErrSequenceFull":          history.ErrSequenceFull,
	"pty.ErrInvalidCommand":            pty.ErrInvalidCommand,
	"pty.ErrInvalidCWD":                pty.ErrInvalidCWD,
	"pty.ErrInvalidDimensions":         pty.ErrInvalidDimensions,
	"pty.ErrInvalidSignal":             pty.ErrInvalidSignal,
	"store.ErrIncompatible":            store.ErrIncompatible,
	"store.ErrCorrupt":                 store.ErrCorrupt,
	"store.ErrConflict":                store.ErrConflict,
	"store.ErrNotFound":                store.ErrNotFound,
	"store.ErrReplayGap":               store.ErrReplayGap,
	"store.ErrResultTooLarge":          store.ErrResultTooLarge,
}

// Only a complete, consistent cause tree may become a wire rejection.
// Independent native faults stay operational; no original prose crosses IPC.
func encodeError(err error) *wireError {
	pending := []error{err}
	var projection *wireError
	for visited := 0; len(pending) > 0; visited++ {
		if visited >= 16 {
			return &wireError{Code: "owner_operation_failed"}
		}
		node := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if node == nil {
			return &wireError{Code: "owner_operation_failed"}
		}
		value := reflect.ValueOf(node)
		switch value.Kind() {
		case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
			if value.IsNil() {
				return &wireError{Code: "owner_operation_failed"}
			}
		}
		var leaf *wireError
		switch typed := node.(type) {
		case *session.StaleGenerationError:
			if typed != nil {
				leaf = &wireError{Code: "stale_generation", CurrentGeneration: typed.CurrentGeneration}
			}
		case *history.GapError:
			if typed != nil {
				leaf = &wireError{Code: "history_gap", Requested: typed.RequestedSequence, Earliest: typed.EarliestSequence, Latest: typed.LatestSequence}
			}
		case *store.GapError:
			if typed != nil {
				leaf = &wireError{Code: "store_gap", Requested: typed.Requested, Earliest: typed.Earliest, Latest: typed.Latest}
			}
		default:
			if multi, ok := node.(interface{ Unwrap() []error }); ok {
				children := multi.Unwrap()
				if len(children) == 0 || len(children) > 16-len(pending) {
					return &wireError{Code: "owner_operation_failed"}
				}
				pending = append(pending, children...)
				continue
			}
			if unary, ok := node.(interface{ Unwrap() error }); ok {
				pending = append(pending, unary.Unwrap())
				continue
			}
			for code, value := range errorCodes {
				if node == value {
					leaf = &wireError{Code: code}
					break
				}
			}
		}
		if leaf == nil || projection != nil && *projection != *leaf {
			return &wireError{Code: "owner_operation_failed"}
		}
		projection = leaf
	}
	if projection == nil {
		return &wireError{Code: "owner_operation_failed"}
	}
	return projection
}
func decodeError(err *wireError) error {
	switch err.Code {
	case "stale_generation":
		return &session.StaleGenerationError{CurrentGeneration: err.CurrentGeneration}
	case "history_gap":
		return &history.GapError{RequestedSequence: err.Requested, EarliestSequence: err.Earliest, LatestSequence: err.Latest}
	case "store_gap":
		return &store.GapError{Requested: err.Requested, Earliest: err.Earliest, Latest: err.Latest}
	}
	if value, ok := errorCodes[err.Code]; ok {
		return value
	}
	return session.ErrOwnerUnavailable
}
