package tunnelv1

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/puddle/v2"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/tunnelcert"
)

type unusedPlatformWorkerIssuer struct{}

func (unusedPlatformWorkerIssuer) Issue(context.Context, tunnelcert.IssueRequest) (tunnelcert.CertificateBundle, error) {
	return tunnelcert.CertificateBundle{}, errors.New("unexpected certificate issue")
}

func TestPlatformCertificateWorkerTelemetryUsesTargetIdentityAndRetry(t *testing.T) {
	worker := &PlatformCertificateWorker{}
	var events []CertificateTelemetryEvent
	if err := worker.SetTelemetryObserver(func(_ context.Context, event CertificateTelemetryEvent) {
		events = append(events, event)
	}); err != nil {
		t.Fatal(err)
	}
	next := time.Date(2026, time.September, 1, 12, 1, 0, 0, time.UTC)
	worker.emitTelemetry(context.Background(), tunnelcert.PlatformPreviewTargetID, "", "issue", "failed", next)
	worker.emitTelemetry(context.Background(), tunnelcert.PlatformPreviewTargetID, "cert_platform_1", "issue", "success", time.Time{})
	if len(events) != 2 {
		t.Fatalf("telemetry events = %d, want 2", len(events))
	}
	if events[0].DomainID != tunnelcert.PlatformPreviewTargetID || events[0].Operation != "issue" || events[0].Outcome != "failed" || !events[0].NextRetryAt.Equal(next) {
		t.Fatalf("failure event = %+v", events[0])
	}
	if events[1].DomainID != tunnelcert.PlatformPreviewTargetID || events[1].CertificateID != "cert_platform_1" || events[1].Outcome != "success" || !events[1].NextRetryAt.IsZero() {
		t.Fatalf("success event = %+v", events[1])
	}
}

func TestPlatformCertificateWorkerLastErrorClearsOnlyOnCompletePass(t *testing.T) {
	worker := &PlatformCertificateWorker{}
	transient := errors.New("temporary issuer failure")
	worker.setLastError(transient)
	if !errors.Is(worker.LastError(), transient) {
		t.Fatalf("last error = %v, want transient failure", worker.LastError())
	}
	// Reconcile invokes this only after every target has completed successfully;
	// a retryable target leaves the value untouched for health explainability.
	worker.setLastError(nil)
	if err := worker.LastError(); err != nil {
		t.Fatalf("cleared last error = %v", err)
	}
}

func TestPlatformCertificateWorkerTelemetryDistinguishesIssueAndRenew(t *testing.T) {
	if got := platformCertificateOperation(false); got != "issue" {
		t.Fatalf("initial platform operation = %q", got)
	}
	if got := platformCertificateOperation(true); got != "renew" {
		t.Fatalf("replacement platform operation = %q", got)
	}
}

func TestPlatformCertificateWorkerBackoffRestoresTypedFailure(t *testing.T) {
	for code, want := range map[string]error{
		"issuer_unavailable":        tunnelcert.ErrIssuerUnavailable,
		"dns_challenge_unavailable": tunnelcert.ErrDNSChallengeUnavailable,
		"edge_not_ready":            tunnelcert.ErrCertificateNotReady,
		"unknown_code":              ErrPlatformCertificateUnavailable,
	} {
		if got := platformRetryError(code); !errors.Is(got, want) {
			t.Fatalf("retry code %q = %v, want %v", code, got, want)
		}
	}
}

func TestPlatformCertificateWorkerAcceptsEveryBuiltInTarget(t *testing.T) {
	for _, targetID := range []string{
		tunnelcert.PlatformPreviewTargetID,
		tunnelcert.PlatformTunnelTargetID,
		tunnelcert.PlatformRuntimeTargetID,
	} {
		if !validPlatformTargetID(targetID) {
			t.Fatalf("built-in platform target %q was rejected", targetID)
		}
	}
	if validPlatformTargetID("platform_cert_unknown_v1") {
		t.Fatal("unknown platform target was accepted")
	}
}

func TestPlatformCertificateRevokePreservesMarkerFailureWithCoordinatorCause(t *testing.T) {
	database, err := db.Open(config.Database{Driver: "postgres", DSN: "postgres://paperboat_test:paperboat_test@127.0.0.1:1/paperboat_test?sslmode=disable&connect_timeout=1"})
	if err != nil {
		t.Fatal(err)
	}
	store, err := tunnelcert.NewPlatformCertificateStore(database)
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	coordinator, err := tunnelcert.NewCoordinator(tunnelcert.Config{
		Store: tunnelcert.NewMemoryStore(), Locks: tunnelcert.NewMemoryLock(),
		Keys:   tunnelcert.ReferenceKeySource{Keys: map[string][]byte{"master/current": make([]byte, 32)}},
		Issuer: unusedPlatformWorkerIssuer{}, CAA: tunnelcert.MemoryCAA{Result: tunnelcert.CAAResult{State: "ready"}},
		Distributor: tunnelcert.NewMemoryDistributor(), MasterKeyReference: "master/current", OwnerID: "server_test",
	})
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	worker := &PlatformCertificateWorker{store: store, coordinator: coordinator, now: time.Now}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	err = worker.Revoke(context.Background(), tunnelcert.PlatformPreviewTargetID, "test_revocation")
	if !errors.Is(err, tunnelcert.ErrCertificateNotReady) || !errors.Is(err, puddle.ErrClosedPool) {
		t.Fatalf("revoke lost coordinator or target-marker failure: %v", err)
	}
}

func TestPlatformRetryDelayIsBoundedAndExponential(t *testing.T) {
	if got := platformRetryDelay(1); got != time.Minute {
		t.Fatalf("first retry delay = %s", got)
	}
	if got := platformRetryDelay(3); got != 4*time.Minute {
		t.Fatalf("third retry delay = %s", got)
	}
	if got := platformRetryDelay(100); got != time.Hour {
		t.Fatalf("bounded retry delay = %s", got)
	}
}
