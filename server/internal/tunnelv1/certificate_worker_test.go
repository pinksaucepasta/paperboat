package tunnelv1

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/server/internal/testutil"
	"github.com/pinksaucepasta/paperboat/server/internal/tunnelcert"
)

func TestCertificateDomainUsesConfiguredIssuerAndRenewalWindow(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	row := dbsqlc.TunnelDomain{
		ID:                          "dom_01",
		AccountID:                   "acct_01",
		TunnelID:                    "tun_01",
		Hostname:                    "*.example.test",
		OwnershipChallengeReference: "dns-challenge://dom_01",
		OwnershipState:              "verified",
		CertificateStrategy:         "managed",
		CertificateState:            "ready",
		CertificateExpiresAt:        sql.NullTime{Time: now.Add(20 * 24 * time.Hour), Valid: true},
		Generation:                  3,
	}
	for _, test := range []struct {
		name        string
		renewBefore time.Duration
		due         bool
	}{
		{name: "short window", renewBefore: 15 * 24 * time.Hour, due: false},
		{name: "long window", renewBefore: 60 * 24 * time.Hour, due: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			domain, err := certificateDomainWithConfig(row, nil, now, test.renewBefore, "acme.example")
			if err != nil {
				t.Fatal(err)
			}
			if domain.Issuer != "acme.example" {
				t.Fatalf("issuer = %q, want configured issuer", domain.Issuer)
			}
			if domain.RenewalDue != test.due {
				t.Fatalf("renewal due = %v, want %v", domain.RenewalDue, test.due)
			}
		})
	}
}

func TestCertificateWorkerDeferralRequiresPurePolicyOutcome(t *testing.T) {
	for _, err := range []error{
		tunnelcert.ErrCAABlocked,
		tunnelcert.ErrCertificateNotReady,
		fmt.Errorf("placement pending: %w", tunnelcert.ErrCertificateNotReady),
		fmt.Errorf("certificate challenge: %w", tunnelcert.ErrDNSChallengePending),
	} {
		if !expectedCertificateDeferral(err) {
			t.Fatalf("pure certificate policy deferral was classified as an operational failure: %v", err)
		}
	}
	if expectedCertificateDeferral(errors.Join(tunnelcert.ErrCertificateNotReady, syscall.EIO)) {
		t.Fatal("independent storage failure was hidden by distribution readiness")
	}
	if expectedCertificateDeferral(errors.Join(tunnelcert.ErrCAABlocked, syscall.EIO)) {
		t.Fatal("independent operational cause was hidden by CAA policy state")
	}
}

func TestCertificateDomainDistributionFailureDoesNotRequestRenewal(t *testing.T) {
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	row := dbsqlc.TunnelDomain{ID: "dom_01", AccountID: "acct_01", TunnelID: "tun_01", RouteID: "route_01",
		Hostname: "app.example.test", OwnershipChallengeReference: "dns-challenge://dom_01", OwnershipState: "verified",
		CertificateStrategy: "managed", CertificateReference: sql.NullString{String: "tcert_existing", Valid: true},
		CertificateExpiresAt: sql.NullTime{Time: now.Add(89 * 24 * time.Hour), Valid: true}, Generation: 76}
	for _, state := range []string{"ready", "renewing", "failed"} {
		row.CertificateState = state
		domain, err := certificateDomainWithConfig(row, nil, now, 30*24*time.Hour, "letsencrypt")
		if err != nil || domain.RenewalDue {
			t.Fatalf("state %s: due=%v err=%v", state, domain.RenewalDue, err)
		}
	}
	row.CertificateExpiresAt.Time = now.Add(29 * 24 * time.Hour)
	domain, err := certificateDomainWithConfig(row, nil, now, 30*24*time.Hour, "letsencrypt")
	if err != nil || !domain.RenewalDue {
		t.Fatalf("renewal window: due=%v err=%v", domain.RenewalDue, err)
	}
	row.CertificateExpiresAt.Valid = false
	domain, err = certificateDomainWithConfig(row, nil, now, 30*24*time.Hour, "letsencrypt")
	if err != nil || !domain.RenewalDue {
		t.Fatalf("missing expiry: due=%v err=%v", domain.RenewalDue, err)
	}
}

type placementPendingIssuer struct{ calls atomic.Int32 }

func (i *placementPendingIssuer) Issue(context.Context, tunnelcert.IssueRequest) (tunnelcert.CertificateBundle, error) {
	i.calls.Add(1)
	return tunnelcert.CertificateBundle{}, tunnelcert.ErrIssuerUnavailable
}

