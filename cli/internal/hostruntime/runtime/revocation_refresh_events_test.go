//go:build darwin || linux || windows

package runtime

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/auth"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/machineevents"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/machineevents/eventstest"
)

type revocationTestAuth struct{}

func (revocationTestAuth) Token(context.Context) (string, error) { return "token", nil }
func (revocationTestAuth) Proof(context.Context, string, string, string, []byte) ([]byte, error) {
	return []byte("proof"), nil
}

// A revoked access credential is learned when the server says access changed, not at the next poll.
func TestRevocationRefreshRunsWhenAccessChanges(t *testing.T) {
	var refreshes atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshes.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jtis":[]}`))
	}))
	defer server.Close()
	_, subscriptions, plane := eventstest.Start(t, time.Hour, machineevents.TopicMachineAccess)
	var operations atomic.Int32
	service, err := newRevocationRefreshService(server.URL+"/v1/helper-trust/revocations", revocationTestAuth{}, revocationTestAuth{}, func() (string, error) {
		operations.Add(1)
		return "operation_" + time.Now().Format("150405.000000000"), nil
	}, auth.NewRevocationCache(), server.Client().Transport, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	service.events = subscriptions[0]
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer service.Shutdown(context.Background())
	waitForRefreshes(t, &refreshes, 1)
	plane.Publish(machineevents.TopicMachineAccess)
	waitForRefreshes(t, &refreshes, 2)
}

func waitForRefreshes(t *testing.T, count *atomic.Int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for count.Load() < want {
		if time.Now().After(deadline) {
			t.Fatalf("refreshes = %d, want %d", count.Load(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type revocationRoundTripper func(*http.Request) (*http.Response, error)

func (f revocationRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A healthy hub: the 15 s poll base does not poll; the 5-minute safety refresh and a push do.
func TestRevocationRefreshFiveMinuteSafety(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		changes := make(chan map[machineevents.Topic]int64)
		hub, err := machineevents.NewWithWaiter(func(ctx context.Context, _ map[machineevents.Topic]int64) (map[machineevents.Topic]int64, error) {
			select {
			case moved := <-changes:
				return moved, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}, 0)
		if err != nil {
			t.Fatal(err)
		}
		events := hub.Subscribe(machineevents.TopicMachineAccess)
		if err := hub.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		defer hub.Shutdown(context.Background())
		changes <- nil
		synctest.Wait()
		var fetches atomic.Int32
		service, err := newRevocationRefreshService("https://control.example.test/v1/helper-trust/revocations", revocationTestAuth{}, revocationTestAuth{}, func() (string, error) { return "operation_test", nil }, auth.NewRevocationCache(), revocationRoundTripper(func(r *http.Request) (*http.Response, error) {
			fetches.Add(1)
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"jtis":[]}`)), Header: make(http.Header), Request: r}, nil
		}), 15*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		service.events = events
		if err := service.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		defer service.Shutdown(context.Background())
		synctest.Wait()
		time.Sleep(5*time.Minute - time.Second)
		synctest.Wait()
		if fetches.Load() != 1 {
			t.Fatalf("early refresh: %d", fetches.Load())
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if fetches.Load() != 2 {
			t.Fatalf("five minute refreshes: %d", fetches.Load())
		}
		changes <- map[machineevents.Topic]int64{machineevents.TopicMachineAccess: 1}
		synctest.Wait()
		if fetches.Load() != 3 {
			t.Fatalf("push refreshes: %d", fetches.Load())
		}
	})
}
