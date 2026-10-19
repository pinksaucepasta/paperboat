//go:build darwin || linux || windows

package runtime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/auth"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machineevents"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machineevents/eventstest"
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
