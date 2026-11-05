package runtime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/privateproxyconfig"
)

func TestGuardPACHandsTheProxyPACToTheGuardAndWithdrawsIt(t *testing.T) {
	const pac = "function FindProxyForURL(url, host) { return 'PROXY 127.0.0.1:1'; }\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(pac)) }))
	defer server.Close()
	var sent []string
	g := newGuardPAC(func(_ context.Context, body string) error { sent = append(sent, body); return nil }, 0)
	if err := g.Install(t.Context(), server.URL+"/proxy-1.pac"); err != nil {
		t.Fatal(err)
	}
	if err := g.Refresh(t.Context(), server.URL+"/proxy-2.pac"); err != nil {
		t.Fatal(err)
	}
	if err := g.Remove(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 3 || sent[0] != pac || sent[1] != pac || sent[2] != "" {
		t.Fatalf("guard received %q", sent)
	}
}

func TestGuardPACRefusesAMissingPAC(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	g := newGuardPAC(func(context.Context, string) error { t.Fatal("must not register an unreadable PAC"); return nil }, 0)
	if err := g.Install(t.Context(), server.URL+"/proxy.pac"); err == nil {
		t.Fatal("a 404 PAC was accepted")
	}
}

// fakeGuard models the guard: registrations live only until it restarts, and a connection that predates a restart
// fails its first send (the real client sees a closed socket).
type fakeGuard struct {
	mu         sync.Mutex
	pac        string
	down       bool
	generation int // bumps on restart
	conn       int // generation of the client's connection; -1 none
	registered chan struct{}
}

func (f *fakeGuard) set(_ context.Context, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		f.conn = -1
		return ErrMachineGuardUnavailable
	}
	if f.conn != f.generation {
		stale := f.conn >= 0
		f.conn = f.generation
		if stale { // first send on a dead connection
			return errors.New("connection closed")
		}
	}
	f.pac = body
	select {
	case f.registered <- struct{}{}:
	default:
	}
	return nil
}

func (f *fakeGuard) restart() {
	f.mu.Lock()
	f.generation++
	f.pac = ""
	f.mu.Unlock()
}

func (f *fakeGuard) served() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pac
}

func TestRestartedGuardServesThePACAgainWithinTheRefreshBound(t *testing.T) {
	const pac = "function FindProxyForURL(url, host) { return 'PROXY 127.0.0.1:1'; }\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(pac)) }))
	defer server.Close()
	guard := &fakeGuard{conn: -1, registered: make(chan struct{}, 16)}
	const interval = 50 * time.Millisecond
	g := newGuardPAC(guard.set, interval)
	if err := g.Install(t.Context(), server.URL+"/proxy.pac"); err != nil {
		t.Fatal(err)
	}
	defer g.Remove(context.Background())
	if guard.served() != pac {
		t.Fatal("PAC not registered")
	}
	guard.restart()
	if guard.served() != "" {
		t.Fatal("restart kept the PAC")
	}
	deadline := time.After(interval*7/6 + 500*time.Millisecond) // the bound plus scheduling slack
	for guard.served() != pac {
		select {
		case <-guard.registered:
		case <-deadline:
			t.Fatal("the restarted guard did not serve the PAC again within the refresh bound")
		}
	}
	if err := g.Remove(t.Context()); err != nil || guard.served() != "" {
		t.Fatalf("remove: %v, served %q", err, guard.served())
	}
}

// A missing guard is a named, reported failure of the preview_recovery component, never a silent skip: the
// access service fails to start with the reason, which the host records as the component's degraded summary.
func TestMissingGuardFailsPrivateAccessWithANamedReason(t *testing.T) {
	g := newGuardPAC(func(context.Context, string) error {
		return fmt.Errorf("%w: dial unix: no such file", ErrMachineGuardUnavailable)
	}, 0)
	pac := "function FindProxyForURL(url, host) { return 'DIRECT'; }\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(pac)) }))
	defer server.Close()
	err := g.Install(t.Context(), server.URL+"/proxy.pac")
	if !errors.Is(err, ErrMachineGuardUnavailable) || privateproxyconfig.UnsupportedOnly(err) {
		t.Fatalf("missing guard must be a reported failure, not an unsupported skip: %v", err)
	}
	summary := "Host component preview_recovery failed to start: " + err.Error()
	if !strings.Contains(summary, "machine guard") || !strings.Contains(summary, "sudo pb daemon machine-guard install") {
		t.Fatalf("degraded reason does not name the cause and repair: %s", summary)
	}
}
