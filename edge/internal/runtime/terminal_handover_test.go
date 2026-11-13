package runtime

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"

	"github.com/pinksaucepasta/paperboat/edge/internal/control"
	"github.com/pinksaucepasta/paperboat/edge/internal/datacarrier"
	"github.com/pinksaucepasta/paperboat/edge/internal/edgehttp"
)

// pb update: a browser terminal's upstream stream on the old worker's runtime carrier crosses two
// consecutive handovers with no close and no gap: the old runtime admission is held for the same machine's newer
// attachment, and the replaced route stays the standby its open streams run on. A new request lands
// on the new worker's carrier. The held stream ends when the old carrier closes (its drain ended or
// reached DrainLimit), or at once when the route is withdrawn (revocation).
func TestHeldRuntimeStreamCrossesAWorkerHandover(t *testing.T) {
	for _, end := range []string{"old carrier closes", "revocation"} {
		t.Run(end, func(t *testing.T) {
			oldRuntime := runtimeAdmission(t)
			newRuntime := oldRuntime
			newRuntime.Binding.SessionID, newRuntime.Binding.ProcessGeneration, newRuntime.AttachmentGeneration = "session_2", 2, 2
			epoch, node := oldRuntime.Binding.EdgeProcessEpoch, oldRuntime.Binding.EdgeNodeID
			source := &runtimeSourceFake{admissions: []runtimewire.Admission{oldRuntime}}
			expected, err := datacarrier.NewExpectedAdmissionRegistry(datacarrier.ExpectedAdmissionRegistryConfig{NodeID: node, ProcessEpoch: epoch})
			if err != nil {
				t.Fatal(err)
			}
			routes, err := edgehttp.NewDataCarrierPreviewRegistry(edgehttp.DataCarrierPreviewRegistryConfig{BaseDomain: "runtime.example.test", ProcessEpoch: epoch})
			if err != nil {
				t.Fatal(err)
			}
			hub := edgehttp.NewBrowserTerminalHub()
			w := &RuntimeCarrierWorker{Source: source, Expected: expected, Routes: routes, Endpoints: endpointRegistry(t, epoch), BrowserTerminalHub: hub, Interval: 20 * time.Millisecond, Timeout: time.Second}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if err := w.Start(ctx); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = w.Shutdown(context.Background()) }()
			identity := func(a runtimewire.Admission) datacarrier.Identity {
				ea, err := control.ExpectedRuntimeAdmission(a, node, epoch, time.Now())
				if err != nil {
					t.Fatal(err)
				}
				return ea.Identity
			}
			waitRuntime(t, func() bool { return len(expected.Snapshot()) == 1 })
			oldServer, oldClient := streamingCarrier(t, identity(oldRuntime), expected, "old")
			go func() { _ = w.Handle(ctx, oldServer) }()
			host := oldRuntime.Hostname
			waitRuntime(t, func() bool { r, ok := routes.Lookup(host); return ok && r.Server == oldServer })
			transport, err := edgehttp.NewDataCarrierPreviewTransport(edgehttp.DataCarrierPreviewTransportConfig{Registry: routes, StreamOpenTimeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			get := func(path string) *http.Response {
				t.Helper()
				request := httptest.NewRequest(http.MethodGet, "https://"+host+path, nil)
				request.Host = host
				response, err := transport.RoundTrip(request)
				if err != nil {
					t.Fatal(err)
				}
				return response
			}
			held := get("/stream")
			defer held.Body.Close()
			lines := make(chan string, 1024)
			ended := make(chan error, 1)
			go func() {
				scanner := bufio.NewScanner(held.Body)
				for scanner.Scan() {
					lines <- scanner.Text()
				}
				ended <- fmt.Errorf("held stream ended: %v", scanner.Err())
			}()
			next := 1
			expect := func(d time.Duration) {
				t.Helper()
				deadline := time.After(d)
				for {
					select {
					case line := <-lines:
						if line != "old "+strconv.Itoa(next) {
							t.Fatalf("held stream gap: got %q, want old %d", line, next)
						}
						next++
					case err := <-ended:
						t.Fatal(err)
					case <-deadline:
						return
					}
				}
			}
			expect(100 * time.Millisecond)

			// The handover: goaway, the server names the new worker's carrier, the new carrier attaches.
			sendHostGoaway(t, oldClient, identity(oldRuntime), oldRuntime.Binding.RouteID)
			source.mu.Lock()
			source.admissions = []runtimewire.Admission{newRuntime}
			source.mu.Unlock()
			newServer, _ := streamingCarrier(t, identity(newRuntime), expected, "new")
			go func() { _ = w.Handle(ctx, newServer) }()
			waitRuntime(t, func() bool { r, ok := routes.Lookup(host); return ok && r.Server == newServer })
			expect(300 * time.Millisecond) // several pulls and carrier rechecks
			fresh := get("/name")
			body, err := io.ReadAll(fresh.Body)
			_ = fresh.Body.Close()
			if err != nil || string(body) != "new" {
				t.Fatalf("a new request landed on %q (%v), want the new worker", body, err)
			}
			before := next
			expect(100 * time.Millisecond)
			if next == before {
				t.Fatal("the held stream stalled after the handover")
			}

			// A second update while the first worker still holds the stream: the third worker takes the route,
			// the second one (which holds nothing) leaves, and the first one's stream goes on.
			thirdRuntime := newRuntime
			thirdRuntime.Binding.SessionID, thirdRuntime.Binding.ProcessGeneration, thirdRuntime.AttachmentGeneration = "session_3", 3, 3
			source.mu.Lock()
			source.admissions = []runtimewire.Admission{thirdRuntime}
			source.mu.Unlock()
			thirdServer, _ := streamingCarrier(t, identity(thirdRuntime), expected, "third")
			go func() { _ = w.Handle(ctx, thirdServer) }()
			waitRuntime(t, func() bool { r, ok := routes.Lookup(host); return ok && r.Server == thirdServer })
			_ = newServer.Close()
			expect(300 * time.Millisecond)
			before = next
			expect(100 * time.Millisecond)
			if next == before {
				t.Fatal("the held stream stalled after the second handover")
			}

			if end == "revocation" {
				source.mu.Lock()
				source.admissions = nil
				source.mu.Unlock()
			} else {
				_ = oldClient.Close()
			}
			select {
			case <-ended:
			case <-time.After(2 * time.Second):
				t.Fatalf("%s: the held stream did not end", end)
			}
		})
	}
}

// streamingCarrier is one worker's runtime carrier whose host answers /stream with "name n" lines
// every 10 ms until the request ends, and anything else with name.
func streamingCarrier(t *testing.T, identity datacarrier.Identity, expected *datacarrier.ExpectedAdmissionRegistry, name string) (*datacarrier.Server, *datacarrier.Client) {
	t.Helper()
	local, remote := net.Pipe()
	config := datacarrier.DefaultConfig()
	config.Identity = identity
	config.Authorize = expected
	server, err := datacarrier.NewServer(context.Background(), remote, config)
	if err != nil {
		t.Fatal(err)
	}
	client, err := datacarrier.NewClient(context.Background(), local, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/stream" {
			_, _ = io.WriteString(w, name)
			return
		}
		flusher, _ := w.(http.Flusher)
		for n := 1; ; n++ {
			if _, err := fmt.Fprintf(w, "%s %d\n", name, n); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	})
	go func() {
		for {
			stream, _, err := client.AcceptStream(context.Background())
			if err != nil {
				return
			}
			go func() { _ = datacarrier.ServeHTTPStream(context.Background(), stream, handler, 4096, time.Minute) }()
		}
	}()
	return server, client
}
