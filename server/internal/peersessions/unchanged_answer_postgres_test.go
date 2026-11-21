package peersessions

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/peernetwork"
)

type countingSigner struct {
	meshTestSigner
	n *atomic.Int64
}

func (c countingSigner) SignNetworkConfiguration(claims peernetwork.Configuration) (string, error) {
	c.n.Add(1)
	return c.meshTestSigner.SignNetworkConfiguration(claims)
}

// An account-wide invalidation woke every endpoint of a 200-machine account, and each renewal bumped its generation
// and signed again although nothing it holds changed: N bumps and N signatures per account change. A renewal whose
// answer matches the configuration it holds is now answered unchanged, without either; a real change (one machine
// publishes endpoints, every peer names it) still bumps and signs for the endpoints it changes.
func TestAccountWideInvalidationSignsOnlyChangedEndpointsOnPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	store := openMeshTestStore(t, ctx)
	s := fmt.Sprint(time.Now().UnixNano())
	account := "unchanged_user_" + s
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, account); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1`, account)
	})
	const machines = 200
	seedMeshMachines(t, ctx, store, account, 0, machines-1)
	var signs atomic.Int64
	service, err := NewNetworkService(store, countingSigner{n: &signs}, "https://issuer.example.test")
	if err != nil {
		t.Fatal(err)
	}
	usePolicyCache(t, service, store)
	held := map[string]uint64{}
	generations := func() int64 {
		var n int64
		if err := store.SQL().QueryRowContext(ctx, `SELECT coalesce(sum(config_generation),0) FROM paperboat.peer_network_identities WHERE user_id=$1`, account).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	round := func(name string, base bool) (signed, bumped, unchanged int64) {
		t.Helper()
		s0, g0 := signs.Load(), generations()
		for i := range machines {
			id := fmt.Sprintf("%s_m_%d", account, i)
			in := NetworkConfigRequest{OperationID: fmt.Sprintf("unch_%s_%s_%d", s, name, i), UserID: account, EndpointID: id, Authority: AuthorityUntilRevoked}
			if base {
				in.BaseGeneration = held[id]
			}
			var trace configTrace
			r, err := service.configuration(ctx, in, &trace)
			if err != nil {
				t.Fatalf("%s %d: %v", name, i, err)
			}
			switch {
			case r.Unchanged:
				unchanged++
			case r.ConfigurationDelta != nil:
				held[id] = r.ConfigurationDelta.Configuration.Generation
			default:
				held[id] = peernetwork.ConfigurationGeneration(r.Configuration)
			}
		}
		signed, bumped = signs.Load()-s0, generations()-g0
		t.Logf("RESULT %s, %d endpoints: %d signatures, %d generation bumps, %d unchanged", name, machines, signed, bumped, unchanged)
		return
	}
	round("first", false)
	if signed, bumped, _ := round("invalidation without base (before)", false); signed != machines || bumped != machines {
		t.Fatalf("a full renewal signed %d and bumped %d, want %d each", signed, bumped, machines)
	}
	service.cache.reset(true)
	if signed, bumped, unchanged := round("invalidation with base (after)", true); signed != 0 || bumped != 0 || unchanged != machines {
		t.Fatalf("an invalidation that changed nothing signed %d, bumped %d, answered %d unchanged", signed, bumped, unchanged)
	}
	// One machine publishes its endpoints: every other endpoint's configuration names them, so each signs once.
	var trace configTrace
	published, err := service.configuration(ctx, NetworkConfigRequest{OperationID: "unch_publish_" + s, UserID: account, EndpointID: account + "_m_0",
		Endpoints: []string{"198.51.100.7:41641"}, Authority: AuthorityUntilRevoked}, &trace)
	if err != nil {
		t.Fatal(err)
	}
	held[account+"_m_0"] = peernetwork.ConfigurationGeneration(published.Configuration)
	signed, bumped, _ := round("one real change", true)
	if signed > machines || bumped > machines {
		t.Fatalf("one change signed %d and bumped %d for %d endpoints", signed, bumped, machines)
	}
	if again, bumpedAgain, unchanged := round("repeat after the change", true); again != 0 || bumpedAgain != 0 || unchanged != machines {
		t.Fatalf("a repeat signed %d, bumped %d, answered %d unchanged", again, bumpedAgain, unchanged)
	}
}
