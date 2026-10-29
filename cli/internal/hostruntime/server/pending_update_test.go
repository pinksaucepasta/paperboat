package server

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
)

// An authorized, read-only inbox long poll must not hold the update's mutation
// drain open. The actual transfer handler and HTTP association own this contract.
func TestPendingInboxDoesNotDelayUpdate(t *testing.T) {
	handler, _ := fileTransferTestHandler(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	controller, err := hostdproto.NewController(hostdproto.ControllerConfig{APIMin: 1, APIMax: 1})
	if err != nil {
		t.Fatal(err)
	}
	welcome, err := controller.Negotiate(hostdproto.Hello{WorkerID: "worker", Version: "1", APIMin: 1, APIMax: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err = controller.MarkReady(hostdproto.Ready{WorkerID: welcome.WorkerID, APIVersion: welcome.APIVersion, Epoch: welcome.Epoch, Lease: welcome.Lease}); err != nil {
		t.Fatal(err)
	}
	active, err := controller.Activate(hostdproto.Activate{WorkerID: welcome.WorkerID, APIVersion: welcome.APIVersion, Epoch: welcome.Epoch, Lease: welcome.Lease})
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	released := make(chan struct{})
	ctx = context.WithValue(ctx, nativeHTTPAdmissionKey{}, nativeHTTPAdmission(func(context.Context) (func(), <-chan struct{}, error) {
		release, err := controller.AcquireActive(active.WorkerID, active.Epoch)
		if err != nil {
			return nil, nil, err
		}
		close(entered)
		return func() { release(); close(released) }, make(chan struct{}), nil
	}))
	origin, client := net.Pipe()
	defer client.Close()
	done := make(chan error, 1)
	go func() { done <- ServeHTTPConnection(ctx, origin, handler) }()
	defer func() { cancel(); client.Close(); <-done }()
	request := transferRequest(http.MethodGet, "http://example.test/v1/file-transfers/pending?session_id=ses_1&wait_seconds=2", nil)
	if err = request.Write(client); err != nil {
		t.Fatal(err)
	}
	<-entered
	select {
	case <-released:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("idle inbox poll holds native mutation admission")
	}
	drain, stop := context.WithTimeout(ctx, 500*time.Millisecond)
	defer stop()
	if _, err = controller.PrepareMaintenance(drain, false, nil); err != nil {
		t.Fatalf("update waits for idle inbox: %v", err)
	}
	response, err := http.ReadResponse(bufio.NewReader(client), request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK || string(body) != "{\"transfers\":[]}\n" {
		t.Fatalf("pending response status=%d body=%q err=%v", response.StatusCode, body, err)
	}
}
