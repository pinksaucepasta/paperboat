package tunnelcert

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// F21-52: a certificate action staged on one server instance is pulled and acknowledged by the edge on another,
// and the staging instance sees the result. Before the shared store each instance kept its own pending map.
func TestDistributionActionStagedOnOneInstanceIsPulledAndAckedOnAnother(t *testing.T) {
	stagedOnOneInstanceAckedOnAnother(t, NewMemoryDistributionActionStore(), "epoch_multi_001")
}

func TestDistributionActionStagedOnOneInstanceIsPulledAndAckedOnAnotherOnPostgres(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("PAPERBOAT_TEST_DATABASE_DSN"))
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN to run the shared distribution store on PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := db.Migrate(context.Background(), database); err != nil {
		t.Fatal(err)
	}
	store, err := NewSQLDistributionActionStore(database)
	if err != nil {
		t.Fatal(err)
	}
	epoch := fmt.Sprintf("epoch_pg_%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = database.Pool().Exec(context.Background(), `DELETE FROM paperboat.tunnel_certificate_distribution_actions WHERE edge_process_epoch=$1`, epoch)
	})
	stagedOnOneInstanceAckedOnAnother(t, store, epoch)
	var plaintext bool
	if err := database.Pool().QueryRow(context.Background(), `SELECT EXISTS(SELECT 1 FROM paperboat.tunnel_certificate_distribution_actions WHERE edge_process_epoch=$1 AND (position('private-key-secret' IN message::text) > 0 OR sealed_private_key IS NOT NULL))`, epoch).Scan(&plaintext); err != nil || plaintext {
		t.Fatalf("an acknowledged action kept key material: %v %v", plaintext, err)
	}
}

func stagedOnOneInstanceAckedOnAnother(t *testing.T, store DistributionActionStore, epoch string) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	identity := DistributionNodeIdentity{NodeID: "edge_multi", ProcessEpoch: epoch}
	hub := func() *DistributionHub {
		h, err := NewDistributionHub(DistributionHubConfig{Store: store, Keys: testSecretSource{value: bytes.Repeat([]byte{7}, 32)}, KeyReference: "distribution-test",
			Credential: []byte("distribution-credential-multi-012345678901234567890123456789"),
			Now:        func() time.Time { return now },
			Identity: DistributionNodeIdentityResolverFunc(func(context.Context, *http.Request) (DistributionNodeIdentity, error) {
				return identity, nil
			}),
		})
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	first, second := hub(), hub()
	request := trk35DistributionRequest(1, now, identity)
	if _, err := first.enqueue(context.Background(), "stage", request); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(distributionPull{EdgeNodeID: identity.NodeID, EdgeProcessEpoch: identity.ProcessEpoch})
	pull := httptest.NewRequest(http.MethodPost, CertificateDistributionPullPath, strings.NewReader(string(body)))
	pull.Header.Set("Content-Type", "application/json")
	pull = pull.WithContext(withDistributionNodeIdentity(context.Background(), identity))
	recorder := httptest.NewRecorder()
	second.handlePull(recorder, pull)
	var page distributionPage
	if err := json.Unmarshal(recorder.Body.Bytes(), &page); err != nil || recorder.Code != http.StatusOK || len(page.Messages) != 1 {
		t.Fatalf("pull on the other instance = %d %+v %v", recorder.Code, page, err)
	}
	if !bytes.Equal(page.Messages[0].PrivateKeyPEM, request.Bundle.PrivateKeyPEM) {
		t.Fatal("the other instance did not deliver the staged key")
	}
	if status := trk35HandleDistributionAck(t, second, identity, trk35DistributionAck(page.Messages[0])); status != http.StatusNoContent {
		t.Fatalf("ack on the other instance = %d", status)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := first.wait(ctx, distributionKey("stage", request)); err != nil {
		t.Fatalf("the staging instance did not see the other instance's ack: %v", err)
	}
}

// F21-53: a distribution proof accepted by one server instance is a replay on every other one.
// Before, each resolver kept its own in-memory nonce map, so a replay passed on another instance.
func TestDistributionProofReplayIsRefusedOnAnotherInstance(t *testing.T) {
	replayRefusedOnAnotherInstance(t, newTestNonces())
}

func TestDistributionProofReplayIsRefusedOnAnotherInstanceOnPostgres(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("PAPERBOAT_TEST_DATABASE_DSN"))
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN to run proof nonces on PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := db.Migrate(context.Background(), database); err != nil {
		t.Fatal(err)
	}
	replayRefusedOnAnotherInstance(t, database)
}