func TestCertificateWorkerRetriesPreviewPlacementWithoutExitingOnPostgres(t *testing.T) {
	f := newTRK07PostgresFixture(t)
	account, host := f.addAccount(t, "certificate-placement")
	ctx := context.Background()
	preview, domain := "prv_certificate_placement_"+f.suffix, "dom_certificate_placement_"+f.suffix
	endpoint := "https://" + testutil.EndpointLabel(preview) + ".preview.example.test"
	if _, err := f.database.SQL().ExecContext(ctx, `INSERT INTO paperboat.preview_leases(owner_session_kind,id,endpoint,account_id,actor_id,owner_machine_id,owner_session_id,target_scheme,target_address,access_mode,lease_deadline) VALUES('foreground',$1,$2,$3,$3,$4,'certificate-owner','http','127.0.0.1:3000','public',now()+interval '1 hour')`, preview, endpoint, account, host); err != nil {
		t.Fatal(err)
	}
	if _, err := f.database.SQL().ExecContext(ctx, `INSERT INTO paperboat.preview_domains(id,account_id,preview_id,preview_generation,hostname,match_type,ownership_challenge_reference,ownership_state,dns_target,certificate_strategy,certificate_state,caa_state,conflict_state) VALUES($1,$2,$3,1,$4,'exact','dns:placement','verified','edge.example.test','managed','issuing','ready','clear')`, domain, account, preview, "placement-"+testutil.EndpointLabel(preview)+".example.test"); err != nil {
		t.Fatal(err)
	}
	issuer := &placementPendingIssuer{}
	// The coordinator must read the preview's issuance authority from the same
	// PostgreSQL rows the worker lists; an empty in-memory authority table would
	// refuse the refresh before placement is checked.
	store, err := tunnelcert.NewSQLStore(f.database)
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := tunnelcert.NewCoordinator(tunnelcert.Config{Store: store, Locks: tunnelcert.NewMemoryLock(), Keys: tunnelcert.ReferenceKeySource{Keys: map[string][]byte{"master/current": make([]byte, 32)}}, Issuer: issuer, CAA: tunnelcert.MemoryCAA{Result: tunnelcert.CAAResult{State: "ready"}}, Distributor: tunnelcert.NewMemoryDistributor(), MasterKeyReference: "master/current", OwnerID: "certificate-placement-worker"})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := NewCertificateWorker(f.database, coordinator, 30*24*time.Hour, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan CertificateTelemetryEvent, 8)
	if err := worker.SetTelemetryObserver(func(_ context.Context, event CertificateTelemetryEvent) {
		select {
		case events <- event:
		default:
		}
	}); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- worker.Worker(10*time.Millisecond, 100)(runCtx) }()
	for i := 0; i < 2; i++ {
		select {
		case event := <-events:
			if event.DomainID != domain || event.Outcome != "deferred" {
				t.Fatalf("pending placement telemetry = %+v", event)
			}
		case err := <-done:
			t.Fatalf("certificate worker exited on pending placement: %v", err)
		case <-time.After(5 * time.Second):
			t.Fatal("certificate worker did not retry pending placement")
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("worker cancellation = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("certificate worker did not stop on cancellation")
	}
	if issuer.calls.Load() != 0 {
		t.Fatal("pending placement spent a certificate authority order")
	}
	row, err := f.database.Queries().GetPreviewDomainV1(ctx, dbsqlc.GetPreviewDomainV1Params{DomainID: domain, AccountID: account, PreviewID: preview})
	if err != nil {
		t.Fatal(err)
	}
	failedAt := time.Now().UTC()
	if err := worker.recordPreviewCertificateFailure(ctx, row, "dns_challenge_unavailable", failedAt); err != nil {
		t.Fatal(err)
	}
	persisted, err := f.database.Queries().GetPreviewDomainV1(ctx, dbsqlc.GetPreviewDomainV1Params{DomainID: domain, AccountID: account, PreviewID: preview})
	if err != nil || persisted.CertificateState != "failed" || persisted.CertificateFailureCode.String != "dns_challenge_unavailable" || !persisted.CertificateRenewalAttemptedAt.Valid || persisted.Generation != row.Generation+1 {
		t.Fatalf("durable certificate failure = %+v, err=%v", persisted, err)
	}
	worker.now = func() time.Time { return failedAt.Add(59 * time.Second) }
	if changed, err := worker.Reconcile(ctx, 100); err != nil || changed != 0 {
		t.Fatalf("cooldown reconciliation changed=%d err=%v", changed, err)
	}
	worker.now = func() time.Time { return failedAt.Add(61 * time.Second) }
	if changed, err := worker.Reconcile(ctx, 100); err != nil || changed != 1 {
		t.Fatalf("retry after cooldown changed=%d err=%v", changed, err)
	}
	if issuer.calls.Load() != 0 {
		t.Fatal("cooldown retry bypassed absent-placement authorization")
	}
}
