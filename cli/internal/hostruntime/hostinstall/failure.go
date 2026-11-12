package hostinstall

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"reflect"
	"slices"
	"strings"
	"syscall"

	"github.com/pinksaucepasta/paperboat/internal/diskspace"
)

// ErrLegacyCarrierRunning is a refusal to perform the first independent
// carrier transition while the installation still runs an embedded peer.
var ErrLegacyCarrierRunning = errors.New("This first update requires the existing Paperboat runtime to be stopped. Finish active sessions and SSH connections before stopping the native service, then retry.")

// ErrInsufficientSpace is a refusal because the disk that holds the installation has no room for the
// file being staged. Staging removes its partial file, so nothing was changed.
var ErrInsufficientSpace = errors.New("there is not enough free disk space to stage the installation")

// ErrNoPendingInstallation is a rollback that found no installation journal: the install already committed (a bound
// install commits inside install-supplied), so there is nothing to undo.
var ErrNoPendingInstallation = errors.New("the installation already committed, so there is nothing to roll back")

// ErrUpdateNotReady is an installation whose new version did not come up healthy before it was
// committed; the previous version was restored.
var ErrUpdateNotReady = errors.New("the new version did not become ready, so the previous version was restored")

// UpdateNotReady is the rollback of a new version that failed validation; cause is the failed step.
func UpdateNotReady(cause error) error {
	return hostInstallFailure("the new version did not become ready; restoring the previous version", ErrUpdateNotReady, cause)
}

// notReadyIfRolledBack names a replacement whose candidate failed after the services were replaced and whose previous
// version was then restored (Unix does the same in install_unix.go). A rollback that did not finish keeps its own error,
// because the previous version is then not known to be running; a full disk keeps its own message.
func notReadyIfRolledBack(err error, candidateFailed, recovered bool) error {
	if err == nil || !candidateFailed || !recovered || diskFull(err) {
		return err
	}
	return UpdateNotReady(err)
}

// diskFull reports a write that failed because the disk or the user's quota is full.
func diskFull(err error) bool {
	return diskspace.IsFull(err)
}

// installFailure keeps diagnostics available to the existing final caller while
// excluding paths, process output, identities and parser input from presentation.
type installFailure struct {
	message string
	causes  []error
}

func (e *installFailure) Error() string   { return e.message }
func (e *installFailure) Unwrap() []error { return e.causes }
func hostInstallFailure(message string, causes ...error) error {
	retained := make([]error, 0, len(causes))
	for _, cause := range causes {
		if cause != nil {
			retained = append(retained, cause)
		}
	}
	if len(retained) == 0 {
		return nil
	}
	return &installFailure{message: message, causes: retained}
}

func decodeInstallJSON(reader io.Reader, target any, maximum int64) error {
	if reader == nil {
		return ErrInvalidRequest
	}
	body, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil {
		return hostInstallFailure("reading the installation request failed", ErrInvalidRequest, err)
	}
	if int64(len(body)) > maximum {
		return hostInstallFailure("installation request exceeds its size limit", ErrInvalidRequest)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return hostInstallFailure("decoding the installation request failed", ErrInvalidRequest, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return hostInstallFailure("installation request contains multiple values", ErrInvalidRequest)
		}
		return hostInstallFailure("finishing the installation request failed", ErrInvalidRequest, err)
	}
	return nil
}

func readInstallFile(path string, maximum int64) (body []byte, resultErr error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, hostInstallFailure("opening installation state failed", err)
	}
	defer func() {
		resultErr = withCleanup("reading or closing installation state failed", resultErr, file.Close())
		if resultErr != nil {
			body = nil
		}
	}()
	body, err = io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maximum {
		return nil, ErrInvalidRequest
	}
	return body, nil
}

// A missing object authorizes recovery only when the complete bounded cause
// tree proves absence. Cleanup or native probe failures must fail closed.
func installNotExistOnly(err error) bool {
	if err == nil {
		return false
	}
	pending := []error{err}
	nodes := 0
	leaves := 0
	for len(pending) > 0 {
		node := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		nodes++
		if nodes > 32 || node == nil {
			return false
		}
		value := reflect.ValueOf(node)
		switch value.Kind() {
		case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
			if value.IsNil() {
				return false
			}
		}
		if many, ok := node.(interface{ Unwrap() []error }); ok {
			children := many.Unwrap()
			if len(children) == 0 {
				return false
			}
			pending = append(pending, children...)
			continue
		}
		if one, ok := node.(interface{ Unwrap() error }); ok {
			child := one.Unwrap()
			if child == nil {
				return false
			}
			pending = append(pending, child)
			continue
		}
		if !os.IsNotExist(node) && node != os.ErrNotExist {
			return false
		}
		leaves++
	}
	return leaves > 0
}

