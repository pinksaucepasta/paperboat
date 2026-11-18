package tunnelv1

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var addressAttempts atomic.Uint64

func (f *trk07PostgresFixture) changeAddress(t *testing.T, accountID, tunnelID, label string, generation int64) (MutationRecord, error) {
	t.Helper()
	n := fmt.Sprint(addressAttempts.Add(1)) // a fresh key per call: a repeated key is an idempotent replay, not a second change
	hash := sha256.Sum256([]byte("address:" + tunnelID + label + n))
	return f.repository.Patch(context.Background(), PatchRecord{
		OperationID: "op_addr_" + tunnelID + label + n, AuditEventID: "aud_addr_" + tunnelID + label + n, TunnelID: tunnelID, AccountID: accountID,
		Subdomain: &label, AddressPolicy: AddressPolicy{MoveTimeout: 5 * time.Minute, Cooldown: time.Hour}, ExpectedGeneration: generation, IdempotencyKey: "address:" + tunnelID + label + n, RequestHash: hash,
		ActorID: accountID, AuditActorID: accountID, ActorType: "user", RequestID: "req_addr_" + label,
		CorrelationID: "corr_addr_" + label, SourceMachineID: "", Now: f.now,
	})
}

// A tunnel's address moves atomically: reservation, endpoint and managed route follow the new label, the
// generation advances, another account cannot take a held name, the old name is free at once, and an
// operator-blocked address cannot be renamed away.
func TestSQLRepositoryChangesTunnelAddressOnPostgres(t *testing.T) {
	f := newTRK07PostgresFixture(t)
	ctx := context.Background()
	account := f.accounts[0]
	host := "mch_trk07_primary_" + f.suffix
	otherAccount, otherHost := f.addAccount(t, "other")
	tunnelID, otherID := "tun_addr_a_"+f.suffix, "tun_addr_b_"+f.suffix
	tail := strings.ReplaceAll(f.suffix, "_", "")
	tail = tail[len(tail)-6:]
	created := f.createTunnel(t, account, host, tunnelID, "addr-a-"+f.suffix)
	other := f.createTunnel(t, otherAccount, otherHost, otherID, "addr-b-"+f.suffix)
	label := "hello" + tail
	oldHost := created.Tunnel.StableEndpoint[len("https://"):]

	moved, err := f.changeAddress(t, account, tunnelID, label, created.Tunnel.Generation)
	if err != nil {
		t.Fatal(err)
	}
	want := "https://" + label + ".tunnels.example.test"
	if moved.Tunnel.StableEndpoint != want || moved.Tunnel.Generation != created.Tunnel.Generation+1 || !moved.Changed {
		t.Fatalf("moved tunnel = %q generation %d changed %v, want %q", moved.Tunnel.StableEndpoint, moved.Tunnel.Generation, moved.Changed, want)
	}
	var reserved, oldReserved, routes, oldRoutes int
	var snapshot string
	if err := f.database.SQL().QueryRowContext(ctx, `SELECT (SELECT count(*) FROM paperboat.managed_hostnames WHERE hostname=$2 AND resource_id=$1),
 (SELECT count(*) FROM paperboat.managed_hostnames WHERE hostname=$3),
 (SELECT count(*) FROM paperboat.tunnel_routes WHERE tunnel_id=$1 AND match_hostname=$2 AND generation=2),
 (SELECT count(*) FROM paperboat.tunnel_routes WHERE tunnel_id=$1 AND match_hostname=$3),
 (SELECT convert_from(snapshot,'UTF8') FROM paperboat.tunnel_config_generations WHERE tunnel_id=$1 AND generation=$4)`,
		tunnelID, label+".tunnels.example.test", oldHost, moved.Tunnel.Generation).Scan(&reserved, &oldReserved, &routes, &oldRoutes, &snapshot); err != nil {
		t.Fatal(err)
	}
	if reserved != 1 || oldReserved != 0 || routes != 1 || oldRoutes != 0 || !strings.Contains(snapshot, want) {
		t.Fatalf("reserved=%d oldReserved=%d routes=%d oldRoutes=%d snapshot has new address=%v", reserved, oldReserved, routes, oldRoutes, strings.Contains(snapshot, want))
	}
	var audited int
	if err := f.database.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.audit_events WHERE resource_id=$1 AND event_type='tunnel.address_changed' AND metadata->>'previous_hostname'=$2`, tunnelID, oldHost).Scan(&audited); err != nil || audited != 1 {
		t.Fatalf("address audit events = %d, %v", audited, err)
	}

	// Another account cannot take it; availability agrees.
	if _, err := f.changeAddress(t, otherAccount, otherID, label, other.Tunnel.Generation); !errors.Is(err, ErrSubdomainTaken) {
		t.Fatalf("taking a held name = %v, want ErrSubdomainTaken", err)
	}
	if c, err := f.repository.CheckAddress(ctx, otherAccount, otherID, label); err != nil || c.Free || c.Current {
		t.Fatalf("held name check = %+v err=%v", c, err)
	}
	// The old generated name stays the tunnel's for the cooldown: another account cannot take it, the tunnel can
	// take it back, and once the cooldown has passed it is free for anyone.
	oldLabel := oldHost[:len(oldHost)-len(".tunnels.example.test")]
	if c, err := f.repository.CheckAddress(ctx, otherAccount, otherID, oldLabel); err != nil || c.Free {
		t.Fatalf("name in its cooldown check = %+v err=%v", c, err)
	}
	if _, err := f.changeAddress(t, otherAccount, otherID, oldLabel, other.Tunnel.Generation); !errors.Is(err, ErrSubdomainTaken) {
		t.Fatalf("taking a name in its cooldown = %v, want ErrSubdomainTaken", err)
	}
	if c, err := f.repository.CheckAddress(ctx, account, tunnelID, oldLabel); err != nil || !c.Free {
		t.Fatalf("own old name check = %+v err=%v", c, err)
	}
	if _, err := f.database.SQL().ExecContext(ctx, `UPDATE paperboat.tunnel_address_holds SET until=now()-interval '1 second' WHERE hostname=$1`, oldHost); err != nil {
		t.Fatal(err)
	}
	if c, err := f.repository.CheckAddress(ctx, otherAccount, otherID, oldLabel); err != nil || !c.Free || c.Host != oldHost {
		t.Fatalf("released name check = %+v err=%v", c, err)
	}
	if got, err := f.changeAddress(t, otherAccount, otherID, oldLabel, other.Tunnel.Generation); err != nil || got.Tunnel.StableEndpoint != "https://"+oldHost {
		t.Fatalf("taking the released name = %+v, %v", got.Tunnel.StableEndpoint, err)
	}
	// A stale generation changes nothing.
	if _, err := f.changeAddress(t, account, tunnelID, "stale"+tail, created.Tunnel.Generation); !errors.Is(err, ErrGenerationConflict) {
		t.Fatalf("stale generation = %v", err)
	}
	// Same address again is unchanged and keeps the generation.
	if same, err := f.changeAddress(t, account, tunnelID, label, moved.Tunnel.Generation); err != nil || same.Changed || same.Tunnel.Generation != moved.Tunnel.Generation {
		t.Fatalf("same address changed=%v generation=%d err=%v", same.Changed, same.Tunnel.Generation, err)
	}
	// A name an operator blocked cannot be taken; a lifted block (the row stays as history) frees it.
	blockedName := "blocked" + tail
	if _, err := f.database.SQL().ExecContext(ctx, `INSERT INTO paperboat.publication_blocks(hostname,blocked,reason) VALUES($1,true,'test')`, blockedName+".tunnels.example.test"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.database.SQL().ExecContext(ctx, `DELETE FROM paperboat.publication_blocks WHERE hostname=$1`, blockedName+".tunnels.example.test")
	})
	if _, err := f.changeAddress(t, account, tunnelID, blockedName, moved.Tunnel.Generation); !errors.Is(err, ErrSubdomainTaken) {
		t.Fatalf("taking a blocked name = %v, want ErrSubdomainTaken", err)
	}
	if _, err := f.database.SQL().ExecContext(ctx, `UPDATE paperboat.publication_blocks SET blocked=false WHERE hostname=$1`, blockedName+".tunnels.example.test"); err != nil {
		t.Fatal(err)
	}
	if c, err := f.repository.CheckAddress(ctx, account, tunnelID, blockedName); err != nil || !c.Free {
		t.Fatalf("name with a lifted block check = %+v err=%v", c, err)
	}
	// An operator block pins the address.
	if _, err := f.database.SQL().ExecContext(ctx, `INSERT INTO paperboat.publication_blocks(hostname,blocked,reason) VALUES($1,true,'test')`, label+".tunnels.example.test"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.database.SQL().ExecContext(ctx, `DELETE FROM paperboat.publication_blocks WHERE hostname=$1`, label+".tunnels.example.test")
	})
	if _, err := f.changeAddress(t, account, tunnelID, "escape"+tail, moved.Tunnel.Generation); !errors.Is(err, ErrAddressBlocked) {
		t.Fatalf("renaming a blocked address = %v, want ErrAddressBlocked", err)
	}
}
