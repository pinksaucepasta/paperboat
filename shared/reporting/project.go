package reporting

import (
	"context"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"reflect"
	"syscall"
)

// Projection is the bounded, content-free reading of an error tree. It never
// calls Error and never holds an error string.
type Projection struct {
	Cause      string
	ErrorType  string
	ErrorChain []string
	Errno      int
	HTTPStatus int
	// Rejected is true when every terminal branch is an expected 4xx rejection.
	Rejected bool
}

// NodeClassifier names the cause of one error node; rank 0 means it does not know.
type NodeClassifier func(err error) (cause string, errno, rank int)

// Options lets one binary add its own node classes and observe each node.
type Options struct {
	// Classify runs before the built-in classes, in order; the first rank > 0 wins.
	Classify []NodeClassifier
	// Visit sees every non-nil node once, in traversal order (stage/code annotations).
	Visit func(err error)
}

const (
	maxNodes = 16
	// MaxErrorChain bounds the type chain a fault carries.
	MaxErrorChain = 8
)

// Project is the one error-tree traversal and cause ranking every binary uses.
// It visits at most sixteen nodes (joined errors included, cycles cut) and
// ranks causes (a native errno outranks a synthesized HTTP status): the highest rank names the failure, a 4xx status that is not
// the only terminal branch yields to the best operational cause, and
// cancellation only counts when every terminal branch is canceled.
func Project(err error, opts Options) (p Projection) {
	p = Projection{Cause: "internal", ErrorType: "error"}
	if err == nil {
		return p
	}
	// A broken Error, Unwrap or classifier must not interrupt cleanup or hide the failure.
	defer func() {
		if recover() != nil {
			p = Projection{Cause: "internal", ErrorType: "error"}
		}
	}()
	remaining := []error{err}
	seen := make(map[error]bool)
	priority, nativeErrno, nativePriority, httpPriority, operationalPriority := 0, 0, 0, 0, 0
	operationalCause, operationalErrno, operationalType := "internal", 0, "error"
	for count := 0; len(remaining) > 0 && count < maxNodes; count++ {
		current := remaining[0]
		remaining = remaining[1:]
		if current == nil {
			continue
		}
		typ := reflect.TypeOf(current)
		if typ.Comparable() {
			if seen[current] {
				continue
			}
			seen[current] = true
		}
		name := safeErrorType(typ)
		if len(p.ErrorChain) < MaxErrorChain && (len(p.ErrorChain) == 0 || p.ErrorChain[len(p.ErrorChain)-1] != name) {
			p.ErrorChain = append(p.ErrorChain, name)
		}
		if value := reflect.ValueOf(current); value.Kind() == reflect.Pointer && value.IsNil() {
			continue
		}
		if opts.Visit != nil {
			opts.Visit(current)
		}
		cause, errno, rank := nodeCause(current, opts.Classify)
		// A deadline can be the primary cause while cleanup also failed with a
		// native error. Preserve that independent diagnostic evidence.
		if errno != 0 && rank > nativePriority {
			nativeErrno, nativePriority = errno, rank
		}
		status := 0
		if coded, ok := current.(interface{ DiagnosticStatus() int }); ok {
			status = coded.DiagnosticStatus()
		}
		if status >= 400 && status <= 599 {
			statusRank := rank
			if status >= 500 {
				statusRank++
			}
			if statusRank > httpPriority {
				p.HTTPStatus, httpPriority = status, statusRank
			}
		}
		if rank > priority {
			p.Cause, p.Errno, p.ErrorType, priority = cause, errno, name, rank
		}
		if rank > operationalPriority && cause != "context_canceled" && !(status >= 400 && status < 500) {
			operationalCause, operationalErrno, operationalType, operationalPriority = cause, errno, name, rank
		}
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			children := wrapped.Unwrap()
			if len(children) > maxNodes-count {
				children = children[:maxNodes-count]
			}
			remaining = append(remaining, children...)
		case interface{ Unwrap() error }:
			remaining = append(remaining, wrapped.Unwrap())
		}
	}
	if p.HTTPStatus >= 400 && p.HTTPStatus < 500 {
		if rejectionOnly(err) {
			p.Rejected = true
		} else {
			p.Cause, p.Errno, p.ErrorType = operationalCause, operationalErrno, operationalType
		}
	}
	if p.Cause == "context_canceled" && !cancellationOnly(err) {
		p.Cause = "internal"
	}
	if nativeErrno != 0 {
		p.Errno = nativeErrno
	}
	return p
}

