package edgehttp

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/tunnel/internal/route"
)

func TestHoldSetBoundsOverallPerNameAndKeepsAReserve(t *testing.T) {
	h := newHoldSet(10, 3, 4)
	var releases []func()
	take := func(name string, known bool) bool {
		release, ok := h.acquire(name, known)
		if ok {
			releases = append(releases, release)
		}
		return ok
	}
	for i := 0; i < 3; i++ {
		if !take("a.example.test", false) {
			t.Fatalf("hold %d of one name refused below the per-name bound", i)
		}
	}
	if take("a.example.test", false) || take("a.example.test", true) {
		t.Fatal("a fourth hold on one name was granted")
	}
	// Unknown names stop at max - reserve = 6 in all; the rest is for known names.
	for _, name := range []string{"b.example.test", "c.example.test"} {
		for i := 0; i < 3; i++ {
			if want := name == "b.example.test"; take(name, false) != want {
				t.Fatalf("unknown hold on %s: want granted=%v", name, want)
			}
		}
	}
	if take("d.example.test", false) {
		t.Fatal("unknown names took the reserve")
	}
	for i := 0; i < 4; i++ {
		if !take("known"+string(rune('a'+i))+".example.test", true) {
			t.Fatalf("known hold %d refused although the reserve is free", i)
		}
	}
	if take("known.example.test", true) {
		t.Fatal("holds exceeded the overall bound")
	}
	for _, release := range releases {
		release()
	}
	if h.total != 0 || len(h.names) != 0 {
		t.Fatalf("holds left after release: %d %v", h.total, h.names)
	}
}

// A name that cannot be ours (not under our domains, not in the snapshot) is answered at once, never held for the
// grace: random names right after a deploy must not occupy the holds a real preview needs.
func TestSharedTLSNameThatCannotBeOursIsNotHeld(t *testing.T) {
	match, _ := browserTestMatch()
	clientTLS, serverTLS, _, _, _ := task24Certificates(t, match.Host)
	clientTLS.ServerName = match.Host
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	registry, _ := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 4})
	defer registry.Close()
	var admitted atomic.Bool
	time.AfterFunc(700*time.Millisecond, func() { admitted.Store(true) })
	shared := newSharedTLSListener(raw, &quotaDeniedAuthority{}, registry, func(string) bool { return false }, make(chan struct{}, 4), "", func(string) bool { return admitted.Load() }, nil, nil)
	shared.ServeUnknownNamesWithPage(serverTLS)
	shared.routeGrace = func() time.Duration { return 5 * time.Second }
	shared.holdableName = func(string) bool { return false }
	server := &http.Server{TLSConfig: serverTLS, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })}
	defer server.Close()
	go server.ServeTLS(shared, "", "")
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: clientTLS, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", raw.Addr().String())
	}}}
	start := time.Now()
	response, err := client.Get("https://" + match.Host + "/")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound || time.Since(start) > 400*time.Millisecond {
		t.Fatalf("status %d after %v, want 404 at once", response.StatusCode, time.Since(start))
	}
}

// A client that leaves while its name is held ends the hold at once; before, the connection kept polling the
// control plane for the whole grace.
func TestSharedTLSHoldEndsWhenTheClientLeaves(t *testing.T) {
	match, _ := browserTestMatch()
	clientTLS, serverTLS, _, _, _ := task24Certificates(t, match.Host)
	clientTLS.ServerName = match.Host
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	registry, _ := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 4})
	defer registry.Close()
	shared := newSharedTLSListener(raw, &quotaDeniedAuthority{}, registry, func(string) bool { return false }, make(chan struct{}, 4), "", func(string) bool { return false }, nil, nil)
	defer shared.Close()
	shared.ServeUnknownNamesWithPage(serverTLS)
	shared.routeGrace = func() time.Duration { return time.Minute }
	shared.holdableName = func(string) bool { return true }
	conn, err := net.Dial("tcp", raw.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = tls.Client(conn, clientTLS).Handshake() }()
	held := func() int { shared.held.mu.Lock(); defer shared.held.mu.Unlock(); return shared.held.total }
	waitFor := func(want int, what string) {
		t.Helper()
		for deadline := time.Now().Add(2 * time.Second); held() != want; time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("%s: %d holds, want %d", what, held(), want)
			}
		}
	}
	waitFor(1, "the name is held while the client waits")
	_ = conn.Close()
	waitFor(0, "the client left")
}

