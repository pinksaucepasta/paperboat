package tunnelcert

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestCoordinatorRedistributesActiveCertificateAfterEdgeProcessReplacement
// covers the active-certificate fast path. A changed process epoch is a new
// distribution target, but it must reuse the active certificate material and
// retire the old process only after the replacement is active.
func TestCoordinatorRedistributesActiveCertificateAfterEdgeProcessReplacement(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	issuer := &testIssuer{bundle: testBundle(t, []string{"*.edge.example.test"}, now, 60*24*time.Hour)}
	coordinator, store, _, _ := coordinatorForTest(t, issuer)
	distributor := newDistributionLedger()
	coordinator.config.Distributor = distributor

	domain := Domain{
		ID: "platform_cert_preview_v1", AccountID: PlatformAccountID,
		TargetKind: TargetPlatformWildcard, Hostname: "*.edge.example.test",
		Generation: 1, Strategy: StrategyPlatformDNS01,
		CAAState: "not_applicable",
		EdgeTargets: []EdgeTarget{
			{NodeID: "edge_a", ProcessEpoch: "epoch-old-a", Generation: 1},
			{NodeID: "edge_b", ProcessEpoch: "epoch-stable-b", Generation: 1},
		},
	}
	first, err := coordinator.Ensure(context.Background(), domain)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Issued || first.Certificate.CertificateGeneration != 1 {
		t.Fatalf("initial result = %+v", first)
	}
	certificateID := first.Certificate.Reference[len("tcert_"):]

	domain.CertificateReference = first.Certificate.Reference
	domain.EdgeTargets[0].ProcessEpoch = "epoch-new-a"
	second, err := coordinator.Ensure(context.Background(), domain)
	if err != nil {
		t.Fatal(err)
	}
	if second.Issued || second.Certificate.CertificateGeneration != first.Certificate.CertificateGeneration {
		t.Fatalf("replacement result = %+v, want active generation %d without issuance", second, first.Certificate.CertificateGeneration)
	}
	if len(issuer.seen) != 1 {
		t.Fatalf("replacement called issuer %d times, want 1", len(issuer.seen))
	}

	if got, want := distributor.events(), []string{
		"stage:edge_a/epoch-old-a/1", "stage:edge_b/epoch-stable-b/1",
		"ready:edge_a/epoch-old-a/1", "ready:edge_b/epoch-stable-b/1",
		"activate:edge_a/epoch-old-a/1", "activate:edge_b/epoch-stable-b/1",
		"stage:edge_a/epoch-new-a/1", "ready:edge_a/epoch-new-a/1",
		"activate:edge_a/epoch-new-a/1", "retire:edge_a/epoch-old-a/1",
	}; joinEvents(got) != joinEvents(want) {
		t.Fatalf("distribution events = %v, want %v", got, want)
	}

	if got := distributor.state(certificateID, DistributionTarget{NodeID: "edge_a", ProcessEpoch: "epoch-new-a", Generation: 1}); got != "active" {
		t.Fatalf("replacement edge state = %q, want active", got)
	}
	if got := distributor.state(certificateID, DistributionTarget{NodeID: "edge_a", ProcessEpoch: "epoch-old-a", Generation: 1}); got != "retired" {
		t.Fatalf("old edge state = %q, want retired", got)
	}
	if got := distributor.state(certificateID, DistributionTarget{NodeID: "edge_b", ProcessEpoch: "epoch-stable-b", Generation: 1}); got != "active" {
		t.Fatalf("stable edge state = %q, want active", got)
	}
	current, found, err := store.Current(context.Background(), domain.ID)
	if err != nil || !found || current.CertificateGeneration != first.Certificate.CertificateGeneration || current.State != StateActive {
		t.Fatalf("stored replacement = %+v found=%v err=%v", current, found, err)
	}
}

