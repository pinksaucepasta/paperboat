package machineevents_test

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machineevents"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machineevents/eventstest"
	"github.com/pinksaucepasta/paperboat/internal/httptransport"
)

// In production the server holds a wait for 30 s and the shared transport's response-header timeout
// was also 30 s, so every empty wait failed, the hub never became healthy, and every daemon re-listed
// the account's machines every 15 s. With the production transport and a header timeout shorter than
// the hold, empty waits must still succeed and keep the hub healthy.
func TestHubStaysHealthyAcrossEmptyWaitsLongerThanTheHeaderTimeout(t *testing.T) {
	production := func(server *httptest.Server) http.RoundTripper {
		roots := x509.NewCertPool()
		roots.AddCert(server.Certificate())
		config := httptransport.DevelopmentConfig()
		config.ProxySource = httptransport.StaticProxySource{}
		config.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
		config.ResponseHeaderTimeout = eventstest.Hold / 3
		transport, err := httptransport.New(config)
		if err != nil {
			t.Fatal(err)
		}
		return transport
	}
	hub, _, plane := eventstest.StartWith(t, time.Hour, production, machineevents.TopicInventory)
	deadline := time.Now().Add(10 * time.Second)
	for len(plane.Requests()) < 4 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := len(plane.Requests()); got < 4 {
		t.Fatalf("only %d waits in 10 s; empty waits are failing and backing off", got)
	}
	if !hub.Healthy() {
		t.Fatal("hub is unhealthy after empty waits held longer than the response-header timeout")
	}
}
