package edgehttp

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
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