func replayRefusedOnAnotherInstance(t *testing.T, nonces ProofNonceReserver) {
	t.Helper()
	now := time.Now().UTC()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	epoch := fmt.Sprintf("epoch_%d", now.UnixNano())
	lookup := distributionTestKeyLookup{keys: map[string]ed25519.PublicKey{"edge_01\x00" + epoch: public}}
	first, err := NewSignedDistributionNodeIdentityResolver(lookup, nonces, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	second, _ := NewSignedDistributionNodeIdentityResolver(lookup, nonces, func() time.Time { return now })
	body := []byte(`{"edge_node_id":"edge_01","edge_process_epoch":"` + epoch + `","limit":1}`)
	nonce := distributionTestNonce(9)
	if _, err := first.ResolveDistributionNode(context.Background(), signedDistributionRequest(t, CertificateDistributionPullPath, "edge_01", epoch, now.Add(-time.Second), nonce, body, private)); err != nil {
		t.Fatalf("first use refused: %v", err)
	}
	if _, err := second.ResolveDistributionNode(context.Background(), signedDistributionRequest(t, CertificateDistributionPullPath, "edge_01", epoch, now.Add(-time.Second), nonce, body, private)); err == nil {
		t.Fatal("a replayed proof was accepted by another instance")
	}
}

func distributionBudgetTestDatabase(t *testing.T) (context.Context, *db.DB) {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("PAPERBOAT_TEST_DATABASE_DSN"))
	if dsn == "" {
		t.Skip("isolated PostgreSQL required")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	if err := db.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	return ctx, database
}

func TestDistributionPendingBudgetAcrossEpochsOnPostgres(t *testing.T) {
	ctx, database := distributionBudgetTestDatabase(t)
	store, err := NewSQLDistributionActionStore(database)
	if err != nil {
		t.Fatal(err)
	}
	node := fmt.Sprintf("edge_budget_%d", time.Now().UnixNano())
	defer func() {
		_, _ = database.Pool().Exec(context.Background(), `DELETE FROM paperboat.tunnel_certificate_distribution_actions WHERE edge_node_id IN ($1,$2)`, node, node+"_other")
	}()
	makeAction := func(i int, node, epoch string) DistributionAction {
		return DistributionAction{Key: distributionStoreKey(fmt.Sprintf("%s_%d", node, i)), Message: DistributionMessage{
			Action: "stage", EdgeNodeID: node, EdgeProcessEpoch: epoch, AccountID: fmt.Sprintf("account_%d", i), ExpiresAt: time.Now().Add(time.Minute),
		}}
	}

	const budget = 256
	const ownerBudget = 64
	for i := range ownerBudget + 1 {
		action := makeAction(i, node+"_other", "epoch_old")
		action.Message.AccountID = "account_full"
		_, err := store.Put(ctx, action)
		if i < ownerBudget && err != nil || i == ownerBudget && !errors.Is(err, ErrDistributionTransportFailed) {
			t.Fatalf("owner action %d: %v", i, err)
		}
	}
	first := makeAction(0, node, "epoch_old")
	for i := range budget - 1 {
		if _, err := store.Put(ctx, makeAction(i, node, "epoch_old")); err != nil {
			t.Fatalf("action %d: %v", i, err)
		}
	}
	if _, err := store.Put(ctx, first); err != nil {
		t.Fatalf("idempotent retry at capacity: %v", err)
	}
	// Two replicas compete for the last slot with different process epochs.
	last := makeAction(budget-1, node, "epoch_old")
	next := makeAction(budget, node, "epoch_new")
	second, err := NewSQLDistributionActionStore(database)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := second.Put(ctx, next)
		result <- err
	}()
	_, lastErr := store.Put(ctx, last)
	nextErr := <-result
	switch {
	case lastErr == nil && errors.Is(nextErr, ErrDistributionTransportFailed):
	case nextErr == nil && errors.Is(lastErr, ErrDistributionTransportFailed):
		next = last
	default:
		t.Fatalf("expected one slot across epochs: old=%v, new=%v", lastErr, nextErr)
	}
	if _, err := store.Put(ctx, next); !errors.Is(err, ErrDistributionTransportFailed) {
		t.Fatalf("retry bypassed the node budget: %v", err)
	}

	if _, err := store.Put(ctx, makeAction(budget, node+"_other", "epoch_new")); err != nil {
		t.Fatalf("another node inherited the first node's budget: %v", err)
	}
	if ok, err := store.Complete(ctx, first.Key, "ready", "", time.Now()); !ok || err != nil {
		t.Fatalf("acknowledge pending action: %v", err)
	}
	if _, err := store.Put(ctx, next); err != nil {
		t.Fatalf("ack did not free a slot across epochs: %v", err)
	}
}

