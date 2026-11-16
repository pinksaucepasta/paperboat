//go:build darwin || linux || windows

package runtime

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/controlbackoff"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/envinject"
	runtimeidentity "github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestControlWatchMarksNetworkFailuresAndAnAnswerClearsThem(t *testing.T) {
	var fail error = &net.OpError{Op: "dial", Err: errors.New("i/o timeout")}
	watch := &controlWatch{next: roundTripFunc(func(*http.Request) (*http.Response, error) {
		if fail != nil {
			return nil, fail
		}
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: http.NoBody}, nil
	})}
	request, _ := http.NewRequest(http.MethodGet, "https://control.example.test/", nil)
	fail = context.Canceled
	_, _ = watch.RoundTrip(request)
	if watch.ControlDown() {
		t.Fatal("a caller's own cancel marked the control plane down")
	}
	for _, failure := range []error{&net.OpError{Op: "dial", Err: errors.New("i/o timeout")}, errors.New("net/http: request canceled")} { // the second is what the client's own 15 s timeout leaves
		fail = context.Canceled
		_, _ = watch.RoundTrip(request)
		watch.failedAt = time.Time{}
		fail = failure
		_, _ = watch.RoundTrip(request)
		if !watch.ControlDown() {
			t.Fatalf("%v did not mark the control plane down", failure)
		}
	}
	watch.failedAt = time.Time{}
	fail = controlbackoff.ErrIdentityRejected
	_, _ = watch.RoundTrip(request)
	if watch.ControlDown() {
		t.Fatal("a refused identity (an answer from the control plane) marked it down")
	}
	fail = &net.OpError{Op: "dial", Err: errors.New("i/o timeout")}
	_, _ = watch.RoundTrip(request)
	fail = nil
	if _, err := watch.RoundTrip(request); err != nil || watch.ControlDown() {
		t.Fatal("an HTTP answer did not clear the mark", err)
	}
}

func TestLayerEnvironmentDoesNotWaitOnAControlPlaneKnownDown(t *testing.T) {
	calls := 0
	watch := &controlWatch{next: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, &net.OpError{Op: "dial", Err: errors.New("i/o timeout")}
	})}
	base, _ := url.Parse("https://control.example.test")
	service := newLayerEnvironmentService(t.TempDir(), base, watch, layerRegistration(t), nil, nil)
	request, _ := http.NewRequest(http.MethodGet, base.String(), nil)
	_, _ = watch.RoundTrip(request) // the host's own control traffic failed
	calls = 0
	ctx := envinject.WithLaunchContext(context.Background(), "personal", "account")
	_, err := service.EnvironmentForLaunch(ctx)
	if err == nil || calls != 0 || envinject.LaunchNoticeFor(err) != envinject.LaunchNoticeOffline {
		t.Fatalf("ENV lookup waited on a known outage: calls=%d err=%v notice=%q", calls, err, envinject.LaunchNoticeFor(err))
	}
}

// The host's own boot-time key registration holds the lock for its whole 15 s request when Paperboat is unreachable; a
// launch's ENV lookup queued behind it for all of that (15 s measured on the first exec after a restart in an outage).
func TestLayerRegistrationWaitIsBoundedByTheCallersContext(t *testing.T) {
	base, _ := url.Parse("https://control.example.test")
	service := newLayerEnvironmentService(t.TempDir(), base, nil, layerRegistration(t), nil, nil)
	service.refresh <- struct{}{} // the boot-time registration is in flight
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err := service.registerLayerRecipient(ctx); !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 2*time.Second {
		t.Fatalf("err=%v after %v; want the caller's deadline", err, time.Since(started))
	}
}

func layerRegistration(t *testing.T) runtimeidentity.Registration {
	t.Helper()
	return runtimeidentity.Registration{MachineID: "machine_1", InstallationGeneration: 11}
}

// Until the first control answer after boot the host cannot know Paperboat is down, so a launch's ENV lookup is short;
// once anything answered, it gets the full budget.
func TestEnvironmentLookupIsShortUntilTheControlPlaneFirstAnswers(t *testing.T) {
	watch := &controlWatch{next: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	})}
	if got := environmentBudget(watch); got != unprovenLookupBudget {
		t.Fatalf("budget before any answer = %v", got)
	}
	request, _ := http.NewRequest(http.MethodGet, "https://control.example.test/", nil)
	if _, err := watch.RoundTrip(request); err != nil {
		t.Fatal(err)
	}
	if got := environmentBudget(watch); got != environmentLookupBudget {
		t.Fatalf("budget after an answer = %v", got)
	}
}
