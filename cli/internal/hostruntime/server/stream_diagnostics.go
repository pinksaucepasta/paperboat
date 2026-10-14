package server

import (
	"context"
	"io"
	"net"
	"reflect"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/browserbroadcastserver"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/execprocess"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/history"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
)

func streamFailureClassification(capability string) (operation, code string, ok bool) {
	switch capability {
	case "terminal.v1":
		return "sessions", "terminal_session_failed", true
	case "exec.v1":
		return "exec", "exec_operation_failed", true
	case "ssh.v1":
		return "ssh", "managed_ssh_failed", true
	default:
		return "", "", false
	}
}

func reportStreamFailure(ctx context.Context, capability, stage string, err error) {
	if err == nil || expectedStreamFailure(err) {
		return
	}
	operation, code, ok := streamFailureClassification(capability)
	if !ok {
		return
	}
	if allErrorLeavesMatch(err, func(leaf error) bool { return leaf == context.DeadlineExceeded }) {
		errorreport.Current().ObserveFailure(ctx, "paperboat-daemon", operation, stage, code, err)
		return
	}
	errorreport.Current().CaptureFailure(ctx, "paperboat-daemon", operation, stage, code, err)
}

func reportStreamCloseFailure(ctx context.Context, capability string, err error) {
	reportStreamFailure(ctx, capability, "lifecycle", err)
}

func reportStreamOpenFailure(ctx context.Context, capability string, err error) {
	if err == nil {
		return
	}
	operation, code, ok := streamFailureClassification(capability)
	if !ok || expectedStreamOpenFailure(err) {
		return
	}
	if allErrorLeavesMatch(err, func(leaf error) bool { return leaf == context.DeadlineExceeded }) {
		errorreport.Current().ObserveFailure(ctx, "paperboat-daemon", operation, "stream_open", code, err)
		return
	}
	errorreport.Current().CaptureFailure(ctx, "paperboat-daemon", operation, "stream_open", code, err)
}

func expectedStreamFailure(err error) bool {
	return allErrorLeavesMatch(err, normalStreamFailureLeaf)
}

func expectedServerTermination(err error) bool {
	if err == nil {
		return false
	}
	type node struct {
		err  error
		path map[error]struct{}
	}
	stack := []node{{err: err}}
	visited := 0
	for len(stack) > 0 {
		if visited >= 16 {
			return false
		}
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if current.err == nil {
			return false
		}
		value := reflect.ValueOf(current.err)
		if nilable(value.Kind()) && value.IsNil() {
			return false
		}
		visited++
		path := current.path
		typeOf := reflect.TypeOf(current.err)
		if typeOf.Comparable() {
			if _, cycle := path[current.err]; cycle {
				return false
			}
			next := make(map[error]struct{}, len(path)+1)
			for previous := range path {
				next[previous] = struct{}{}
			}
			next[current.err] = struct{}{}
			path = next
		}
		if normalStreamFailureLeaf(current.err) {
			continue
		}
		if protocolErrorIsExpected(current.err) {
			continue
		}
		switch wrapped := current.err.(type) {
		case interface{ Unwrap() []error }:
			children := wrapped.Unwrap()
			if len(children) == 0 || len(children)+len(stack) > 16-visited {
				return false
			}
			for index := len(children) - 1; index >= 0; index-- {
				stack = append(stack, node{err: children[index], path: path})
			}
		case interface{ Unwrap() error }:
			child := wrapped.Unwrap()
			if child == nil || len(stack)+1 > 16-visited {
				return false
			}
			stack = append(stack, node{err: child, path: path})
		default:
			return false
		}
	}
	return visited > 0
}

func protocolErrorIsExpected(err error) bool {
	protocolFailure, ok := err.(*protocol.Error)
	if !ok || protocolFailure == nil {
		return false
	}
	switch protocolFailure.Code {
	case protocol.Malformed, protocol.Oversized, protocol.UnsupportedMessage, protocol.InvalidFrame,
		protocol.CapabilityRequired, protocol.InvalidDeadline, protocol.CredentialExpired:
		return true
	default:
		return false
	}
}

