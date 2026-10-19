package preview

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/originhealth"
)

// A preview whose app stops must say so: the carrier probes the app after the preview is ready
// and reports Down once the app has refused three probes, and Up when it returns.
func TestPreviewCarrierReportsAStoppedAndRestartedApp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	identity := testPreviewCarrierIdentity(1)
	pair := newPreviewCarrierPair(t, ctx, identity)
	defer pair.close()
	hub, err := NewDataCarrierPreviewHub(ctx, DataCarrierPreviewHubConfig{Active: pair.active, Identity: identity})
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	app, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := app.Addr().String()
	carrier, err := NewDataCarrierPreviewCarrier(DataCarrierPreviewCarrierConfig{
		Hub: hub, Identity: identity, OriginProbeInterval: 30 * time.Millisecond,
		DialOrigin: func(context.Context, LeaseTarget) (io.ReadWriteCloser, error) { return &previewTestOrigin{}, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	lease := Lease{ID: "preview_origin_health", Target: LeaseTarget{Scheme: "http", Address: address}}
	events := make(chan originhealth.Event, 16)
	unregister := previewOriginReports.register(lease.ID, func(event originhealth.Event) { events <- event })
	defer unregister()
	ready := make(chan struct{})
	go func() {
		_ = carrier.Run(ctx, lease, func(Lease) error { close(ready); return nil })
	}()
	<-ready
	expect := func(state originhealth.State) originhealth.Event {
		t.Helper()
		for {
			select {
			case event := <-events:
				if event.State == state && !event.Repeat {
					return event
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("no %v event", state)
			}
		}
	}
	expect(originhealth.Up)
	_ = app.Close()
	if event := expect(originhealth.Down); event.Reason != originhealth.ReasonRefused {
		t.Fatalf("down for reason %q, want refused", event.Reason)
	}
	again, err := net.Listen("tcp", address)
	if err != nil {
		t.Skipf("port was taken: %v", err)
	}
	defer again.Close()
	expect(originhealth.Up)
}

type originRecorder struct {
	mu    sync.Mutex
	calls []bool
	dispatchObserver
}

func (o *originRecorder) ObservePreviewOrigin(_ context.Context, _ DispatchReadiness, lease Lease, up bool) (Lease, error) {
	o.mu.Lock()
	o.calls = append(o.calls, up)
	o.mu.Unlock()
	return lease, nil
}

func (o *originRecorder) snapshot() []bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]bool(nil), o.calls...)
}

// The dispatch layer sends one report per change, and a first 'up' (the readiness already
// published when the preview became ready) is not repeated.
func TestOriginReporterSendsDownThenUpAndSkipsAnInitialUp(t *testing.T) {
	recorder := &originRecorder{}
	reporter := &originReporter{send: func(ctx context.Context, up bool) error {
		_, err := recorder.ObservePreviewOrigin(ctx, DispatchReadiness{}, Lease{}, up)
		return err
	}}
	ctx := context.Background()
	reporter.handle(ctx, originhealth.Event{State: originhealth.Up})
	reporter.handle(ctx, originhealth.Event{State: originhealth.Down, Reason: originhealth.ReasonRefused})
	reporter.handle(ctx, originhealth.Event{State: originhealth.Down, Repeat: true})
	reporter.handle(ctx, originhealth.Event{State: originhealth.Up})
	reporter.handle(ctx, originhealth.Event{State: originhealth.Up})
	got := recorder.snapshot()
	want := []bool{false, false, true}
	if len(got) != len(want) {
		t.Fatalf("reports %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("reports %v, want %v", got, want)
		}
	}
}

// The origin report is a readiness POST whose origin_state is down (and ready again later),
// signed like the creation readiness and carrying the lease's current ETag.
func TestObservePreviewOriginPostsTheOriginStateAndReturnsTheCanonicalLease(t *testing.T) {
	now := time.Date(2098, 1, 2, 3, 4, 5, 0, time.UTC)
	lease := testDispatchRequest(t, now).Lease()
	for _, tc := range []struct {
		up         bool
		wantOrigin string
		wantState  string
		wantServer string
	}{{false, "down", "origin_unavailable", "unavailable"}, {true, "ready", "ready", "ready"}} {
		proofs := &readinessProofSource{t: t, operation: "operation_origin_1", path: "/v1/previews/prv_dispatch_1/readiness"}
		transport := readinessRoundTripper(func(request *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(request.Body)
			if !strings.Contains(string(body), `"origin_state":"`+tc.wantOrigin+`"`) || !strings.Contains(string(body), `"allocation_state":"ready"`) || request.Header.Get("If-Match") != lease.ETag || request.Header.Get("Idempotency-Key") != "operation_origin_1" {
				t.Fatalf("origin report up=%v body=%s headers=%v", tc.up, body, request.Header)
			}
			reply := lease
			reply.State, reply.AllocationState, reply.EdgeState, reply.OriginState = tc.wantState, "ready", "ready", tc.wantServer
			reply.ETag, reply.Generation = "", 0
			encoded, _ := json.Marshal(map[string]any{"data": reply})
			headers := make(http.Header)
			headers.Set("ETag", formatLeaseETag(lease.ID, 2))
			return &http.Response{StatusCode: http.StatusOK, Header: headers, Body: io.NopCloser(strings.NewReader(string(encoded)))}, nil
		})
		observer, err := NewHTTPDispatchReadinessObserver(HTTPDispatchReadinessObserverConfig{ControlURL: "https://api.example.test", AllowedHosts: []string{"api.example.test"}, Identities: readinessTokenSource("machine-identity"), Proofs: proofs, Transport: transport})
		if err != nil {
			t.Fatal(err)
		}
		updated, err := observer.ObservePreviewOrigin(context.Background(), DispatchReadiness{OperationID: "operation_origin_1", IdempotencyKey: "operation_origin_1", RequestID: "request_origin_1", CorrelationID: "correlation_dispatch_1"}, lease, tc.up)
		if err != nil || updated.State != tc.wantState || updated.Generation != 2 {
			t.Fatalf("up=%v updated=%+v err=%v", tc.up, updated, err)
		}
	}
}
