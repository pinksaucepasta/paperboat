//go:build darwin || linux

package localapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/machineevents"
)

func TestMachineEventsPushOverOwnerSocket(t *testing.T) {
	socket := filepath.Join(localAPITestDir(t), "events.sock")
	hub := machineevents.NewPushed()
	sub := hub.Subscribe(machineevents.TopicInventory)
	server, err := NewServer(ServerConfig{SocketPath: socket, OwnerUID: os.Geteuid(), OwnerGID: os.Getegid(), Source: snapshotSourceFunc(func(context.Context) (Snapshot, error) { return validSnapshot(), nil }), MachineEvents: func(_ context.Context, push MachineEventPush) error { hub.Push(push.Topics, push.Healthy); return nil }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		case <-time.After(time.Second):
			t.Error("local API did not stop")
		}
	})
	waitForSocket(t, socket)
	client, err := NewClient(socket, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.PushMachineEvents(ctx, MachineEventPush{MachineID: "machine_test", Topics: []machineevents.Topic{machineevents.TopicInventory}, Healthy: true}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sub.Changed():
	case <-time.After(time.Second):
		t.Fatal("inventory consumer missed socket push")
	}
	if !sub.Healthy() {
		t.Fatal("stream health lost over socket")
	}
	// Notification writes need ownership even if read access was granted.
	boundary := *server
	boundary.config.Authorize = func(Peer) bool { return true }
	for _, test := range []struct {
		name, body string
		owner      bool
		status     int
	}{
		{"nonowner", `{"machine_id":"machine_test","topics":["inventory"]}`, false, http.StatusForbidden},
		{"unknown topic", `{"machine_id":"machine_test","topics":["credentials"]}`, true, http.StatusBadRequest},
		{"unknown field", `{"machine_id":"machine_test","topics":[],"secret":"refuse"}`, true, http.StatusBadRequest},
		{"oversized", strings.Repeat(" ", 4097), true, http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			peer := Peer{UID: os.Geteuid() + 1}
			if test.owner {
				peer.UID = os.Geteuid()
			}
			request := httptest.NewRequest(http.MethodPost, "/v1/machine-events", strings.NewReader(test.body))
			request = request.WithContext(context.WithValue(request.Context(), peerContextKey{}, peer))
			response := httptest.NewRecorder()
			boundary.handler().ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("got %d, want %d", response.Code, test.status)
			}
			select {
			case <-sub.Changed():
				t.Fatal("refused request delivered invalidation")
			default:
			}
		})
	}
}
