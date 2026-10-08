package splitdns

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

type stagedProxyFailure struct{ cause error }

func (e stagedProxyFailure) Error() string           { return "private credential and target details" }
func (e stagedProxyFailure) Unwrap() error           { return e.cause }
func (e stagedProxyFailure) DiagnosticStage() string { return "grant_issue" }

func TestProxyFailureIsDiagnosableOfflineAndRecovers(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !supportref.Valid(r.Header.Get(supportref.Header)) {
			t.Error("gateway request lost its support reference")
		}
		w.Header().Set(supportref.Header, "untrusted application response")
		_, _ = io.WriteString(w, "application ready")
	}))
	defer backend.Close()
	recorder := diagnostics.NewMemoryRecorder()
	restoreObserver := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) { _ = recorder.RecordFault(fault) })
	defer restoreObserver()
	restoreReporter := errorreport.Install(nil)
	defer restoreReporter()
	var mu sync.Mutex
	var references []string
	proxy, err := NewProxy(ProxyConfig{
		Routes: map[string]BrowserRoute{"3000.machine.local.pprbt.dev": {Address: netip.MustParseAddr("127.100.0.1"), Port: 3000}},
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			mu.Lock()
			references = append(references, supportref.FromContext(ctx))
			attempt := len(references)
			mu.Unlock()
			if attempt == 1 {
				return nil, stagedProxyFailure{cause: context.DeadlineExceeded}
			}
			return (&net.Dialer{}).DialContext(ctx, "tcp", backend.Listener.Addr().String())
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.transport.CloseIdleConnections()
	for attempt, status := range []int{http.StatusBadGateway, http.StatusOK} {
		request := httptest.NewRequest(http.MethodGet, "http://3000.machine.local.pprbt.dev/", nil)
		request.Header.Set(supportref.Header, "caller-controlled secret value")
		response := httptest.NewRecorder()
		proxy.ServeHTTP(response, request)
		reference := response.Header().Get(supportref.Header)
		if response.Code != status || !supportref.Valid(reference) {
			t.Fatalf("attempt %d status=%d reference valid=%v", attempt, response.Code, supportref.Valid(reference))
		}
		mu.Lock()
		gotReference := references[attempt]
		mu.Unlock()
		if reference != gotReference {
			t.Fatal("dial and browser references diverged")
		}
		if attempt == 0 {
			body := response.Body.String()
			if !strings.Contains(body, "authorize") || !strings.Contains(body, reference) || strings.Contains(body, "credential") || strings.Contains(body, "127.100") {
				t.Fatal("failure response lacks useful safe recovery")
			}
		}
	}
	events := recorder.Recent()
	if len(events) != 1 || events[0].Category != "grant_issue" || events[0].Fields["cause"] != "deadline_exceeded" || events[0].SupportReference != references[0] {
		t.Fatalf("offline typed failure missing: %#v", events)
	}
	if !errors.Is(stagedProxyFailure{cause: context.DeadlineExceeded}, context.DeadlineExceeded) {
		t.Fatal("cause wrapper violated errors.Is")
	}
}
