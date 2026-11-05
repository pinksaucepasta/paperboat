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
	"github.com/pinksaucepasta/paperboat/shared/httptransport"
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

// A server rollout reloads the proxy: it answers the wait it holds, closes kept-alive
// connections, and cuts a wait written onto one as it closes. Every change is still delivered
// at once, with no failed wait (a failure would back off at least a second).
func TestHubDeliversAcrossAProxyReload(t *testing.T) {
	_, subscriptions, plane := eventstest.Start(t, time.Hour, machineevents.TopicInventory)
	held := func(n int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for len(plane.Requests()) < n && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if len(plane.Requests()) < n {
			t.Fatalf("wait %d never arrived", n)
		}
	}
	delivered := func(step string) {
		t.Helper()
		published := time.Now()
		plane.Publish(machineevents.TopicInventory)
		select {
		case <-subscriptions[0].Changed():
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: change not delivered", step)
		}
		if took := time.Since(published); took > 900*time.Millisecond {
			t.Fatalf("%s: change took %v; a wait failed and backed off", step, took)
		}
	}
	held(1)
	plane.KeepAlives(false) // the reload: the old configuration stops reusing connections
	delivered("wait held across the reload")
	plane.KeepAlives(true)
	held(2)
	plane.CutNext(1) // the wait after the next answer is written onto a connection the proxy closes
	delivered("before the cut")
	cut := time.Now()
	held(4) // the cut wait and its resend
	if took := time.Since(cut); took > 900*time.Millisecond {
		t.Fatalf("the cut wait was resent after %v; it failed and backed off", took)
	}
	delivered("after the cut")
}