func TestCoordinatorRetriesPartialActiveCertificateRedistribution(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	issuer := &testIssuer{bundle: testBundle(t, []string{"*.edge.example.test"}, now, 60*24*time.Hour)}
	coordinator, store, _, _ := coordinatorForTest(t, issuer)
	distributor := newDistributionLedger()
	coordinator.config.Distributor = distributor
	domain := Domain{ID: "platform_cert_runtime_v1", AccountID: PlatformAccountID, TargetKind: TargetPlatformWildcard,
		Hostname: "*.edge.example.test", Generation: 1, Strategy: StrategyPlatformDNS01,
		CAAState:    "not_applicable",
		EdgeTargets: []EdgeTarget{{NodeID: "edge_a", ProcessEpoch: "epoch-old-a", Generation: 1}, {NodeID: "edge_b", ProcessEpoch: "epoch-old-b", Generation: 1}}}
	first, err := coordinator.Ensure(context.Background(), domain)
	if err != nil {
		t.Fatal(err)
	}
	domain.CertificateReference = first.Certificate.Reference
	domain.EdgeTargets[0].ProcessEpoch = "epoch-new-a"
	domain.EdgeTargets[1].ProcessEpoch = "epoch-new-b"
	distributor.failActivateOnce = "edge_b/epoch-new-b/1"
	if _, err := coordinator.Ensure(context.Background(), domain); err == nil {
		t.Fatal("wanted partial activation failure")
	}
	certificateID := first.Certificate.Reference[len("tcert_"):]
	if got := distributor.state(certificateID, DistributionTarget(domain.EdgeTargets[0])); got != "active" {
		t.Fatalf("first replacement = %q, want active", got)
	}
	for _, event := range distributor.events() {
		if event == "retire:edge_a/epoch-new-a/1" || event == "retire:edge_b/epoch-new-b/1" {
			t.Fatalf("active certificate replacement was rolled back: %v", distributor.events())
		}
	}
	if _, err := coordinator.Ensure(context.Background(), domain); err != nil {
		t.Fatal(err)
	}
	for _, edge := range domain.EdgeTargets {
		if got := distributor.state(certificateID, DistributionTarget(edge)); got != "active" {
			t.Fatalf("%s replacement = %q, want active", edge.NodeID, got)
		}
	}
	current, found, err := store.Current(context.Background(), domain.ID)
	if err != nil || !found || current.State != StateActive || current.CertificateGeneration != 1 {
		t.Fatalf("authority changed: %+v found=%v err=%v", current, found, err)
	}
}

type distributionLedger struct {
	mu               sync.Mutex
	rows             map[string]string
	eventLog         []string
	failActivateOnce string
	stragglerNode    string // WaitReady for this edge node never answers: it blocks until its context ends
}

func newDistributionLedger() *distributionLedger {
	return &distributionLedger{rows: make(map[string]string)}
}

func (d *distributionLedger) Stage(_ context.Context, request DistributionRequest) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	key := distributionLedgerKey(request.Certificate.ID, request.Target)
	d.rows[key] = "staged"
	d.eventLog = append(d.eventLog, "stage:"+distributionTargetDescription(request.Target))
	return nil
}

func (d *distributionLedger) WaitReady(ctx context.Context, request DistributionRequest) error {
	d.mu.Lock()
	straggler := d.stragglerNode == request.Target.NodeID
	d.mu.Unlock()
	if straggler {
		<-ctx.Done()
		return ctx.Err()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	key := distributionLedgerKey(request.Certificate.ID, request.Target)
	if d.rows[key] != "staged" {
		return ErrCertificateNotReady
	}
	d.rows[key] = "ready"
	d.eventLog = append(d.eventLog, "ready:"+distributionTargetDescription(request.Target))
	return nil
}

func (d *distributionLedger) Activate(_ context.Context, request DistributionRequest) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failActivateOnce == distributionTargetDescription(request.Target) {
		d.failActivateOnce = ""
		return ErrDistributionUnavailable
	}
	key := distributionLedgerKey(request.Certificate.ID, request.Target)
	if d.rows[key] != "ready" {
		return ErrCertificateNotReady
	}
	d.rows[key] = "active"
	d.eventLog = append(d.eventLog, "activate:"+distributionTargetDescription(request.Target))
	return nil
}

func (d *distributionLedger) Retire(_ context.Context, certificate StoredCertificate, target DistributionTarget) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	key := distributionLedgerKey(certificate.ID, target)
	if d.rows[key] == "" {
		return ErrGenerationConflict
	}
	d.rows[key] = "retired"
	d.eventLog = append(d.eventLog, "retire:"+distributionTargetDescription(target))
	return nil
}

func (d *distributionLedger) MissingCertificateTargets(_ context.Context, certificate StoredCertificate, targets []DistributionTarget) ([]DistributionTarget, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	missing := make([]DistributionTarget, 0, len(targets))
	for _, target := range targets {
		if err := target.Validate(); err != nil {
			return nil, err
		}
		if d.rows[distributionLedgerKey(certificate.ID, target)] != "active" {
			missing = append(missing, target)
		}
	}
	return missing, nil
}

