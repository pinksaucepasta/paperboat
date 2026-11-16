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
