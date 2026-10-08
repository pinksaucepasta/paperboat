package tunnelenrollment

import (
	"errors"
	"io"
	"net/http"
	"reflect"
	"sync"
)

// enrollmentFailure keeps the typed cause chain available to callers and
// diagnostics while keeping paths, response text, and transport prose out of
// user-facing errors. Messages passed to safeEnrollmentFailure must be
// package-owned constants.
type enrollmentFailure struct {
	message string
	causes  []error
}

func (failure enrollmentFailure) Error() string { return failure.message }

func (failure enrollmentFailure) Unwrap() []error {
	causes := make([]error, len(failure.causes))
	copy(causes, failure.causes)
	return causes
}

func safeEnrollmentFailure(message string, causes ...error) error {
	filtered := make([]error, 0, len(causes))
	for _, cause := range causes {
		if cause != nil {
			filtered = append(filtered, cause)
		}
	}
	return enrollmentFailure{message: message, causes: filtered}
}

type enrollmentClassFailure struct {
	message        string
	classification error
	cause          error
}

func (failure enrollmentClassFailure) Error() string { return failure.message }
func (failure enrollmentClassFailure) Unwrap() error { return failure.cause }
func (failure enrollmentClassFailure) Is(target error) bool {
	return target == failure.classification || errors.Is(failure.cause, target)
}

func safeEnrollmentClassFailure(message string, classification, cause error) error {
	return enrollmentClassFailure{message: message, classification: classification, cause: cause}
}

// enrollmentOutcomeOnly permits an HTTP rejection mapping only when every
// bounded terminal cause proves the same expected outcome. A joined storage or
// network failure must not inherit a 401, 403, or 409 from one sibling.
func enrollmentOutcomeOnly(err, outcome error) bool {
	if err == nil || outcome == nil {
		return false
	}
	pending := []error{err}
	sawOutcome := false
	for visited := 0; len(pending) > 0; visited++ {
		if visited >= 16 {
			return false
		}
		current := pending[0]
		pending = pending[1:]
		if current == nil {
			return false
		}
		value := reflect.ValueOf(current)
		switch value.Kind() {
		case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
			if value.IsNil() {
				return false
			}
		}
		if classified, ok := current.(enrollmentClassFailure); ok && classified.classification != outcome {
			return false
		}
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			children := wrapped.Unwrap()
			if len(children) == 0 || len(pending)+len(children) > 16-visited {
				return false
			}
			pending = append(pending, children...)
		case interface{ Unwrap() error }:
			child := wrapped.Unwrap()
			if child != nil {
				pending = append(pending, child)
				continue
			}
			if !errors.Is(current, outcome) {
				return false
			}
			sawOutcome = true
		default:
			if !errors.Is(current, outcome) {
				return false
			}
			sawOutcome = true
		}
	}
	return sawOutcome
}

var errMissingResponseBody = errors.New("tunnel enrollment response body is missing")

// readAndCloseResponseBody bounds response memory and owns Body.Close so read
// and cleanup failures remain available to the final error owner.
func readAndCloseResponseBody(body io.ReadCloser, limit int64) ([]byte, bool, error) {
	if body == nil {
		return nil, false, errMissingResponseBody
	}
	raw, readErr := io.ReadAll(io.LimitReader(body, limit+1))
	closeErr := body.Close()
	return raw, int64(len(raw)) > limit, errors.Join(readErr, closeErr)
}

type responseBodyCloseCapture struct {
	mu     sync.Mutex
	closed bool
	err    error
}

func (capture *responseBodyCloseCapture) record(err error) {
	capture.mu.Lock()
	capture.closed = true
	if err != nil {
		capture.err = errors.Join(capture.err, err)
	}
	capture.mu.Unlock()
}

func (capture *responseBodyCloseCapture) result() (bool, error) {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	return capture.closed, capture.err
}

type closeCapturingResponseBody struct {
	io.ReadCloser
	capture *responseBodyCloseCapture
	once    sync.Once
	err     error
}

func (body *closeCapturingResponseBody) Close() error {
	body.once.Do(func() {
		body.err = body.ReadCloser.Close()
		body.capture.record(body.err)
	})
	return body.err
}

// closeCapturingReadWriteBody preserves the upgraded HTTP response body's
// writer capability used by WebSocket clients after a 101 response.
type closeCapturingReadWriteBody struct {
	*closeCapturingResponseBody
}

func (body *closeCapturingReadWriteBody) Write(value []byte) (int, error) {
	return body.ReadCloser.(io.Writer).Write(value)
}

type closeCapturingTransport struct {
	base    http.RoundTripper
	capture *responseBodyCloseCapture
}

func (transport closeCapturingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := transport.base.RoundTrip(request)
	if response != nil && response.Body != nil {
		wrapped := &closeCapturingResponseBody{ReadCloser: response.Body, capture: transport.capture}
		if _, ok := response.Body.(io.Writer); ok {
			response.Body = &closeCapturingReadWriteBody{closeCapturingResponseBody: wrapped}
		} else {
			response.Body = wrapped
		}
	}
	return response, err
}
