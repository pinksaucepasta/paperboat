package edgehttp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/edge/internal/datacarrier"
	"github.com/pinksaucepasta/paperboat/edge/internal/route"
)

// An upload that sends no body bytes for the idle timeout is cut (nginx client_body_timeout); one
// that keeps sending slowly is not, and time the proxy spends not reading (upstream backpressure)
// never counts. Over HTTP/1.1 and HTTP/2, whose read deadlines work differently.
func TestRequestBodyIdleTimeoutCutsOnlyAStalledUpload(t *testing.T) {
	const idle = 200 * time.Millisecond
	for _, h2 := range []bool{false, true} {
		results := make(chan error, 1)
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body := &proxyRequestBody{body: r.Body, controller: http.NewResponseController(w), idle: idle}
			var err error
			if r.URL.Path == "/backpressure" {
				var first [1]byte
				if _, err = io.ReadFull(body, first[:]); err == nil {
					time.Sleep(3 * idle)
					_, err = io.Copy(io.Discard, body)
				}
			} else {
				_, err = io.Copy(io.Discard, body)
			}
			results <- err
		}))
		server.EnableHTTP2 = h2
		t.Cleanup(server.Close)
		if h2 {
			server.StartTLS()
		} else {
			server.Start()
		}
		upload := func(path string, send func(*io.PipeWriter)) (error, time.Duration) {
			t.Helper()
			reader, writer := io.Pipe()
			defer writer.Close() // a failed check must not leave the handler waiting on the upload
			go send(writer)
			request, _ := http.NewRequest(http.MethodPost, server.URL+path, reader)
			started := time.Now()
			go func() {
				if response, err := server.Client().Do(request); err == nil {
					response.Body.Close()
				}
			}()
			select {
			case err := <-results:
				return err, time.Since(started)
			case <-time.After(5 * time.Second):
				t.Fatalf("h2=%t %s: handler never finished reading", h2, path)
				return nil, 0
			}
		}
		err, took := upload("/stalled", func(w *io.PipeWriter) { _, _ = w.Write([]byte("x")) }) // then silence
		if !errors.Is(err, errRequestBodyIdle) || took > 2*time.Second {
			t.Fatalf("h2=%t stalled upload: err=%v after %s, want errRequestBodyIdle soon after %s", h2, err, took, idle)
		}
		if err, _ := upload("/slow", func(w *io.PipeWriter) {
			for range 6 {
				_, _ = w.Write([]byte("x"))
				time.Sleep(idle / 2)
			}
			_ = w.Close()
		}); err != nil {
			t.Fatalf("h2=%t slow but moving upload was cut: %v", h2, err)
		}
		if err, _ := upload("/backpressure", func(w *io.PipeWriter) {
			_, _ = io.Copy(w, strings.NewReader("abc"))
			_ = w.Close()
		}); err != nil {
			t.Fatalf("h2=%t upload cut while the proxy was not reading: %v", h2, err)
		}
	}
}

// The proxy answers a cut upload with 408, not a gateway error.
func TestRequestBodyIdleIsAnsweredWith408(t *testing.T) {
	proxy := &httputil.ReverseProxy{}
	configureProxyDiagnostics(proxy, nil)
	recorder := httptest.NewRecorder()
	proxy.ErrorHandler(recorder, httptest.NewRequest(http.MethodPost, "/", nil), errors.Join(ErrDataCarrierRouteTransport, errRequestBodyIdle))
	if recorder.Code != http.StatusRequestTimeout || recorder.Header().Get("X-Paperboat-Error") != "request_body_timeout" {
		t.Fatalf("status %d error %q", recorder.Code, recorder.Header().Get("X-Paperboat-Error"))
	}
}

// Through the gateway and a durable carrier: a stalled upload ends with 408, not a hung request.
func TestStalledUploadThroughDurableTunnelGets408(t *testing.T) {
	identity := replicaIdentity("host_pinned", "connector_pinned", "session_pinned", 3)
	carrier, connector := testEdgePreviewCarrierPair(t, identity)
	rule := replicaRule()
	rule.Generation, rule.Target, rule.Hostname, rule.MatchType, rule.PathPrefix = 1, rule.RouteID, "upload.customer.test", route.MatchExact, "/"
	rule.AccessMode, rule.ObservedState, rule.Node, rule.EdgeProcessEpoch, rule.EdgeFailureDomain = "public", "ready", "edge_01", "epoch_0001", "region_01"
	registry, err := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	if err := registry.AttachReplica(carrier, "key", "thumb", replicaState(rule, 3, rule.AssignmentID, "region_01", time.Millisecond, 2)); err != nil {
		t.Fatal(err)
	}
	transport, err := NewDataCarrierRouteTransport(DataCarrierRouteTransportConfig{Registry: registry, StreamOpenTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	routes := route.NewRegistry("preview.example.test", "runtime.example.test")
	if err := routes.ApplyGeneration(context.Background(), 1, []route.RouteRule{rule}, func(context.Context, []route.RouteRule) error { return nil }, 0); err != nil {
		t.Fatal(err)
	}
	policy, err := NewGatewayWithTransports(Config{PreviewBaseDomain: "preview.example.test", TunnelBaseDomain: "tunnels.example.test", RuntimeBaseDomain: "runtime.example.test", MaxHeaderBytes: 16 << 10, MaxBodyBytes: 1 << 20, BodyIdleTimeout: 200 * time.Millisecond, Routes: routes}, "127.0.0.1:1", nil, transport)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(policy)
	defer server.Close()
	go func() {
		stream, _, acceptErr := connector.AcceptStream(context.Background())
		if acceptErr != nil {
			return
		}
		_ = datacarrier.ServeHTTPStream(context.Background(), stream, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
		}), 16<<10, 5*time.Second)
	}()
	reader, writer := io.Pipe()
	defer writer.Close()
	go func() { _, _ = writer.Write([]byte("x")) }() // then silence
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/upload", reader)
	request.Host = "upload.customer.test"
	client := server.Client()
	client.Timeout = 5 * time.Second
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("stalled upload: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusRequestTimeout {
		t.Fatalf("status %d, want 408", response.StatusCode)
	}
}
