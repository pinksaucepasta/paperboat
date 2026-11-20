package peersessions

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/peernetwork"
)

// After a restart the cache is empty; the warm-up builds a machine's document first, so its first refresh is a hit and
// builds nothing.
func TestWarmPoliciesFillsTheCacheForOnlineMachinesOnPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	store := openMeshTestStore(t, ctx)
	account := "warm_user_" + fmt.Sprint(time.Now().UnixNano())
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, account); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1`, account)
	})
	seedMeshMachines(t, ctx, store, account, 1, 3)
	var signed peernetwork.Configuration
	service, err := NewNetworkService(store, meshTestSigner{claims: &signed}, "https://issuer.example.test")
	if err != nil {
		t.Fatal(err)
	}
	listenCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for listenCtx.Err() == nil {
			if service.ListenPolicy(listenCtx, store.Direct(), time.Second) != nil && listenCtx.Err() == nil {
				time.Sleep(50 * time.Millisecond)
			}
		}
	}()
	t.Cleanup(func() { stop(); <-done })

	// WarmPolicies lists every online machine of the shared test database in id order, far more than these three; the
	// per-machine step it runs for each is what the test drives, once the LISTEN session is attached.
	for {
		if _, listening := service.cache.begin(); listening {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("the policy LISTEN session did not attach")
		case <-time.After(20 * time.Millisecond):
		}
	}
	for i := 1; i <= 3; i++ {
		if !service.warmPolicy(ctx, account, fmt.Sprintf("%s_m_%d", account, i)) {
			t.Fatalf("machine %d was not warmed", i)
		}
	}
	hits, builds := service.cache.hits, service.cache.builds
	for i := 1; i <= 3; i++ {
		request := NetworkConfigRequest{OperationID: fmt.Sprintf("warm_config_%d_%d", i, time.Now().UnixNano()), UserID: account, EndpointID: fmt.Sprintf("%s_m_%d", account, i), Authority: AuthorityUntilRevoked}
		if _, err := service.Configuration(ctx, request); err != nil {
			t.Fatal(err)
		}
	}
	if gotHits, gotBuilds := service.cache.hits-hits, service.cache.builds-builds; gotHits != 3 || gotBuilds != 0 {
		t.Fatalf("first refreshes after the warm-up: %d hits and %d builds, want 3 and 0", gotHits, gotBuilds)
	}
}

// Without a LISTEN session nothing is stored, so the warm-up waits for one and gives up when its context ends
// rather than reporting documents it could not keep.
func TestWarmPoliciesWaitsForTheListenSessionAndHonorsItsContext(t *testing.T) {
	service := &NetworkService{cache: newPolicyCache()}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if warmed, err := service.WarmPolicies(ctx, 10, 2); warmed != 0 || err == nil {
		t.Fatalf("warm-up without a LISTEN session = %d, %v; want 0 and the context's error", warmed, err)
	}
}
