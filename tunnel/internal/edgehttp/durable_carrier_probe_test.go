package edgehttp

import (
	"bufio"
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/route"
	"io"
	"net/http"
	"testing"
	"time"
)

func TestDurableProbeHeaderFailureClosesStalledBodyAndRecovers(t *testing.T) {
	identity := replicaIdentity("host_pinned", "connector_pinned", "session_pinned", 3)
	carrier, connector := testEdgePreviewCarrierPair(t, identity)
	rule := replicaRule()
	rule.Protocol = "http"
	registry, err := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	if err := registry.AttachReplica(carrier, "key", "thumb", replicaState(rule, 3, rule.AssignmentID, "region_01", time.Millisecond, 2)); err != nil {
		t.Fatal(err)
	}
	worker := make(chan error, 1)
	go func() {
		for _, status := range []string{"503 Service Unavailable", "200 OK"} {
			stream, _, err := connector.AcceptStream(t.Context())
			if err != nil {
				worker <- err
				return
			}
			if _, err = http.ReadRequest(bufio.NewReader(stream)); err == nil {
				_, err = io.WriteString(stream, "HTTP/1.1 "+status+"\r\nContent-Length: 1000000\r\n\r\n")
			}
			if err != nil {
				_ = stream.Close()
				worker <- err
				return
			}
			var b [1]byte
			_, err = stream.Read(b[:])
			_ = stream.Close()
			if err == nil {
				worker <- errors.New("probe did not close stream")
				return
			}
		}
		worker <- nil
	}()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err = registry.ProbeRoutes(ctx, []route.RouteRule{rule})
	var status interface{ DiagnosticStatus() int }
	if !errors.As(err, &status) || status.DiagnosticStatus() != 503 {
		t.Fatalf("probe status = %v", err)
	}
	if err := registry.ProbeRoutes(ctx, []route.RouteRule{rule}); err != nil {
		t.Fatalf("healthy retry = %v", err)
	}
	select {
	case err := <-worker:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("probe workers did not stop")
	}
}