func removeInstallFile(path string) error {
	err := os.Remove(path)
	if installNotExistOnly(err) {
		return nil
	}
	return err
}

// FailureMessage returns the curated installation failure message. It never
// contains paths, process output or identities, so callers may present it.
func FailureMessage(err error) (string, bool) {
	var failure *installFailure
	if !errors.As(err, &failure) || failure.message == "" {
		return "", false
	}
	// Cleanup wrappers keep the original failure as their first cause; report
	// the innermost curated message, which names the step that failed.
	for len(failure.causes) > 0 {
		var inner *installFailure
		if !errors.As(failure.causes[0], &inner) || inner.message == "" {
			break
		}
		failure = inner
	}
	return failure.message, true
}

// MaxFailureDetail bounds FailureDetail.
const MaxFailureDetail = 1024

// FailureDetail names every failed step of an error tree and its cause as one line, for a status file or log the
// user reads after the command is gone. Only text written in this program or by the operating system is kept:
// curated step messages, a file error's operation and native error, and the well-known causes below. Any other
// error may hold private text (a path, a command's payload), so it is never formatted: it is stood for by what
// it wraps, or by its type.
// knownFailureCauses are fixed public messages, safe to show as they are.
var knownFailureCauses = []error{fs.ErrPermission, fs.ErrNotExist, fs.ErrExist, context.DeadlineExceeded, context.Canceled, ErrInvalidRequest, ErrNotPrivileged}

func FailureDetail(err error) (detail string) {
	// A broken Error or Unwrap must not interrupt the cleanup that reports it.
	defer func() {
		if recover() != nil {
			detail = ""
		}
	}()
	budget := 64
	detail = strings.Join(strings.Fields(failureDetail(err, 0, &budget)), " ")
	if len(detail) > MaxFailureDetail {
		detail = strings.ToValidUTF8(detail[:MaxFailureDetail], "")
	}
	return detail
}

func failureDetail(err error, depth int, budget *int) string {
	if err == nil || depth > 32 || *budget <= 0 {
		return ""
	}
	*budget--
	var children []error
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		children = wrapped.Unwrap()
	case interface{ Unwrap() error }:
		children = []error{wrapped.Unwrap()}
	}
	var parts []string
	for _, child := range children {
		if part := failureDetail(child, depth+1, budget); part != "" && !slices.Contains(parts, part) {
			parts = append(parts, part)
		}
	}
	below := strings.Join(parts, "; ")
	prefix := ""
	switch typed := err.(type) {
	case *installFailure:
		prefix = typed.message
	case *fs.PathError:
		prefix = typed.Op
	case *os.LinkError:
		prefix = typed.Op
	case *os.SyscallError:
		prefix = typed.Syscall
	case interface{ Unwrap() []error }:
		return below
	case syscall.Errno:
		return typed.Error()
	default:
		if slices.Contains(knownFailureCauses, err) {
			return err.Error()
		}
		if below == "" {
			return reflect.TypeOf(err).String()
		}
		return below
	}
	if below == "" {
		return prefix
	}
	return prefix + ": " + below
}

// withCleanup attaches cleanup failures to a primary result without
// relabeling it. A successful cleanup returns the primary error unchanged;
// when both fail, the primary stays first so its message is presented.
func withCleanup(message string, primary error, cleanups ...error) error {
	cleanup := hostInstallFailure(message, cleanups...)
	if cleanup == nil {
		return primary
	}
	if primary == nil {
		return cleanup
	}
	return errors.Join(primary, cleanup)
}

// SuppliedExecutableError names why a binary offered to `pb install` is
// refused and what to do about it. It is an invalid-request failure.
type SuppliedExecutableError struct{ Path, Reason, Fix string }

func (e *SuppliedExecutableError) Error() string {
	return "the executable " + e.Path + " " + e.Reason + "; " + e.Fix + ", then run `pb install` again"
}
func (e *SuppliedExecutableError) Is(target error) bool { return target == ErrInvalidRequest }
