package workloadbridge

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/history"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/session"
)

// idleOutputService waits for output on a context derived from the request's, as the session manager does, and
// has output only after outputAt.
type idleOutputService struct {
	stalledDetachService
	outputAt time.Time
}

func (s *idleOutputService) WaitNext(ctx context.Context, _, _ string, from uint64) (history.Event, error) {
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	select {
	case <-time.After(time.Until(s.outputAt)):
		return history.Event{StartSequence: from, EndSequence: from + 1, Data: []byte("x")}, nil
	case <-waitCtx.Done():
		return history.Event{}, waitCtx.Err()
	}
}

func bridgeHop(t *testing.T, sessions session.Service) *Client {
	t.Helper()
	token := make([]byte, 32)
	server := &Server{Token: token, Sessions: sessions, MaximumConcurrent: 4,
		Fence: func() hostdproto.Status {
			return hostdproto.Status{State: hostdproto.StateActive, WorkerID: "worker", Epoch: 1}
		},
		AcquireActive: func(string, uint64) (func(), error) { return func() {}, nil },
	}
	listener := httptest.NewServer(server.handler())
	t.Cleanup(listener.Close)
	client, err := NewClient(listener.Listener.Addr().String(), token, "worker", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Listener.Addr().String()
	client.transport = &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}}
	client.http = &http.Client{Transport: client.transport}
	t.Cleanup(client.Close)
	return client
}

// An idle web terminal dropped ~25 s after each attach ("Reconnecting: The connection was interrupted"): the output
// wait's server deadline reached the publisher as an owner failure instead of a deadline to poll again. An idle
// wait must outlive several server deadlines, through one bridge and through hostd's bridge to the session owner,
// and then deliver the output.
func TestIdleOutputWaitOutlivesTheRequestDeadline(t *testing.T) {
	oldServer, oldClient := serverRequestTimeout, clientRequestTimeout
	// The client deadline is far past the server's, as in production, so a loaded runner cannot invert them.
	serverRequestTimeout, clientRequestTimeout = 150*time.Millisecond, 600*time.Millisecond
	t.Cleanup(func() { serverRequestTimeout, clientRequestTimeout = oldServer, oldClient })
	for _, hops := range []int{1, 2} {
		owner := &idleOutputService{outputAt: time.Now().Add(700 * time.Millisecond)}
		client := bridgeHop(t, owner)
		if hops == 2 {
			forward, err := client.Sessions()
			if err != nil {
				t.Fatal(err)
			}
			client = bridgeHop(t, forward)
		}
		remote, err := client.Sessions()
		if err != nil {
			t.Fatal(err)
		}
		event, err := remote.WaitNext(context.Background(), "s", "a", 4)
		if err != nil || event.EndSequence != 5 {
			t.Fatalf("%d hop(s): idle WaitNext = %+v, %v; want the output after the server deadline", hops, event, err)
		}
	}
}