func (d *distributionLedger) RetireObsoleteCertificateTargets(_ context.Context, certificate StoredCertificate, current []DistributionTarget) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	keep := make(map[string]struct{}, len(current))
	for _, target := range current {
		keep[distributionLedgerKey(certificate.ID, target)] = struct{}{}
	}
	for key, state := range d.rows {
		if state != "staged" && state != "ready" && state != "active" {
			continue
		}
		if _, ok := keep[key]; ok {
			continue
		}
		d.rows[key] = "retired"
		separator := len(certificate.ID) + 1
		if len(key) > separator {
			d.eventLog = append(d.eventLog, "retire:"+key[separator:])
		}
	}
	return nil
}

func (d *distributionLedger) state(certificateID string, target DistributionTarget) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.rows[distributionLedgerKey(certificateID, target)]
}

func (d *distributionLedger) events() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.eventLog...)
}

func distributionLedgerKey(certificateID string, target DistributionTarget) string {
	return certificateID + "\x00" + distributionTargetDescription(target)
}

func distributionTargetDescription(target DistributionTarget) string {
	return fmt.Sprintf("%s/%s/%d", target.NodeID, target.ProcessEpoch, target.Generation)
}

func TestCoordinatorRedistributionRecoveryPreservesExplicitRenewal(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	issuer := &testIssuer{bundle: testBundle(t, []string{"app.example.test"}, now, 60*24*time.Hour)}
	coordinator, store, _, _ := coordinatorForTest(t, issuer)
	ledger := newDistributionLedger()
	coordinator.config.Distributor = ledger
	domain := testDomain("app.example.test", StrategyDelegatedDNS01)
	domain.RouteID = "route_001"
	first, err := coordinator.Ensure(context.Background(), domain)
	if err != nil {
		t.Fatal(err)
	}
	domain.CertificateReference = first.Certificate.Reference
	domain.Generation++
	domain.EdgeTargets[0].ProcessEpoch = "epoch-replaced"
	ledger.failActivateOnce = distributionTargetDescription(DistributionTarget(domain.EdgeTargets[0]))
	issuer.err = ErrIssuerRateLimited
	if _, err = coordinator.Ensure(context.Background(), domain); !errors.Is(err, ErrDistributionUnavailable) {
		t.Fatalf("distribution failure = %v", err)
	}
	domain.Generation++
	result, err := coordinator.Ensure(context.Background(), domain)
	if err != nil || result.Issued || len(issuer.seen) != 1 || result.Certificate.Reference != first.Certificate.Reference {
		t.Fatalf("recovery: issued=%v calls=%d err=%v", result.Issued, len(issuer.seen), err)
	}
	current, found, err := store.Current(context.Background(), domain.ID)
	if err != nil || !found || current.CertificateGeneration != 1 {
		t.Fatal("active certificate changed")
	}
	// Explicit coordinator renewal remains authoritative even when the
	// certificate has plenty of remaining lifetime.
	domain.RenewalDue = true
	if _, err = coordinator.Ensure(context.Background(), domain); !errors.Is(err, ErrIssuerRateLimited) || len(issuer.seen) != 2 {
		t.Fatalf("explicit renewal calls=%d err=%v", len(issuer.seen), err)
	}
}

type domainRecoveryStore struct {
	*MemoryStore
	calls int
}

func (s *domainRecoveryStore) RecoverDomainCertificateReady(_ context.Context, _ Domain, _ StoredCertificate, _ time.Time) error {
	s.calls++
	return ErrGenerationConflict
}

func TestCoordinatorProjectionRecoveryRejectsUnreadyAndPropagatesFence(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	issuer := &testIssuer{bundle: testBundle(t, []string{"app.example.test"}, now, 60*24*time.Hour)}
	coordinator, store, _, _ := coordinatorForTest(t, issuer)
	domain := testDomain("app.example.test", StrategyDelegatedDNS01)
	domain.RouteID = "route_001"
	result, err := coordinator.Ensure(context.Background(), domain)
	if err != nil {
		t.Fatal(err)
	}
	domain.CertificateReference = result.Certificate.Reference
	domain.Generation++
	certificate, _, _ := store.Current(context.Background(), domain.ID)
	recovery := &domainRecoveryStore{MemoryStore: store}
	coordinator.config.Store = recovery
	for _, mutate := range []func(*Domain){
		func(d *Domain) { d.EdgeTargets = nil },
		func(d *Domain) { d.CertificateReference = "different" },
		func(d *Domain) { d.AccountID = "other_account" },
		func(d *Domain) { d.RouteID = "other_route" },
		func(d *Domain) { d.RenewalDue = true },
	} {
		candidate := domain
		mutate(&candidate)
		if err := coordinator.RecoverDomainCertificateReady(context.Background(), candidate, certificate, now); !errors.Is(err, ErrCertificateNotReady) {
			t.Fatalf("unsafe recovery reached store: %v", err)
		}
	}
	if recovery.calls != 0 {
		t.Fatal("unsafe recovery called store")
	}
	if err := coordinator.RecoverDomainCertificateReady(context.Background(), domain, certificate, now); !errors.Is(err, ErrGenerationConflict) || recovery.calls != 1 {
		t.Fatalf("store fence calls=%d err=%v", recovery.calls, err)
	}
}

