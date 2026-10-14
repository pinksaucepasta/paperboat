package tunnelcert

import (
	"context"
	"errors"
	"testing"
	"time"
)

func recoveryFixture(t *testing.T, bundle CertificateBundle) (*Coordinator, *MemoryStore, *testIssuer, Domain, StoredCertificate) {
	t.Helper()
	issuer := &testIssuer{bundle: bundle}
	coordinator, store, _, _ := coordinatorForTest(t, issuer)
	domain := testDomain("app.example.test", StrategyDelegatedDNS01)
	domain.RouteID = "route_original"
	result, err := coordinator.Ensure(context.Background(), domain)
	if err != nil {
		t.Fatal(err)
	}
	row := store.all[result.Certificate.Reference[len("tcert_"):]]
	// Represents the durable encrypted row left by the old pre-placement
	// issuance path. The leaf was minted/sealed by the real coordinator above.
	row.State = StateFailed
	row.FailureCode = "no_ready_edge_targets"
	store.all[row.ID] = row
	delete(store.current, domain.ID)
	issuer.err = ErrIssuerRateLimited
	domain.Generation++
	domain.EdgeTargets[0].Generation++
	return coordinator, store, issuer, domain, row
}

func TestCoordinatorResumesIssuedLeafAfterPlacementFailure(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	coordinator, store, issuer, domain, old := recoveryFixture(t, testBundle(t, []string{"app.example.test"}, now, 90*24*time.Hour))
	result, err := coordinator.Ensure(context.Background(), domain)
	if err != nil {
		t.Fatalf("existing issued leaf recovery: %v", err)
	}
	if len(issuer.seen) != 1 {
		t.Fatalf("recovery contacted CA: %d calls", len(issuer.seen))
	}
	if !result.Issued || result.Certificate.State != StateActive || result.Certificate.CertificateGeneration != old.CertificateGeneration+1 || result.Certificate.DomainGeneration != domain.Generation {
		t.Fatalf("recovered lifecycle: %+v", result.Certificate)
	}
	active, found, err := store.Current(context.Background(), domain.ID)
	if err != nil || !found || active.Fingerprint != old.Fingerprint || active.RouteID != domain.RouteID {
		t.Fatal("recovery lost issued leaf or current route binding")
	}
	history := store.all[old.ID]
	if history.State != StateFailed || history.DomainGeneration != old.DomainGeneration || history.FailureCode != old.FailureCode {
		t.Fatal("recovery mutated failed history")
	}
}

func TestCoordinatorFailedLeafRecoveryRejectsInvalidBinding(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	bundle := testBundle(t, []string{"app.example.test"}, now, 90*24*time.Hour)
	for _, tc := range []struct {
		name   string
		mutate func(*StoredCertificate)
		want   error
	}{
		{"account", func(r *StoredCertificate) { r.AccountID = "another_account" }, ErrGenerationConflict},
		{"route", func(r *StoredCertificate) { r.RouteID = "another_route" }, ErrGenerationConflict},
		{"hostname", func(r *StoredCertificate) { r.Hostname = "other.example.test" }, ErrGenerationConflict},
		{"newer_generation", func(r *StoredCertificate) { r.DomainGeneration += 2 }, ErrGenerationConflict},
		{"fingerprint", func(r *StoredCertificate) { r.Fingerprint[0] ^= 1 }, ErrCertificateInvalid},
		{"target_kind", func(r *StoredCertificate) { r.TargetKind = TargetPreviewLease; r.PreviewID = "another_preview" }, ErrGenerationConflict},
		{"ciphertext", func(r *StoredCertificate) { r.CertificateCiphertext[20] ^= 1 }, ErrMasterKeyUnavailable},
		{"expiry", func(r *StoredCertificate) { r.ExpiresAt = now.Add(-time.Second) }, ErrIssuerRateLimited},
		{"renewal_due", func(r *StoredCertificate) { r.RenewalAt = now }, ErrIssuerRateLimited},
	} {
		t.Run(tc.name, func(t *testing.T) {
			coordinator, store, issuer, domain, row := recoveryFixture(t, bundle)
			tc.mutate(&row)
			store.all[row.ID] = row
			_, err := coordinator.Ensure(context.Background(), domain)
			if err == nil {
				t.Fatal("invalid issued leaf recovered")
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("recovery error=%v want=%v", err, tc.want)
			}
			wantCalls := 1
			if tc.want == ErrIssuerRateLimited {
				wantCalls = 2
			}
			if len(issuer.seen) != wantCalls {
				t.Fatalf("CA calls=%d want=%d", len(issuer.seen), wantCalls)
			}
		})
	}
}

func TestCoordinatorNewestTerminalRowBlocksOlderFailedLeaf(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	bundle := testBundle(t, []string{"app.example.test"}, now, 90*24*time.Hour)
	for _, state := range []State{StateRevoked, StateSuperseded} {
		t.Run(string(state), func(t *testing.T) {
			coordinator, store, issuer, domain, row := recoveryFixture(t, bundle)
			newer := row
			newer.ID = "newer_terminal"
			newer.CertificateGeneration++
			newer.State = state
			store.all[newer.ID] = newer
			_, err := coordinator.Ensure(context.Background(), domain)
			if !errors.Is(err, ErrIssuerRateLimited) || len(issuer.seen) != 2 {
				t.Fatalf("older failed row reused past newer terminal row: %v", err)
			}
		})
	}
}

func TestCoordinatorDoesNotIssueBeforeReadyPlacement(t *testing.T) {
	issuer := &testIssuer{err: ErrIssuerRateLimited}
	coordinator, _, _, _ := coordinatorForTest(t, issuer)
	domain := testDomain("app.example.test", StrategyDelegatedDNS01)
	domain.EdgeTargets = nil
	_, err := coordinator.Ensure(context.Background(), domain)
	if !errors.Is(err, ErrCertificateNotReady) || len(issuer.seen) != 0 {
		t.Fatalf("unplaced domain contacted CA: calls=%d err=%v", len(issuer.seen), err)
	}
}