// The HTTP-layer hold is bounded too: past the per-host bound a request is answered 404 at once, and the held ones
// are served when their route attaches.
func TestHTTPRouteHoldIsBounded(t *testing.T) {
	routes := route.NewRegistry("preview.example.test", "runtime.example.test")
	policy, err := New(Config{PreviewBaseDomain: "preview.example.test", TunnelBaseDomain: "tunnels.example.test", RuntimeBaseDomain: "runtime.example.test",
		MaxHeaderBytes: 4096, MaxBodyBytes: 4096, Routes: routes, RouteGrace: func() time.Duration { return 2 * time.Second }}, http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	release := make([]func(), 0, routeHoldPerName)
	for i := 0; i < routeHoldPerName; i++ {
		done, ok := policy.routeHolds.acquire("busy.preview.example.test", true)
		if !ok {
			t.Fatalf("hold %d refused below the bound", i)
		}
		release = append(release, done)
	}
	defer func() {
		for _, done := range release {
			done()
		}
	}()
	r := httptest.NewRequest("GET", "https://busy.preview.example.test/", nil)
	r.Host = "busy.preview.example.test"
	w := httptest.NewRecorder()
	start := time.Now()
	policy.ServeHTTP(w, r)
	if w.Code != 404 || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("a request past the hold bound: %d after %v, want 404 at once", w.Code, time.Since(start))
	}
}

// One name may hold at most a quarter of the listener's slots for its connections' lives, so a flood
// aimed at one preview cannot take every slot; a closed connection gives its share back.
func TestSharedTLSNameShareIsBounded(t *testing.T) {
	match, _ := browserTestMatch()
	clientTLS, serverTLS, _, _, _ := task24Certificates(t, match.Host)
	clientTLS.ServerName = match.Host
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	registry, _ := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 4})
	defer registry.Close()
	shared := newSharedTLSListener(raw, &quotaDeniedAuthority{}, registry, func(string) bool { return false }, make(chan struct{}, 4), "", func(string) bool { return true }, nil, nil)
	server := &http.Server{TLSConfig: serverTLS, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })}
	defer server.Close()
	go server.ServeTLS(shared, "", "")
	dial := func() (*tls.Conn, error) {
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 2 * time.Second}, "tcp", raw.Addr().String(), clientTLS)
		if err != nil {
			return nil, err
		}
		// A refused connection is closed after the handshake would have started; a read shows which.
		_ = conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		if _, err := conn.Read(make([]byte, 1)); err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
			_ = conn.Close()
			return nil, err
		}
		_ = conn.SetReadDeadline(time.Time{})
		return conn, nil
	}
	// Four slots: the share is two (one connection plus a reconnect).
	first, err := dial()
	if err != nil {
		t.Fatalf("first connection: %v", err)
	}
	second, err := dial()
	if err != nil {
		t.Fatalf("second connection inside the 2-slot share: %v", err)
	}
	started := time.Now()
	if third, err := dial(); err == nil {
		third.Close()
		t.Fatal("a third connection to the same name passed its 2-slot share")
	}
	if waited := time.Since(started); waited < nameShareWait {
		t.Fatalf("refused after %v, before the %v share wait", waited, nameShareWait)
	}
	// A reconnect right after a close is admitted: it waits for the closed connection's share to come
	// back instead of being refused while the server has not yet noticed the close.
	first.Close()
	reconnect, err := dial()
	if err != nil {
		t.Fatalf("immediate reconnect after a close refused: %v", err)
	}
	reconnect.Close()
	second.Close()
}