func reportConsumedServeFailure(ctx context.Context, err error) {
	if err == nil || expectedServerTermination(err) {
		return
	}
	errorreport.Current().CaptureFailure(ctx, "paperboat-daemon", "daemon", "lifecycle", "service_failed", err)
}

func expectedStreamOpenFailure(err error) bool {
	return allErrorLeavesMatch(err, func(leaf error) bool {
		if normalStreamFailureLeaf(leaf) {
			return true
		}
		switch leaf {
		case session.ErrSessionExists, session.ErrSessionUnknown, session.ErrSessionRunning,
			session.ErrManagerStopped, session.ErrResourceLimit, session.ErrUpdateBusy,
			session.ErrUpdateInProgress, session.ErrStaleGeneration, session.ErrInvalidTransition,
			session.ErrAttachmentExists, session.ErrAttachmentUnknown, session.ErrAttachmentEvicted,
			session.ErrInvalidInput, session.ErrInputConflict, session.ErrInputSequence,
			session.ErrInputUncertain, session.ErrInputUnknown, session.ErrInputJournalFull,
			execprocess.ErrInvalid, execprocess.ErrConflict, execprocess.ErrCapacity,
			execprocess.ErrNotFound, execprocess.ErrReplayUnavailable,
			managedssh.ErrSSHHostInvalid, managedssh.ErrSSHHostStale, managedssh.ErrSSHHostBusy,
			browserbroadcastserver.ErrUnavailable:
			return true
		}
		_, isHistoryGap := leaf.(*history.GapError)
		_, isStaleGeneration := leaf.(*session.StaleGenerationError)
		return isHistoryGap || isStaleGeneration
	})
}

func normalStreamFailureLeaf(err error) bool {
	switch err {
	case context.Canceled, io.EOF, io.ErrClosedPipe, net.ErrClosed, ErrStreamClosed, ErrServerStopped:
		return true
	default:
		return false
	}
}

// allErrorLeavesMatch is deliberately bounded and conservative. A cycle,
// typed nil, empty join, or oversized chain cannot prove that a failure is
// only an expected stream termination.
func allErrorLeavesMatch(err error, match func(error) bool) bool {
	if err == nil || match == nil {
		return false
	}
	type node struct {
		err  error
		path map[error]struct{}
	}
	stack := []node{{err: err}}
	visited := 0
	for len(stack) > 0 {
		if visited >= 16 {
			return false
		}
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if current.err == nil {
			return false
		}
		value := reflect.ValueOf(current.err)
		if nilable(value.Kind()) && value.IsNil() {
			return false
		}
		visited++
		path := current.path
		typeOf := reflect.TypeOf(current.err)
		if typeOf.Comparable() {
			if _, cycle := path[current.err]; cycle {
				return false
			}
			next := make(map[error]struct{}, len(path)+1)
			for previous := range path {
				next[previous] = struct{}{}
			}
			next[current.err] = struct{}{}
			path = next
		}
		switch wrapped := current.err.(type) {
		case interface{ Unwrap() []error }:
			children := wrapped.Unwrap()
			if len(children) == 0 || len(children)+len(stack) > 16-visited {
				return false
			}
			for index := len(children) - 1; index >= 0; index-- {
				stack = append(stack, node{err: children[index], path: path})
			}
		case interface{ Unwrap() error }:
			child := wrapped.Unwrap()
			if child == nil || len(stack)+1 > 16-visited {
				return false
			}
			stack = append(stack, node{err: child, path: path})
		default:
			if !match(current.err) {
				return false
			}
		}
	}
	return true
}

func nilable(kind reflect.Kind) bool {
	switch kind {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return true
	default:
		return false
	}
}
