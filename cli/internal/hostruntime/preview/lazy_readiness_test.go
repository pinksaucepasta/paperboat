package preview

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLazyHTTPReadinessRequiresUsableProtocolResponse(t *testing.T) {
	for name, fixture := range map[string]struct {
		status int
		ready  bool
	}{
		"ready":       {http.StatusNotFound, true},
		"unavailable": {http.StatusServiceUnavailable, false},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodHead || r.URL.Path != "/" {
					t.Error("unexpected readiness request")
				}
				w.WriteHeader(fixture.status)
			}))
			defer server.Close()
			carrier := &DataCarrierPreviewCarrier{routeID: "preview_lazy"}
			err := carrier.probeLazyHTTP(context.Background(), Lease{Target: LeaseTarget{Scheme: "http", Address: server.Listener.Addr().String()}})
			if (err == nil) != fixture.ready {
				t.Fatalf("want ready=%v err=%v", fixture.ready, err)
			}
		})
	}
}

func TestLazyCarrierReadinessFailureIsNotRetryable(t *testing.T) {
	var probes int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { probes++; w.WriteHeader(http.StatusServiceUnavailable) }))
	defer server.Close()
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
	carrier, err := NewDataCarrierPreviewCarrier(DataCarrierPreviewCarrierConfig{Hub: hub, Identity: identity})
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := newLazyLifecycle(ctx, func() time.Time { return time.Now().UTC() }, time.Hour, time.Hour, time.Now().Add(time.Hour))
	defer lifecycle.Stop()
	err = carrier.Run(ctx, Lease{ID: "preview_lazy", Target: LeaseTarget{Scheme: "http", Address: server.Listener.Addr().String()}, LazyLifecycle: lifecycle}, func(Lease) error { t.Fatal("unavailable origin reported ready"); return nil })
	var retryable *RetryableCarrierError
	if err == nil || errors.As(err, &retryable) || probes != 1 {
		t.Fatalf("err=%v probes=%d", err, probes)
	}
}

func TestLazyHTTPReadinessRejectsListeningNonHTTPPort(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr == nil {
			defer connection.Close()
			_, _ = connection.Write([]byte("not-http\n"))
		}
	}()
	carrier := &DataCarrierPreviewCarrier{routeID: "preview_lazy"}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := carrier.probeLazyHTTP(ctx, Lease{Target: LeaseTarget{Scheme: "http", Address: listener.Addr().String()}}); err == nil {
		t.Fatal("non-HTTP listener reported ready")
	}
}
