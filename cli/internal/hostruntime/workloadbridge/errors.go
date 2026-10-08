package workloadbridge

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/execprocess"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/history"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/store"
	"io"
	"os"
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

func encodeError(err error) *wireError {
	var stale *session.StaleGenerationError
	if errors.As(err, &stale) {
		return &wireError{Code: "stale_generation", CurrentGeneration: stale.CurrentGeneration}
	}
	var gap *history.GapError
	if errors.As(err, &gap) {
		return &wireError{Code: "history_gap", Requested: gap.RequestedSequence, Earliest: gap.EarliestSequence, Latest: gap.LatestSequence}
	}
	var storeGap *store.GapError
	if errors.As(err, &storeGap) {
		return &wireError{Code: "store_gap", Requested: storeGap.Requested, Earliest: storeGap.Earliest, Latest: storeGap.Latest}
	}
	for code, value := range errorCodes {
		if errors.Is(err, value) {
			return &wireError{Code: code}
		}
	}
	return &wireError{Code: "owner_operation_failed"}
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