type testExactLeafIssuer struct{ testIssuer }

func (i *testExactLeafIssuer) IssueLeaf(ctx context.Context, request IssueRequest) (CertificateBundle, error) {
	return i.Issue(ctx, request)
}

func TestExactLeafIssuanceBudgetFollowsVerifiedDomainOnPostgres(t *testing.T) {
	ctx, database := distributionBudgetTestDatabase(t)
	admission, err := NewSQLIssuanceLock(database)
	if err != nil {
		t.Fatal(err)
	}
	issuer := &testExactLeafIssuer{testIssuer{err: ErrIssuerUnavailable}}
	coordinator, _, _, _ := coordinatorForTest(t, issuer)
	now := time.Now().UTC().Truncate(time.Minute)
	coordinator.config.Now = func() time.Time { return now }
	coordinator.config.AdmitExactLeaf = admission.AdmitExactLeaf
	coordinator.config.Delegated = nil
	domain := testDomain("*.example.test", StrategyOnDemandLeaf)
	domain.ID = fmt.Sprintf("dom_budget_%d", now.UnixNano())
	defer func() {
		_, _ = database.Pool().Exec(context.Background(), `DELETE FROM paperboat.auth_rate_limits WHERE bucket_key IN ($1,$2)`,
			"certificate-leaf:"+domain.ID, "certificate-leaf:"+domain.ID+"_other")
	}()

	for i := range 16 {
		domain.LeafHostname = fmt.Sprintf("leaf-%d.example.test", i)
		if _, err := coordinator.Ensure(ctx, domain); !errors.Is(err, ErrIssuerUnavailable) {
			t.Fatalf("issue leaf %d: %v", i, err)
		}
	}
	domain.LeafHostname = "next.example.test"
	domain.EdgeTargets = []EdgeTarget{{NodeID: "edge_new", ProcessEpoch: "epoch-new", Generation: 1}}
	if _, err := coordinator.Ensure(ctx, domain); !errors.Is(err, ErrIssuerRateLimited) || len(issuer.seen) != 16 {
		t.Fatalf("changing edge bypassed the issuance budget: issuer calls=%d, err=%v", len(issuer.seen), err)
	}
	other := domain
	other.ID += "_other"
	if _, err := coordinator.Ensure(ctx, other); !errors.Is(err, ErrIssuerUnavailable) || len(issuer.seen) != 17 {
		t.Fatalf("another domain inherited the issuance budget: issuer calls=%d, err=%v", len(issuer.seen), err)
	}
}