// A durable domain's first certificate is all or nothing: its commit ends `pb tunnel domain add --wait`, so one
// edge that does not answer fails the issuance (both copies retired) instead of leaving that edge without it.
// A renewal stays partial: one edge that does not answer must not stop the renewed certificate reaching the edges
// that do (a dead edge would otherwise let it expire); the straggler is left staged and catches up on the next pass.
func TestCoordinatorActivatesReadyEdgesAroundAStraggler(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	issuer := &testIssuer{bundle: testBundle(t, []string{"app.example.test"}, now, 60*24*time.Hour)}
	coordinator, store, _, _ := coordinatorForTest(t, issuer)
	coordinator.config.DistributionTimeout = 200 * time.Millisecond
	ledger := newDistributionLedger()
	ledger.stragglerNode = "edge_b"
	coordinator.config.Distributor = ledger
	domain := testDomain("app.example.test", StrategyDelegatedDNS01)
	domain.RouteID = "route_001"
	if _, err := coordinator.Ensure(context.Background(), domain); err == nil {
		t.Fatal("first issuance with one straggler edge succeeded, want it failed until every edge has it")
	}
	if current, found, _ := store.Current(context.Background(), domain.ID); found && current.State == StateActive {
		t.Fatalf("first issuance with one straggler edge activated %+v", current)
	}
	ledger.stragglerNode = ""
	initial, err := coordinator.Ensure(context.Background(), domain)
	if err != nil || initial.Certificate.State != StateActive {
		t.Fatalf("first issuance with every edge ready = %+v, %v", initial.Certificate, err)
	}
	ledger.stragglerNode = "edge_b"
	domain.CertificateReference = initial.Certificate.Reference
	domain.RenewalDue = true
	first, err := coordinator.Ensure(context.Background(), domain)
	if err != nil {
		t.Fatalf("renewal with one straggler edge = %v, want it activated on the ready edge", err)
	}
	if first.Certificate.State != StateActive || first.Certificate.Reference == initial.Certificate.Reference {
		t.Fatalf("certificate = %+v, want a renewed active one", first.Certificate)
	}
	certificateID := first.Certificate.Reference[len("tcert_"):]
	byNode := map[string]string{}
	for _, edge := range domain.EdgeTargets {
		byNode[edge.NodeID] = ledger.state(certificateID, DistributionTarget(edge))
	}
	if byNode["edge_a"] != "active" || byNode["edge_b"] != "staged" {
		t.Fatalf("edge states = %v, want edge_a active and edge_b still staged", byNode)
	}
	// The straggler recovers: the next pass completes it and never reissues.
	issued := len(issuer.seen)
	ledger.stragglerNode = ""
	domain.CertificateReference = first.Certificate.Reference
	domain.RenewalDue = false
	if _, err := coordinator.Ensure(context.Background(), domain); err != nil {
		t.Fatal(err)
	}
	if got := ledger.state(certificateID, DistributionTarget(domain.EdgeTargets[0])); got != "active" {
		t.Fatalf("straggler after catching up = %q, want active", got)
	}
	current, found, err := store.Current(context.Background(), domain.ID)
	if err != nil || !found || current.CertificateGeneration != first.Certificate.CertificateGeneration || len(issuer.seen) != issued {
		t.Fatalf("catch-up reissued: current=%+v found=%v err=%v issued=%d", current, found, err, len(issuer.seen)-issued)
	}
}

