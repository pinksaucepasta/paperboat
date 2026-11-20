package controlplane

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
)

// Many machines enrolling at once (a fleet rollout, a lane's test containers) exchange their enrollment credentials
// together. The exchange touches rows no two enrollments share (its own enrollment, helper, machine and connector rows)
// and one audit insert, so it needs no SERIALIZABLE isolation: under it the predicate locks of index pages the
// enrollments merely sit on made a burst lose its retries and answer 503 authorization_unavailable
// (postgres_serialization, build 2026.11.20.912). The exchange runs at READ COMMITTED: every enrollment succeeds and
// none runs a serializable transaction.
func TestHelperEnrollmentsExchangeAtOnceWithoutSerializableConflicts(t *testing.T) {
	store := openControlPlaneTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	scope := uniqueScope(t)
	user := "usr_burst_" + scope
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$1,$1||'@example.test','active')`, user); err != nil {
		t.Fatal(err)
	}
	signer, err := mint.New([]mint.Key{{ID: "burst-test", PrivateKey: enrollmentTestPrivateKey("burst-" + scope)}}, "burst-test", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	service := NewEnrollmentService(store, signer, audit.NewWriter(store), "https://api.example.test", "enrollment-test-encryption-key")
	service.clock = func() time.Time { return now }

	const enrollments = 60
	grants := make([]EnrollmentGrant, enrollments)
	keys := make([]ed25519.PublicKey, enrollments)
	for i := range grants {
		environment := fmt.Sprintf("env_burst_%s_%02d", scope, i)
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_environments (id,workspace_id,owner_user_id) VALUES ($1,$1,$2)`, environment, user); err != nil {
			t.Fatal(err)
		}
		seedEnrollmentMachine(t, store, user, environment)
		if grants[i], err = service.Issue(ctx, user, "issue:"+environment, environment, 5*time.Minute); err != nil {
			t.Fatal(err)
		}
		keys[i] = enrollmentTestPrivateKey(environment).Public().(ed25519.PublicKey)
	}
	before := store.TransactionStats()
	var wg sync.WaitGroup
	failures := make(chan error, enrollments)
	for i := range grants {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := service.Exchange(ctx, grants[i].Credential, keys[i]); err != nil {
				failures <- fmt.Errorf("enrollment %d: %w", i, err)
			}
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	after := store.TransactionStats()
	if calls := after.Serializable.Calls - before.Serializable.Calls; calls != 0 {
		t.Fatalf("%d exchanges ran serializable transactions (%d retries, %d exhausted); they must not",
			calls, after.Serializable.Retries-before.Serializable.Retries, after.Serializable.Exhausted-before.Serializable.Exhausted)
	}
}
