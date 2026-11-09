package tunnelcert

import (
	"context"
	"errors"
	"testing"
	"time"
)

type workspaceSource struct {
	bundle CertificateBundle
	err    error
	calls  int
}

func (s *workspaceSource) WorkspaceCertificate(context.Context, string, string) (CertificateBundle, error) {
	s.calls++
	if s.err != nil {
		return CertificateBundle{}, s.err
	}
	return cloneCertificateBundle(s.bundle), nil
}

// A hostname under a workspace domain stores and distributes the domain's wildcard certificate instead of
// ordering its own; a renewal waits until the domain holds a newer certificate.
func TestCoordinatorHostnameUnderWorkspaceDomainReusesItsCertificate(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	issuer := &testIssuer{bundle: testBundle(t, []string{"app.example.test"}, now, 60*24*time.Hour)}
	coordinator, store, distributor, _ := coordinatorForTest(t, issuer)
	source := &workspaceSource{err: ErrWorkspaceCertificatePending}
	coordinator.config.WorkspaceCertificates = source
	domain := testDomain("app.example.test", StrategyDelegatedDNS01)
	domain.WorkspaceDomainID = "wsd_0123456789ab"

	if _, err := coordinator.Ensure(context.Background(), domain); !errors.Is(err, ErrWorkspaceCertificatePending) || !errors.Is(err, ErrCertificateNotReady) {
		t.Fatalf("before the domain's certificate exists: %v", err)
	}
	wildcard := testBundle(t, []string{"*.example.test", "example.test"}, now, 60*24*time.Hour)
	source.bundle, source.err = wildcard, nil
	first, err := coordinator.Ensure(context.Background(), domain)
	if err != nil {
		t.Fatal(err)
	}
	if len(issuer.seen) != 0 || source.calls != 2 || first.Certificate.State != StateActive || len(distributor.Events) == 0 {
		t.Fatalf("issuer calls=%d source calls=%d result=%+v events=%v", len(issuer.seen), source.calls, first, distributor.Events)
	}
	stored, ok, err := store.Current(context.Background(), domain.ID)
	if err != nil || !ok || stored.Hostname != "app.example.test" {
		t.Fatalf("stored = %+v %v %v", stored, ok, err)
	}

	// Renewal due, but the domain still holds the same certificate: wait instead of re-staging it.
	domain.CertificateReference, domain.RenewalDue = first.Certificate.Reference, true
	if _, err := coordinator.Ensure(context.Background(), domain); !errors.Is(err, ErrWorkspaceCertificatePending) {
		t.Fatalf("renewal without a newer domain certificate: %v", err)
	}
	source.bundle = testBundle(t, []string{"*.example.test", "example.test"}, now, 80*24*time.Hour)
	second, err := coordinator.Ensure(context.Background(), domain)
	if err != nil {
		t.Fatal(err)
	}
	if second.Certificate.CertificateGeneration != first.Certificate.CertificateGeneration+1 || len(issuer.seen) != 0 {
		t.Fatalf("renewal = %+v issuer calls=%d", second, len(issuer.seen))
	}
}

func TestWorkspaceChallengeTargetDependsOnWorkspaceAndHostnameOnly(t *testing.T) {
	a, err := WorkspaceChallengeTarget("account:usr_1", "dev.example.com", "challenges.example.test")
	if err != nil {
		t.Fatal(err)
	}
	again, _ := WorkspaceChallengeTarget("account:usr_1", "dev.example.com", "challenges.example.test")
	other, _ := WorkspaceChallengeTarget("team:acme", "dev.example.com", "challenges.example.test")
	if a != again || a == other || len(a) != len("pb-")+32+len(".challenges.example.test") {
		t.Fatalf("targets %q %q %q", a, again, other)
	}
	viaDomain, err := DelegatedChallengeTargetForDomain(Domain{ID: "wsd_x", AccountID: "usr_1", TargetKind: TargetWorkspaceDomain, Hostname: "*.dev.example.com", ChallengeReference: "account:usr_1"}, "challenges.example.test")
	if err != nil || viaDomain != a {
		t.Fatalf("issuance target %q (%v), want the shown record %q", viaDomain, err, a)
	}
}