// A first certificate activated but never recorded on the domain (the worker crashed, or its commit conflicted, between
// Activate and CommitDomainCertificateReady) must not leave the domain without one, and must never order from the CA
// again: the next pass adopts it, and after domain generation bumps it is re-staged from its own bundle. The same holds
// for a renewal whose commit was lost (the domain still records the superseded certificate).
func TestCoordinatorRecoversFirstCertificateNeverRecordedOnTheDomain(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	issuer := &testIssuer{bundle: testBundle(t, []string{"app.example.test"}, now, 60*24*time.Hour)}
	coordinator, store, _, _ := coordinatorForTest(t, issuer)
	coordinator.config.Distributor = newDistributionLedger()
	domain := testDomain("app.example.test", StrategyDelegatedDNS01)
	domain.RouteID = "route_001"
	orphan, err := coordinator.Ensure(context.Background(), domain)
	if err != nil || orphan.Certificate.State != StateActive || len(issuer.seen) != 1 {
		t.Fatalf("first issuance = %+v, %v, issuer calls %d", orphan.Certificate, err, len(issuer.seen))
	}
	// Crash here: the domain row still has no certificate reference. Meanwhile a third edge joined the route: the
	// adopted certificate must reach it too.
	ledger := coordinator.config.Distributor.(*distributionLedger)
	joined := EdgeTarget{NodeID: "edge_c", ProcessEpoch: "epoch-0003", Generation: 3}
	domain.EdgeTargets = append(domain.EdgeTargets, joined)
	adopted, err := coordinator.Ensure(context.Background(), domain)
	if err != nil || !adopted.Issued || adopted.Certificate.Reference != orphan.Certificate.Reference || len(issuer.seen) != 1 {
		t.Fatalf("next pass = %+v, %v, issuer calls %d; want the unrecorded certificate adopted as issued, no new order", adopted, err, len(issuer.seen))
	}
	if got := ledger.state(orphan.Certificate.Reference[len("tcert_"):], DistributionTarget(joined)); got != "active" {
		t.Fatalf("adopted certificate on the edge that joined = %q, want active", got)
	}
	// Its commit kept losing and each failure mark bumped the domain generation: every bump re-stages the same
	// certificate at the new generation, never a new CA order; a retry at one generation adopts again.
	var reissued Result
	for bump := 0; bump < 3; bump++ {
		domain.Generation++
		reissued, err = coordinator.Ensure(context.Background(), domain)
		if err != nil || !reissued.Issued || reissued.Certificate.Reference == orphan.Certificate.Reference || reissued.Certificate.Fingerprint != orphan.Certificate.Fingerprint || len(issuer.seen) != 1 {
			t.Fatalf("after generation bump %d = %+v, %v, issuer calls %d; want the same certificate re-staged, no order", bump+1, reissued, err, len(issuer.seen))
		}
	}
	if again, err := coordinator.Ensure(context.Background(), domain); err != nil || again.Certificate.Reference != reissued.Certificate.Reference || len(issuer.seen) != 1 {
		t.Fatalf("retry at the new generation = %+v, %v, issuer calls %d", again, err, len(issuer.seen))
	}
	current, found, err := store.Current(context.Background(), domain.ID)
	if err != nil || !found || current.State != StateActive || current.CertificateReference != reissued.Certificate.Reference {
		t.Fatalf("current = %+v found=%v err=%v", current, found, err)
	}
	// A renewal whose commit was lost: the domain still records the certificate it superseded. The active one is
	// adopted for the worker to record, not refused until the recorded one expires, and not ordered again.
	domain.CertificateReference = orphan.Certificate.Reference
	recorded, err := coordinator.Ensure(context.Background(), domain)
	if err != nil || !recorded.Issued || recorded.Certificate.Reference != reissued.Certificate.Reference || len(issuer.seen) != 1 {
		t.Fatalf("domain recording a superseded certificate = %+v, %v, issuer calls %d; want the active one adopted", recorded, err, len(issuer.seen))
	}
}

// Every edge failing to answer is still a failed issuance: nothing is activated and the staged copies are rolled back.
func TestCoordinatorFailsWhenNoEdgeIsReady(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	issuer := &testIssuer{bundle: testBundle(t, []string{"app.example.test"}, now, 60*24*time.Hour)}
	coordinator, _, _, _ := coordinatorForTest(t, issuer)
	coordinator.config.DistributionTimeout = 100 * time.Millisecond
	ledger := newDistributionLedger()
	coordinator.config.Distributor = ledger
	domain := testDomain("app.example.test", StrategyDelegatedDNS01)
	domain.EdgeTargets = domain.EdgeTargets[:1]
	ledger.stragglerNode = domain.EdgeTargets[0].NodeID
	if _, err := coordinator.Ensure(context.Background(), domain); !errors.Is(err, ErrCertificateNotReady) {
		t.Fatalf("issuance with no ready edge = %v, want ErrCertificateNotReady", err)
	}
}