func nodeCause(err error, extra []NodeClassifier) (string, int, int) {
	for _, classify := range extra {
		if cause, errno, rank := classify(err); rank > 0 {
			return cause, errno, rank
		}
	}
	if _, ok := err.(interface{ processPanic() }); ok {
		return "process_panic", 0, 95
	}
	if sameError(err, context.Canceled) {
		return "context_canceled", 0, 1
	}
	if sameError(err, context.DeadlineExceeded) {
		return "deadline_exceeded", 0, 90
	}
	if status, ok := err.(interface{ DiagnosticStatus() int }); ok {
		if code := status.DiagnosticStatus(); code >= http.StatusBadRequest && code <= 599 {
			return StatusCause(code), 0, 63
		}
	}
	if cause, rank := domainCause(err); rank > 0 {
		return cause, 0, rank
	}
	switch {
	case sameError(err, http.ErrServerClosed):
		return "server_closed", 0, 81
	case sameError(err, os.ErrNotExist), sameError(err, fs.ErrNotExist):
		return "not_found", 0, 65
	case sameError(err, os.ErrPermission), sameError(err, fs.ErrPermission):
		return "permission_denied", 0, 65
	case sameError(err, net.ErrClosed):
		return "connection_closed", 0, 65
	case sameError(err, io.EOF):
		return "end_of_stream", 0, 65
	case sameError(err, io.ErrUnexpectedEOF):
		return "truncated_stream", 0, 65
	}
	if errno, ok := err.(syscall.Errno); ok {
		// Errno.Is understands native Windows file/path/ACL errors as well as Unix values.
		switch {
		case errno.Is(fs.ErrNotExist):
			return "not_found", int(errno), 64
		case errno.Is(fs.ErrPermission):
			return "permission_denied", int(errno), 64
		case errno.Is(fs.ErrExist):
			return "conflict", int(errno), 64
		}
		if cause := ErrnoCause(errno); cause != "system_error" {
			return cause, int(errno), 64
		}
		return "system_error", int(errno), 60
	}
	if dns, ok := err.(*net.DNSError); ok {
		if dns.IsTimeout {
			return "network_timeout", 0, 50
		}
		return "dns_failure", 0, 50
	}
	if network, ok := err.(net.Error); ok && network.Timeout() {
		return "network_timeout", 0, 40
	}
	// Wrappers do not turn a pure cancellation into a substantive failure;
	// an unknown terminal cause does.
	switch err.(type) {
	case interface{ Unwrap() error }, interface{ Unwrap() []error }:
		return "internal", 0, 0
	}
	return "internal", 0, 2
}

// Rejection is expected only when every bounded terminal branch is a protocol
// rejection or ordinary cancellation; a status wrapper cannot hide an
// independent storage, decoding or network failure beneath it.
func rejectionOnly(err error) bool {
	pending := []error{err}
	sawStatus := false
	for visited := 0; len(pending) > 0; visited++ {
		if visited >= maxNodes {
			return false
		}
		current := pending[0]
		pending = pending[1:]
		if current == nil {
			return false
		}
		value := reflect.ValueOf(current)
		if value.Kind() == reflect.Pointer && value.IsNil() {
			return false
		}
		if value.Type().Comparable() && current == context.Canceled {
			continue
		}
		rejected := false
		if coded, ok := current.(interface{ DiagnosticStatus() int }); ok {
			status := coded.DiagnosticStatus()
			rejected = status >= 400 && status < 500
		}
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			children := wrapped.Unwrap()
			if len(children) == 0 || len(children)+len(pending) > maxNodes-1-visited {
				return false
			}
			pending = append(pending, children...)
		case interface{ Unwrap() error }:
			child := wrapped.Unwrap()
			if child == nil && rejected {
				sawStatus = true
				continue
			}
			pending = append(pending, child)
		default:
			if !rejected {
				return false
			}
			sawStatus = true
		}
	}
	return sawStatus
}

// Cancellation is orderly only when every bounded terminal cause is canceled.
// Unresolved cycles, oversized joins and typed nils cannot prove that claim.
func cancellationOnly(err error) bool {
	remaining := []error{err}
	for visited := 0; len(remaining) > 0; visited++ {
		if visited >= maxNodes {
			return false
		}
		current := remaining[0]
		remaining = remaining[1:]
		if current == nil {
			return false
		}
		value := reflect.ValueOf(current)
		if value.Kind() == reflect.Pointer && value.IsNil() {
			return false
		}
		if value.Type().Comparable() && current == context.Canceled {
			continue
		}
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			children := wrapped.Unwrap()
			if len(children) == 0 || len(children)+len(remaining) > maxNodes-1-visited {
				return false
			}
			remaining = append(remaining, children...)
		case interface{ Unwrap() error }:
			remaining = append(remaining, wrapped.Unwrap())
		default:
			return false
		}
	}
	return true
}
