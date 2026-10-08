package tunnelmanager

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/inspector"
)

func forwardInspectorRequest(t *testing.T, policy inspector.ResourcePolicy, request *http.Request, handler http.HandlerFunc) (*inspector.Store, inspector.Record) {
	t.Helper()
	origin := httptest.NewServer(handler)
	t.Cleanup(origin.Close)
	store := inspector.NewStore()
	if err := store.SetPolicy("route_regression", policy); err != nil {
		t.Fatal(err)
	}
	forwarder := OriginStreamForwarder{Transport: &OriginHTTPTransport{}, Inspector: store}
	route := inspectorTestRoute(origin.Listener.Addr().String())
	route.ID = "route_regression"
	daemon, client := net.Pipe()
	serveErr := make(chan error, 1)
	go func() { serveErr <- forwarder.serveHTTP(context.Background(), daemon, route, forwarder.Transport) }()
	writeErr := make(chan error, 1)
	go func() { writeErr <- request.Write(client) }()
	response, err := http.ReadResponse(bufio.NewReader(client), request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	_ = client.Close()
	if err := <-writeErr; err != nil && !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
	if err := <-serveErr; err != nil {
		t.Fatal(err)
	}
	page, err := store.List(context.Background(), inspectorTestCredential("route_regression"), "", 10)
	if err != nil || len(page.Records) != 1 {
		t.Fatalf("captures=%d err=%v", len(page.Records), err)
	}
	return store, page.Records[0]
}

func replayCapturedBody(t *testing.T, store *inspector.Store, record inspector.Record) ([]byte, error) {
	t.Helper()
	now := time.Now().UTC()
	var replayed []byte
	_, err := inspector.NewManager(store).Replay(context.Background(), inspector.ReplayRequest{
		PrincipalID: "user_01", ResourceID: record.ResourceID, CaptureID: record.ID,
		IdempotencyKey: "regression_replay", AuthorityReadAt: now, ExpiresAt: now.Add(time.Minute),
	}, func(_ context.Context, _ string, _ string, _ http.Header, body []byte) (int, http.Header, []byte, bool, error) {
		replayed = append([]byte(nil), body...)
		return http.StatusNoContent, nil, nil, false, nil
	})
	return replayed, err
}

func TestInspectorForwardingCaptureModesAndRawBounds(t *testing.T) {
	t.Run("metadata only", func(t *testing.T) {
		request, _ := http.NewRequest(http.MethodPost, "http://public.example.test/meta", bytes.NewReader([]byte("body-is-not-retained")))
		store, record := forwardInspectorRequest(t, inspector.ResourcePolicy{Enabled: true}, request, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			_, _ = w.Write([]byte("response-is-not-retained"))
		})
		if len(record.RequestBody) != 0 || len(record.ResponseBody) != 0 || record.RequestBodyState != inspector.StateDropped || record.ResponseBodyState != inspector.StateDropped {
			t.Fatalf("metadata capture retained bodies: %+v", record)
		}
		if store.Stats().DaemonBytes != record.EstimatedBytes {
			t.Fatal("metadata accounting drift")
		}
	})

	for _, testCase := range []struct {
		name, contentType string
		body              []byte
	}{
		{name: "binary", contentType: "application/octet-stream", body: bytes.Repeat([]byte{0, 1, 2, 3, 0xfe, 0xff}, 14<<10)},
		{name: "form", contentType: "application/x-www-form-urlencoded", body: bytes.Repeat([]byte("field=value&"), 6<<10)},
	} {
		t.Run("raw "+testCase.name+" over display bound replays exactly", func(t *testing.T) {
			request, _ := http.NewRequest(http.MethodPost, "http://public.example.test/upload", bytes.NewReader(testCase.body))
			request.Header.Set("Content-Type", testCase.contentType)
			store, record := forwardInspectorRequest(t, inspector.ResourcePolicy{Enabled: true, CaptureRaw: true}, request, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.WriteHeader(http.StatusNoContent)
			})
			if len(record.RequestBody) != 0 || record.RequestBodyState != inspector.StateDropped {
				t.Fatalf("raw-only displayed body: %+v", record)
			}
			replayed, err := replayCapturedBody(t, store, record)
			if err != nil || !bytes.Equal(replayed, testCase.body) {
				t.Fatalf("raw replay len=%d err=%v", len(replayed), err)
			}
		})
	}

	t.Run("raw overflow is ineligible", func(t *testing.T) {
		body := bytes.Repeat([]byte("x"), inspector.RawMaxBytes)
		request, _ := http.NewRequest(http.MethodPost, "http://public.example.test/overflow", bytes.NewReader(body))
		store, record := forwardInspectorRequest(t, inspector.ResourcePolicy{Enabled: true, CaptureRaw: true}, request, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusNoContent)
		})
		if record.ReplayIneligible != "raw_truncated" {
			t.Fatalf("ineligible=%q", record.ReplayIneligible)
		}
		if _, err := replayCapturedBody(t, store, record); !errors.Is(err, inspector.ErrReplayIneligible) {
			t.Fatalf("overflow replay=%v", err)
		}
	})
}

