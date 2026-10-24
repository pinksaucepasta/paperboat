//go:build linux || darwin

package runtime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"testing"
	"time"

	clientapi "github.com/pinksaucepasta/paperboat/internal/api"
)

// Every fresh pair on the shared account failed at "installing system services" once the server
// answered its peer network configuration with 409 peer_network_too_large: the carrier treated it as
// invalid and exited (2026-10-22). A refused peer network keeps the carrier serving and retries; an
// authorization refusal or any other conflict still fails start closed.
func TestRefusedPeerNetworkKeepsTheCarrierRunning(t *testing.T) {
	for _, test := range []struct {
		err  error
		want bool
	}{
		{&clientapi.APIError{Status: 409, Code: "peer_network_too_large"}, true},
		{fmt.Errorf("register: %w", &clientapi.APIError{Status: 409, Code: "peer_network_conflict"}), true},
		{&clientapi.APIError{Status: 503, Code: "peer_network_unavailable"}, true},
		{&clientapi.APIError{Status: 409, Code: "operation_conflict"}, false},
		{&clientapi.APIError{Status: 403, Code: "peer_network_denied"}, false},
		{errors.Join(ErrProductionInvalid, errors.New("invalid certificate")), false},
	} {
		if got := transientNativePeerStart(test.err); got != test.want {
			t.Errorf("%v: keeps running=%t, want %t", test.err, got, test.want)
		}
	}
	if message := (&clientapi.APIError{Status: 409, Code: "peer_network_too_large"}).Error(); message == (&clientapi.APIError{Status: 409}).Error() {
		t.Fatalf("too-large refusal has the generic conflict message %q; status and doctor must say what is wrong", message)
	}
}

type scriptedPeerEnrollment struct{ err error }

func (e scriptedPeerEnrollment) Ensure(context.Context) error { return e.err }

// A fresh pair on round 26 failed at "installing system services" because the carrier's first peer
// identity registration hit one DNS timeout and the carrier exited (pbvm2, 2026-10-24). A transient
// control-plane failure is left to the enrollment service that retries every 2 s, like a pending
// approval; an authorization refusal or invalid state still fails the start closed.
func TestTransientPeerEnrollmentFailureKeepsTheCarrierStarting(t *testing.T) {
	dnsTimeout := &url.Error{Op: "Post", URL: "https://control.example.test/v1/peer-identity", Err: &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "i/o timeout", Name: "control.example.test", IsTimeout: true}}}
	for _, test := range []struct {
		err   error
		start bool
	}{
		{nil, true},
		{fmt.Errorf("register peer identity: %w", dnsTimeout), true},
		{&clientapi.APIError{Status: 503, Code: "service_unavailable"}, true},
		{context.DeadlineExceeded, true},
		{&clientapi.APIError{Status: 403, Code: "machine_revoked"}, false},
		{errors.Join(ErrProductionInvalid, errors.New("invalid certificate")), false},
	} {
		err := allowPendingPeerEnrollment(t.Context(), scriptedPeerEnrollment{err: test.err})
		if (err == nil) != test.start {
			t.Errorf("Ensure error %v: start error %v, want start=%t", test.err, err, test.start)
		}
	}
}

type sequencePeerEnrollment struct {
	results []error
	calls   int
}

func (e *sequencePeerEnrollment) Ensure(context.Context) error {
	result := e.results[min(e.calls, len(e.results)-1)]
	e.calls++
	return result
}

// The background registration keeps retrying after a transient failure (it used to stop on any error but
// pending, so a carrier that started past a DNS timeout never registered its peer identity), and still
// stops on a refusal.
func TestPeerEnrollmentRetriesTransientFailuresUntilRegistered(t *testing.T) {
	timeout := &url.Error{Op: "Post", URL: "https://control.example.test/v1/peer-identity", Err: &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "i/o timeout", Name: "control.example.test", IsTimeout: true}}}
	enrollment := &sequencePeerEnrollment{results: []error{timeout, &clientapi.APIError{Status: 503, Code: "service_unavailable"}, nil}}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := waitForPeerEnrollment(ctx, enrollment, 10*time.Millisecond); err != nil || enrollment.calls != 3 {
		t.Fatalf("err=%v after %d registrations; want registered on the 3rd", err, enrollment.calls)
	}
	refused := &sequencePeerEnrollment{results: []error{timeout, &clientapi.APIError{Status: 403, Code: "machine_revoked"}}}
	if err := waitForPeerEnrollment(ctx, refused, 10*time.Millisecond); err == nil || refused.calls != 2 {
		t.Fatalf("err=%v after %d registrations; want the refusal returned on the 2nd", err, refused.calls)
	}
}
