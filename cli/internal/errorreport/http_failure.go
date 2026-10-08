package errorreport

import (
	"net"
	"net/http"
	"reflect"
)

type observedHTTPFailure struct {
	cause    error
	observed bool
}

func (observedHTTPFailure) Error() string                     { return "control request failed" }
func (failure observedHTTPFailure) Unwrap() error             { return failure.cause }
func (failure observedHTTPFailure) httpAttemptObserved() bool { return failure.observed }

type observedHTTPNetworkFailure struct {
	observedHTTPFailure
	network net.Error
}

func (failure observedHTTPNetworkFailure) Timeout() bool   { return failure.network.Timeout() }
func (failure observedHTTPNetworkFailure) Temporary() bool { return failure.network.Temporary() }

func recordedHTTPFailure(err error) error {
	failure := observedHTTPFailure{cause: err, observed: true}
	if network, ok := err.(net.Error); ok {
		return observedHTTPNetworkFailure{observedHTTPFailure: failure, network: network}
	}
	return failure
}

// HTTPAttemptObserved reports whether this entire failure was already recorded
// by the owned transport. A join with another local failure cannot prove that
// ownership; neither can a cyclic or unresolved wrapper chain.
func HTTPAttemptObserved(err error) bool {
	seen := make(map[error]bool)
	for range 16 {
		if err == nil {
			return false
		}
		value := reflect.ValueOf(err)
		if value.Kind() == reflect.Pointer && value.IsNil() {
			return false
		}
		if value.Type().Comparable() {
			if seen[err] {
				return false
			}
			seen[err] = true
		}
		if failure, ok := err.(interface{ httpAttemptObserved() bool }); ok {
			return failure.httpAttemptObserved()
		}
		if _, joined := err.(interface{ Unwrap() []error }); joined {
			return false
		}
		wrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = wrapper.Unwrap()
	}
	return false
}

// HTTPStatusFailure preserves an unsuccessful response's status without its
// body. Only an attempt recorded by the owned transport carries the marker.
func HTTPStatusFailure(response *http.Response) error {
	if response == nil {
		return responseFault(http.StatusServiceUnavailable)
	}
	observed := false
	if response.Request != nil {
		if attempt, _ := response.Request.Context().Value(attemptFaultKey{}).(*attemptFault); attempt != nil {
			observed = attempt.observed.Load()
		}
	}
	return observedHTTPFailure{cause: responseFault(response.StatusCode), observed: observed}
}