func TestInspectorForwardingRejectsIncompleteAndStreamingReplay(t *testing.T) {
	t.Run("early response", func(t *testing.T) {
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusRequestEntityTooLarge) }))
		defer origin.Close()
		store := inspector.NewStore()
		if err := store.SetPolicy("route_regression", inspector.ResourcePolicy{Enabled: true, CaptureRaw: true}); err != nil {
			t.Fatal(err)
		}
		forwarder := OriginStreamForwarder{Transport: &OriginHTTPTransport{}, Inspector: store}
		route := inspectorTestRoute(origin.Listener.Addr().String())
		route.ID = "route_regression"
		daemon, client := net.Pipe()
		serveErr := make(chan error, 1)
		go func() { serveErr <- forwarder.serveHTTP(context.Background(), daemon, route, forwarder.Transport) }()
		_, _ = io.WriteString(client, "POST /early HTTP/1.1\r\nHost: public.example.test\r\nContent-Length: 100000\r\nExpect: 100-continue\r\n\r\n")
		response, err := http.ReadResponse(bufio.NewReader(client), nil)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		_ = client.Close()
		<-serveErr
		page, err := store.List(context.Background(), inspectorTestCredential("route_regression"), "", 10)
		if err != nil || len(page.Records) != 1 {
			t.Fatalf("captures=%d err=%v", len(page.Records), err)
		}
		if page.Records[0].ReplayIneligible != "request_incomplete" {
			t.Fatalf("ineligible=%q", page.Records[0].ReplayIneligible)
		}
		if _, err := replayCapturedBody(t, store, page.Records[0]); !errors.Is(err, inspector.ErrReplayIneligible) {
			t.Fatalf("early replay=%v", err)
		}
	})

	for _, contentType := range []string{"text/event-stream", "application/grpc"} {
		t.Run(contentType, func(t *testing.T) {
			request, _ := http.NewRequest(http.MethodGet, "http://public.example.test/stream", nil)
			store, record := forwardInspectorRequest(t, inspector.ResourcePolicy{Enabled: true, CaptureRaw: true, CaptureResponseBody: true}, request, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", contentType)
				_, _ = w.Write([]byte("stream-data"))
			})
			if record.ResponseBodyState != inspector.StateUnsupported {
				t.Fatalf("response state=%q", record.ResponseBodyState)
			}
			if _, err := replayCapturedBody(t, store, record); !errors.Is(err, inspector.ErrReplayIneligible) {
				t.Fatalf("stream replay=%v", err)
			}
		})
	}
}

func TestInspectorForwardingCancelReleasesCaptureAndShutdownPurges(t *testing.T) {
	started := make(chan struct{})
	origin := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer origin.Close()
	store := inspector.NewStore()
	if err := store.SetPolicy("route_regression", inspector.ResourcePolicy{Enabled: true, CaptureRaw: true, CaptureRequestBody: true}); err != nil {
		t.Fatal(err)
	}
	forwarder := OriginStreamForwarder{Transport: &OriginHTTPTransport{}, Inspector: store}
	route := inspectorTestRoute(origin.Listener.Addr().String())
	route.ID = "route_regression"
	ctx, cancelForward := context.WithCancel(context.Background())
	daemon, client := net.Pipe()
	serveErr := make(chan error, 1)
	go func() { serveErr <- forwarder.serveHTTP(ctx, daemon, route, forwarder.Transport) }()
	if _, err := io.WriteString(client, "GET /cancel HTTP/1.1\r\nHost: public.example.test\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("origin did not receive request")
	}
	cancelForward()
	_ = client.Close()
	select {
	case <-serveErr:
	case <-time.After(time.Second):
		t.Fatal("canceled forwarding did not stop")
	}
	if stats := store.Stats(); stats.DaemonRecords != 1 {
		t.Fatalf("canceled capture left active reservation: %+v", stats)
	}

	runCtx, cancelRun := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { store.Run(runCtx); close(done) }()
	cancelRun()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("store lifecycle did not stop")
	}
	if stats := store.Stats(); stats.DaemonRecords != 0 || stats.DaemonBytes != 0 {
		t.Fatalf("shutdown retained canceled stream capture: %+v", stats)
	}
}

func TestInspectorPublicForwardingSurvivesCapturePressure(t *testing.T) {
	for _, mode := range []string{"disabled", "queue_full", "enabled"} {
		t.Run(mode, func(t *testing.T) {
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { raw, _ := io.ReadAll(r.Body); _, _ = w.Write(raw) }))
			defer origin.Close()
			store, registry := inspector.NewStore(), inspector.NewRegistry()
			if err := store.SetPolicy("preview_pressure", inspector.ResourcePolicy{Enabled: mode != "disabled"}); err != nil {
				t.Fatal(err)
			}
			if mode == "queue_full" {
				for i := 0; i < inspector.WorkQueueResourceMax; i++ {
					p, err := store.TryBegin("preview_pressure")
					if err != nil {
						t.Fatal(err)
					}
					defer store.Abandon(p)
				}
			}
			transport := &OriginHTTPTransport{}
			defer transport.CloseIdleConnections()
			f := OriginStreamForwarder{Transport: transport, Inspector: store, Registry: registry}
			route := inspectorTestRoute(origin.Listener.Addr().String())
			identity := CaptureIdentity{ResourceID: "preview_pressure", ResourceGeneration: 1, RouteGeneration: 1, TargetGeneration: 1, ExpiresAt: time.Now().Add(time.Minute)}
			server, client := net.Pipe()
			defer client.Close()
			done := make(chan error, 1)
			go func() {
				defer server.Close()
				done <- f.ServePublicHTTP(context.Background(), server, bufio.NewReader(server), route, identity)
			}()
			request, _ := http.NewRequest(http.MethodPost, "http://public.example.test/", bytes.NewBufferString("exact public bytes"))
			writeDone := make(chan error, 1)
			go func() { writeDone <- request.Write(client) }()
			response, err := http.ReadResponse(bufio.NewReader(client), request)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if err != nil || string(body) != "exact public bytes" || response.StatusCode != 200 {
				t.Fatalf("public forwarding changed: status=%d err=%v", response.StatusCode, err)
			}
			if err := <-writeDone; err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if mode == "enabled" {
				if _, ok := registry.Current(identity.ResourceID, time.Now()); !ok {
					t.Fatal("public capture omitted caller-owned replay binding")
				}
			}
		})
	}
}
