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
	// Nothing else may mint it meanwhile either (a new resource claiming the name).
	if _, err := f.database.SQL().ExecContext(ctx, `SELECT paperboat.claim_managed_hostname($1,'preview',$2)`, oldHost, "pre_hold_"+f.suffix); err == nil {
		t.Fatal("a name in its cooldown was claimed by a new resource")
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

// A self-hosted workspace's tunnel has no managed address: the change and the availability check say so.
func TestSQLRepositoryRefusesAnAddressChangeInASelfHostedWorkspaceOnPostgres(t *testing.T) {
	f := newTRK07PostgresFixture(t)
	ctx := context.Background()
	account := f.accounts[0]
	tunnelID := "tun_addr_sh_" + f.suffix
	created := f.createTunnel(t, account, "mch_trk07_primary_"+f.suffix, tunnelID, "addr-sh-"+f.suffix)
	if _, err := f.database.SQL().ExecContext(ctx, `INSERT INTO paperboat.selfhost_pool_policies(scope_kind,scope_id,capability,mode,installation_ids) VALUES('account',$1,'tunnel','self-hosted-only','{}') ON CONFLICT(scope_kind,scope_id,capability) DO UPDATE SET mode='self-hosted-only'`, account); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.database.SQL().ExecContext(ctx, `DELETE FROM paperboat.selfhost_pool_policies WHERE scope_kind='account' AND scope_id=$1 AND capability='tunnel'`, account)
	})
	if _, err := f.changeAddress(t, account, tunnelID, "hello"+f.suffix[len(f.suffix)-6:], created.Tunnel.Generation); !errors.Is(err, ErrAddressUnavailable) {
		t.Fatalf("changing the address of a self-hosted tunnel = %v, want ErrAddressUnavailable", err)
	}
	if c, err := f.repository.CheckAddress(ctx, account, tunnelID, "helloworld"); err != nil || !c.SelfHosted {
		t.Fatalf("availability for a self-hosted tunnel = %+v, %v", c, err)
	}
}

// Two tunnels racing for one name: exactly one gets it, the other is told it is taken, and an expired move of one
// tunnel is never taken over by the other (the settle worker undoes it first).
func TestSQLRepositoryAddressClaimsRaceToOneWinnerOnPostgres(t *testing.T) {
	f := newTRK07PostgresFixture(t)
	ctx := context.Background()
	accountA, hostA := f.accounts[0], "mch_trk07_primary_"+f.suffix
	accountB, hostB := f.addAccount(t, "race")
	tail := strings.ReplaceAll(f.suffix, "_", "")
	tail = tail[len(tail)-6:]
	a := f.createTunnel(t, accountA, hostA, "tun_race_a_"+f.suffix, "race-a-"+f.suffix)
	b := f.createTunnel(t, accountB, hostB, "tun_race_b_"+f.suffix, "race-b-"+f.suffix)
	label := "contested" + tail
	errs := make(chan error, 2)
	start := make(chan struct{})
	for _, c := range []struct {
		account, tunnel string
		generation      int64
	}{{accountA, a.Tunnel.ID, a.Tunnel.Generation}, {accountB, b.Tunnel.ID, b.Tunnel.Generation}} {
		go func() {
			<-start
			_, err := f.changeAddress(t, c.account, c.tunnel, label, c.generation)
			errs <- err
		}()
	}
	close(start)
	first, second := <-errs, <-errs
	if (first == nil) == (second == nil) {
		t.Fatalf("both or neither tunnel got the name: %v, %v", first, second)
	}
	if loser := errors.Join(first, second); !errors.Is(loser, ErrSubdomainTaken) {
		t.Fatalf("the loser = %v, want ErrSubdomainTaken", loser)
	}
	var owners int
	if err := f.database.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.managed_hostnames WHERE hostname=$1`, label+".tunnels.example.test").Scan(&owners); err != nil || owners != 1 {
		t.Fatalf("owners of the name = %d, %v", owners, err)
	}

	// A move of tunnel A past its deadline still holds its name against B's claim until the settle worker undoes it.
	other := "expired" + tail
	host := other + ".tunnels.example.test"
	if _, err := f.database.SQL().ExecContext(ctx, `INSERT INTO paperboat.tunnel_address_holds(hostname,tunnel_id,operation_id,kind,until) VALUES($1,$2,'op_expired_'||$3,'incoming',now()-interval '1 minute')`, host, a.Tunnel.ID, f.suffix); err != nil {
		t.Fatal(err)
	}
	var bGeneration int64
	if err := f.database.SQL().QueryRowContext(ctx, `SELECT generation FROM paperboat.tunnels WHERE id=$1`, b.Tunnel.ID).Scan(&bGeneration); err != nil {
		t.Fatal(err)
	}
	if _, err := f.changeAddress(t, accountB, b.Tunnel.ID, other, bGeneration); !errors.Is(err, ErrSubdomainTaken) {
		t.Fatalf("claiming a name another tunnel's expired move still holds = %v, want ErrSubdomainTaken", err)
	}
	if c, err := f.repository.CheckAddress(ctx, accountB, b.Tunnel.ID, other); err != nil || c.Free {
		t.Fatalf("availability of a name an expired move still holds = %+v, %v; want not free", c, err)
	}
	if err := f.repository.SettleAddressMoves(ctx, AddressSettleRecord{Now: time.Now().UTC(), Limit: 10, Policy: AddressPolicy{MoveTimeout: time.Minute, Cooldown: time.Hour}, ActorID: "settler", ActorType: "system", RequestID: "req_race", CorrelationID: "corr_race"}); err != nil {
		t.Fatal(err)
	}
	var held int
	if err := f.database.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.tunnel_address_holds WHERE hostname=$1`, host).Scan(&held); err != nil || held != 0 {
		t.Fatalf("holds on the name after the settle = %d, %v", held, err)
	}
	if err := f.database.SQL().QueryRowContext(ctx, `SELECT generation FROM paperboat.tunnels WHERE id=$1`, b.Tunnel.ID).Scan(&bGeneration); err != nil {
		t.Fatal(err)
	}
	if got, err := f.changeAddress(t, accountB, b.Tunnel.ID, other, bGeneration); err != nil || got.Tunnel.StableEndpoint != "https://"+host {
		t.Fatalf("claiming the name once its move was undone = %q, %v", got.Tunnel.StableEndpoint, err)
	}
}
