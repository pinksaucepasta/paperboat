package errorreport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"syscall"
	"testing"
)

func TestHTTPAttemptOwnershipDoesNotSuppressIndependentFailure(t *testing.T) {
	cause := &net.OpError{Op: "PRIVATE", Err: syscall.ECONNREFUSED}
	transport := Transport(referenceRoundTripper(func(*http.Request) (*http.Response, error) { return nil, cause }), "https://control.invalid")
	request, _ := http.NewRequest(http.MethodGet, "https://control.invalid/v1/status", nil)
	_, err := transport.RoundTrip(request)
	if !errors.Is(err, cause) || !HTTPAttemptObserved(&url.Error{Op: "PRIVATE", URL: "PRIVATE", Err: err}) {
		t.Fatal("recorded attempt lost its marker or original cause")
	}
	var network *net.OpError
	if !errors.As(err, &network) || network != cause {
		t.Fatal("network cause type was lost")
	}
	timedOut := recordedHTTPFailure(context.DeadlineExceeded)
	var timeoutError net.Error
	if !errors.As(timedOut, &timeoutError) || !timeoutError.Timeout() || !(&url.Error{Err: timedOut}).Timeout() {
		t.Fatal("HTTP wrapper lost net.Error timeout semantics")
	}
	if errors.As(recordedHTTPFailure(errors.New("private validation error")), &timeoutError) {
		t.Fatal("non-network failure was mislabeled as a network error")
	}
	for _, unowned := range []error{cause, errors.Join(err, syscall.ENOSPC), &cyclicFault{}, (*cyclicFault)(nil)} {
		if HTTPAttemptObserved(unowned) {
			t.Fatal("independent/unresolved failure was suppressed")
		}
	}
	for range 20 {
		err = fmt.Errorf("PRIVATE: %w", err)
	}
	if HTTPAttemptObserved(err) {
		t.Fatal("unresolved wrapper chain was suppressed")
	}
}

func TestHTTPStatusOwnershipPreservesBodyAndRawTransportFailures(t *testing.T) {
	response := &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header)}
	transport := Transport(referenceRoundTripper(func(*http.Request) (*http.Response, error) { return response, nil }), "https://control.invalid")
	request, _ := http.NewRequest(http.MethodGet, "https://control.invalid/v1/status", nil)
	result, err := transport.RoundTrip(request)
	if err != nil || result.Body != response.Body || result.StatusCode != response.StatusCode || response.Request != nil {
		t.Fatal("response contract changed")
	}
	failure := HTTPStatusFailure(result)
	if !HTTPAttemptObserved(failure) || ProjectFault(t.Context(), "pb", "status", "control_request", "control_request_failed", failure).HTTPStatus != 503 {
		t.Fatal("recorded response status lost its owner")
	}
	if HTTPAttemptObserved(HTTPStatusFailure(response)) || HTTPAttemptObserved(errors.Join(failure, syscall.ENOSPC)) {
		t.Fatal("raw response or independent local failure was suppressed")
	}
}
