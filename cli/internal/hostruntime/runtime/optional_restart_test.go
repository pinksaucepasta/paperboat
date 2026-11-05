//go:build darwin || linux

package runtime

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/offlineaudit"
	"github.com/pinksaucepasta/paperboat/internal/privatepreviewproxy"
)

// The stable host retries a failed optional component: Start, a failure, its rollback Shutdown, then Start
// again must work, and a later Shutdown stops what the retry started (hostd/daemon.go retryComponent).

type failOnceService struct{ starts, shutdowns int }

func (s *failOnceService) Start(context.Context) error {
	s.starts++
	if s.starts == 1 {
		return errors.New("first start fails")
	}
	return nil
}
func (s *failOnceService) Shutdown(context.Context) error { s.shutdowns++; return nil }

type readyEnrollment struct{}

func (readyEnrollment) Ensure(context.Context) error { return nil }

// retryLikeTheStableHost runs one failed Start with its rollback, then the retry and the final Shutdown.
func retryLikeTheStableHost(t *testing.T, service Service, failingCtx context.Context) {
	t.Helper()
	ctx := context.Background()
	if err := service.Start(failingCtx); err == nil {
		t.Fatal("the first Start did not fail")
	}
	if err := service.Shutdown(ctx); err != nil {
		t.Fatalf("rollback Shutdown after a failed Start: %v", err)
	}
	if err := service.Start(ctx); err != nil {
		t.Fatalf("retried Start: %v", err)
	}
	if err := service.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown after the retry: %v", err)
	}
}

// authorization group: peer enrollment started, a later member failed, so the group stopped it; the retry
// starts it again.
func TestAuthorizationGroupRestartsAfterAFailedStart(t *testing.T) {
	retryLikeTheStableHost(t, serviceGroup{newPeerEnrollmentRuntimeService(readyEnrollment{}, time.Second), &failOnceService{}}, context.Background())
}

// authorization group: layer environment never started (an earlier member failed); its rollback Shutdown
// must not fail the host's start or block the retry.
func TestLayerEnvironmentRestartsAfterAFailedStart(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer server.Close()
	credentials := &layerTestCredentials{token: "helper-token", proof: []byte("helper-proof")}
	layers := newLayerEnvironmentService(t.TempDir(), layerServiceURL(t, server.URL), server.Client().Transport, layerRegistration(t), credentials, credentials)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	retryLikeTheStableHost(t, layers, canceled)
}

// runtime_observation group: offline audit started, a later member failed, so the group stopped it.
func TestRuntimeObservationGroupRestartsAfterAFailedStart(t *testing.T) {
	queue, err := offlineaudit.Open(filepath.Join(t.TempDir(), "offline-audit"), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	retryLikeTheStableHost(t, serviceGroup{&offlineAuditService{uploader: &offlineaudit.Uploader{Queue: queue}}, &failOnceService{}}, context.Background())
}

type flakyPAC struct{ recovers int }

func (p *flakyPAC) Recover(context.Context) error {
	p.recovers++
	if p.recovers == 1 {
		return errors.New("PAC settings are busy")
	}
	return nil
}
func (*flakyPAC) Install(context.Context, string) error { return nil }
func (*flakyPAC) Refresh(context.Context, string) error { return nil }
func (*flakyPAC) Remove(context.Context) error          { return nil }

type emptyAccessSource struct{}

func (emptyAccessSource) Snapshot(context.Context) ([]privatepreviewproxy.AccessRoute, error) {
	return nil, nil
}
func (emptyAccessSource) Open(context.Context, string) (io.ReadWriteCloser, error) {
	return nil, errors.New("no route")
}

// preview_recovery: the private access proxy failed to start (its PAC settings busy, its port held); the
// assembly is not torn down, so the retry starts it and the final Shutdown tears it down once.
func TestPreviewRecoveryRestartsAfterAFailedStart(t *testing.T) {
	access, err := privatepreviewproxy.NewAccessService(privatepreviewproxy.AccessServiceConfig{
		Proxy: privatepreviewproxy.AccessProxyConfig{Source: emptyAccessSource{}}, Configurator: &flakyPAC{},
	})
	if err != nil {
		t.Fatal(err)
	}
	runtimeDone := make(chan struct{})
	assembly := &productionPreviewAssembly{privateAccess: access, runtimeDone: runtimeDone}
	ctx := context.Background()
	if err := assembly.Start(ctx); err == nil {
		t.Fatal("the first Start did not fail")
	}
	if err := assembly.Shutdown(ctx); err != nil {
		t.Fatalf("rollback Shutdown after a failed Start: %v", err)
	}
	select {
	case <-runtimeDone:
		t.Fatal("the rollback of a failed Start tore the assembly down, so no retry can serve previews")
	default:
	}
	if err := assembly.Start(ctx); err != nil {
		t.Fatalf("retried Start: %v", err)
	}
	if err := assembly.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown after the retry: %v", err)
	}
	select {
	case <-runtimeDone:
	default:
		t.Fatal("the final Shutdown did not tear the started assembly down")
	}
}

// managed_ssh_authority: a failed Start sets no state (managed_ssh_service.go Start checks before it starts).
func TestManagedSSHAuthorityRestartsAfterAFailedStart(t *testing.T) {
	gate := newMachineCapabilityController(nil)
	service := &managedSSHService{gate: gate, interval: time.Millisecond, timeout: time.Second, initialize: func(context.Context) (Service, error) { return &optionalSSHAuthority{}, nil }}
	var missing context.Context
	retryLikeTheStableHost(t, service, missing)
}
