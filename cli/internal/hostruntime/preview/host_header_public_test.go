package preview

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/inspector"
	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

// A public preview with --host-header must send the origin that Host even when nobody inspects the preview: without
// the inspector the stream used to be copied byte for byte, so the visitor's public Host reached the app (a dev server
// with a host allow-list answered 403 on prod). The Host is read in the origin, the visitor's own stays public.
func TestPublicPreviewHostHeaderReachesTheOriginWithoutTheInspector(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	hosts := make(chan string, 2)
	origin := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		hosts <- request.Host
		_, _ = io.WriteString(writer, "ok")
	}))
	defer origin.Close()

	identity := testPreviewCarrierIdentity(1)
	pair := newPreviewCarrierPair(t, ctx, identity)
	defer pair.close()
	hub, err := NewDataCarrierPreviewHub(ctx, DataCarrierPreviewHubConfig{Active: pair.active, Identity: identity})
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	lease := Lease{ID: "preview-host", AccountID: identity.AccountID, Generation: 1, AccessMode: "public", Endpoint: "https://public.example.test",
		Target: LeaseTarget{Scheme: "http", Address: origin.Listener.Addr().String(), HostHeader: "allowed.example.test"}, LeaseDeadline: time.Now().UTC().Add(time.Hour)}
	carrier, err := NewDataCarrierPreviewCarrier(DataCarrierPreviewCarrierConfig{Hub: hub, Identity: identity, RouteID: "pvc_rte_host", Inspector: inspector.NewStore(), Registry: inspector.NewRegistry(),
		DialOrigin: func(ctx context.Context, target LeaseTarget) (io.ReadWriteCloser, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", target.Address)
		}})
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- carrier.Run(ctx, lease, func(Lease) error { close(ready); return nil }) }()
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("carrier not ready")
	}

	stream, err := pair.edge.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := connectorprotocol.WriteStreamOpen(stream, testPreviewStreamOpen(identity, "pvc_rte_host", "request-host")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(stream, "GET / HTTP/1.1\r\nHost: public.example.test\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(stream), nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	_ = stream.Close()
	select {
	case host := <-hosts:
		if host != "allowed.example.test" {
			t.Fatalf("origin saw Host %q, want the --host-header value", host)
		}
	case <-ctx.Done():
		t.Fatal("origin was not reached")
	}
	cancel()
	<-done
}
