package tunnelv1

import (
	"context"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/db/dbsqlc"
)

func TestRetiredTunnelDNSLeavesQueueAfterVerifiedWithdrawalPostgres(t *testing.T) {
	f := newTRK07PostgresFixture(t)
	ctx := context.Background()
	account, host := f.addAccount(t, "dns-retirement")
	tunnel := "tun_dns_retirement_" + f.suffix
	f.createTunnel(t, account, host, tunnel, "dns-retirement-"+f.suffix)
	t.Cleanup(func() {
		if _, err := f.database.SQL().ExecContext(ctx, `DELETE FROM paperboat.edge_dns_publications WHERE owner_id=$1`, tunnel); err != nil {
			t.Error(err)
		}
	})
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := f.database.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	queued := func(want bool) {
		t.Helper()
		rows, err := f.database.Queries().ListEdgeDNSPublicationResourcesV1(ctx, dbsqlc.ListEdgeDNSPublicationResourcesV1Params{AfterID: tunnel[:len(tunnel)-1], BatchSize: 1})
		got := len(rows) == 1 && rows[0].ID == tunnel
		if err != nil || got != want {
			t.Fatalf("tunnel DNS queued=%v want=%v: %v", got, want, err)
		}
	}
	queued(true)
	exec(`UPDATE paperboat.tunnels SET desired_state='paused' WHERE id=$1`, tunnel)
	queued(false)
	exec(`INSERT INTO paperboat.edge_dns_publications(hostname,owner_id,resource_generation,readiness_version,observed_at,valid_until,publication_generation,desired_addresses,provider_records,state,created_at,updated_at) VALUES($1,$2,1,'fixture',now(),$3,1,'["8.8.8.8"]','[]','verified',now(),now())`, "retired-"+f.suffix+".example.test", tunnel, time.Now().Add(time.Hour))
	queued(true)
	exec(`UPDATE paperboat.edge_dns_publications SET desired_addresses='[]',provider_records='[]',state='submitted' WHERE owner_id=$1`, tunnel)
	queued(true)
	exec(`UPDATE paperboat.edge_dns_publications SET state='verified',verified_at=now() WHERE owner_id=$1`, tunnel)
	queued(false)
	exec(`UPDATE paperboat.tunnels SET desired_state='active',expires_at=now()+interval '1 hour' WHERE id=$1`, tunnel)
	queued(true)
	exec(`UPDATE paperboat.tunnels SET created_at=now()-interval '1 hour',expires_at=now()-interval '1 second' WHERE id=$1`, tunnel)
	queued(false)
	exec(`UPDATE paperboat.tunnels SET expires_at=now()+interval '1 hour' WHERE id=$1`, tunnel)
	queued(true)
	exec(`UPDATE paperboat.tunnels SET desired_state='deleted',deleted_at=now() WHERE id=$1`, tunnel)
	queued(false)
}
