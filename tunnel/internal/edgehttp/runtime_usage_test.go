package edgehttp

import (
	"context"
	"errors"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/admission"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/control"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/datacarrier"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordingUsage struct {
	mu       sync.Mutex
	records  []string
	exhaust  map[string]func()
	recorded map[string][2]uint64
}

func (u *recordingUsage) SubscribeQuota(authority string, _ time.Time, cancel func()) func() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.exhaust == nil {
		u.exhaust = map[string]func(){}
	}
	u.exhaust[authority] = cancel
	return func() {
		u.mu.Lock()
		delete(u.exhaust, authority)
		u.mu.Unlock()
	}
}

func (u *recordingUsage) Record(environment, route string, revision uint64, ingress, egress uint64, authority string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.recorded == nil {
		u.recorded = map[string][2]uint64{}
	}
	key := authority + "|" + environment + "|" + route
	got := u.recorded[key]
	u.recorded[key] = [2]uint64{got[0] + ingress, got[1] + egress}
	return nil
}

func TestRuntimeSessionBytesAreRecordedUnderItsAuthorityAndCutAtTheCap(t *testing.T) {
	usage := &recordingUsage{}
	edgeSide, hostSide := net.Pipe()
	defer hostSide.Close()
	ctx := withRuntimeUsage(context.Background(), runtimeUsageBinding{Authority: "session_a", Environment: "env_1", IssuedAt: time.Now()})
	binding, ok := runtimeUsageFrom(ctx)
	if !ok {
		t.Fatal("runtime usage binding not carried in the request context")
	}
	binding.Route, binding.Revision = "route_runtime", 4
	stream := meterRuntimeStream(usage, binding, edgeSide)
	go func() { buffer := make([]byte, 64); n, _ := hostSide.Read(buffer); _, _ = hostSide.Write(buffer[:n]) }()
	if _, err := stream.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Read(make([]byte, 64)); err != nil {
		t.Fatal(err)
	}
	if got := usage.recorded["session_a|env_1|route_runtime"]; got != [2]uint64{5, 5} {
		t.Fatalf("recorded under the session authority = %v, want 5 in / 5 out", got)
	}
	usage.mu.Lock()
	cut := usage.exhaust["session_a"]
	usage.mu.Unlock()
	if cut == nil {
		t.Fatal("the stream did not subscribe to its authority's quota")
	}
	cut()
	if _, err := stream.Write([]byte("more")); !errors.Is(err, ErrIngressQuotaExhausted) {
		t.Fatalf("write after the allowance was exhausted: %v, want ErrIngressQuotaExhausted", err)
	}
	_ = stream.Close()
	usage.mu.Lock()
	_, still := usage.exhaust["session_a"]
	usage.mu.Unlock()
	if still {
		t.Fatal("closing the stream left its quota subscription")
	}
}

func TestRuntimeUsageNeedsAnAuthority(t *testing.T) {
	if _, ok := runtimeUsageFrom(withRuntimeUsage(context.Background(), runtimeUsageBinding{Environment: "env_1"})); ok {
		t.Fatal("a credential without an assignment id produced a usage binding")
	}
	if _, ok := runtimeUsageFrom(context.Background()); ok {
		t.Fatal("a request without a runtime credential produced a usage binding")
	}
}

func TestBrowserTerminalUpstreamIsAccountedToItsAttachment(t *testing.T) {
	ctx := browserTerminalUsage(context.Background(), "env_1", control.BrowserTerminalAdmission{AttachmentID: "attachment_a"}, time.Now())
	binding, ok := runtimeUsageFrom(ctx)
	if !ok || binding.Authority != "attachment_a" || binding.Environment != "env_1" {
		t.Fatalf("browser terminal usage binding = %+v ok=%v", binding, ok)
	}
	if _, ok := runtimeUsageFrom(browserTerminalUsage(context.Background(), "env_1", control.BrowserTerminalAdmission{}, time.Now())); ok {
		t.Fatal("an admission without an attachment id produced a usage binding")
	}
}

// Through the whole gateway: a CLI file transfer riding the edge is NOT metered there; the host
// runtime reports it natively, and counting it at the edge too would bill it twice.
func TestCLIRuntimeTrafficThroughTheGatewayIsNotMeteredAtTheEdge(t *testing.T) {
	identity := testEdgePreviewIdentity(1, 1)
	server, client := testEdgePreviewCarrierPair(t, identity)
	registry, err := NewDataCarrierPreviewRegistry(DataCarrierPreviewRegistryConfig{BaseDomain: "runtime.example.test", ProcessEpoch: "edge_epoch_1"})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	value := DataCarrierPreviewRoute{RouteID: "runtime_route", Hostname: "machine.runtime.example.test", Kind: runtimewire.RouteKind, EdgeProcessEpoch: "edge_epoch_1", Revision: 3, AttachmentGeneration: 1, Server: server, ExpiresAt: time.Now().Add(time.Minute)}
	if err = registry.Attach(value); err != nil {
		t.Fatal(err)
	}
	usage := &recordingUsage{}
	transport, err := NewDataCarrierPreviewTransport(DataCarrierPreviewTransportConfig{Registry: registry, StreamOpenTimeout: time.Second, RuntimeUsage: usage})
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := NewGatewayWithTransports(Config{PreviewBaseDomain: "preview.example.test", TunnelBaseDomain: "tunnels.example.test", RuntimeBaseDomain: "runtime.example.test", MaxHeaderBytes: 4096, MaxBodyBytes: 1 << 20, Routes: NewPreviewCarrierRouteMatcher(registry), RuntimeCarrierTransport: transport, HelperAccess: helperAccessFunc(func(_ context.Context, token string) (admission.Claims, error) {
		return admission.Claims{JTI: "test", EnvironmentID: "env_machine", MachineID: identity.HostID, CredentialClass: "file_transfer", AssignmentID: "session_a", IssuedAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute)}, nil
	}), Revocations: revocationFunc(func(context.Context, admission.Claims) (bool, error) { return false, nil }), RevocationCheckInterval: time.Second}, "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		stream, _, err := client.AcceptStream(context.Background())
		if err != nil {
			done <- err
			return
		}
		done <- datacarrier.ServeHTTPStream(context.Background(), stream, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			_, _ = w.Write(body)
		}), 1<<20, time.Second)
	}()
	payload := strings.Repeat("x", 4096)
	req := httptest.NewRequest("PATCH", "https://machine.runtime.example.test/v1/file-transfers/transfer_1/content", strings.NewReader(payload))
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	gateway.ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Body.Len() != len(payload) {
		t.Fatalf("runtime request %d (%d bytes)", rec.Code, rec.Body.Len())
	}
	<-done
	usage.mu.Lock()
	got := usage.recorded["session_a|env_machine|runtime_route"]
	usage.mu.Unlock()
	if got != [2]uint64{} {
		t.Fatalf("CLI runtime bytes were metered at the edge too: %v", got)
	}
}
