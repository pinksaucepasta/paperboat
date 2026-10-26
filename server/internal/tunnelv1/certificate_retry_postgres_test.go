package tunnelv1

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/server/internal/testutil"
)

// A failed durable-route certificate is not selected again until its retry time, and is selected once
// that time has passed.
func TestDurableCertificateFailureBacksOffBeforeItIsDueAgainOnPostgres(t *testing.T) {
	f := newTRK07PostgresFixture(t)
	account, _ := f.addAccount(t, "certificate-retry")
	ctx := context.Background()
	tunnel, route, domain := "tun_retry_"+f.suffix, "rte_retry_"+f.suffix, "dom_retry_"+f.suffix
	hostname := "retry-" + f.suffix + ".example.test"
	endpointID := testutil.EndpointID("certificate-retry:" + f.suffix)
	now := f.now
	if _, err := f.database.SQL().ExecContext(ctx, `
INSERT INTO paperboat.tunnels
 (id,account_id,name,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id,summary_transitioned_at,created_at,updated_at)
VALUES ($1,$2,$1,$3,$4,$5,$2,$6,$6,$6)`, tunnel, account, endpointID, "https://"+testutil.EndpointLabel(endpointID)+".tunnels.example.test", "mch_"+f.suffix, now); err != nil {
		t.Fatal(err)
	}
	if _, err := f.database.SQL().ExecContext(ctx, `
INSERT INTO paperboat.tunnel_routes
 (id,tunnel_id,name,protocol,match_type,match_hostname,origin_scheme,origin_address,created_by_actor_id,updated_by_actor_id,created_at,updated_at)
VALUES ($1,$2,'web','http','exact',$3,'http','127.0.0.1:3000',$4,$4,$5,$5)`, route, tunnel, hostname, account, now); err != nil {
		t.Fatal(err)
	}
	if _, err := f.database.SQL().ExecContext(ctx, `
INSERT INTO paperboat.tunnel_domains
 (id,account_id,tunnel_id,route_id,hostname,match_type,ownership_challenge_reference,ownership_state,dns_target,certificate_strategy,certificate_state,caa_state,conflict_state)
VALUES ($1,$2,$3,$4,$5,'exact',$1,'verified',$5,'managed','issuing','ready','clear')`, domain, account, tunnel, route, hostname); err != nil {
		t.Fatal(err)
	}
	due := func(at time.Time) bool {
		t.Helper()
		rows, err := f.database.Queries().ListDueTunnelCertificateDomainsV1(ctx, dbsqlc.ListDueTunnelCertificateDomainsV1Params{Now: at, RenewBefore: pgtype.Interval{}, RowLimit: 10000})
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if row.ID == domain {
				return true
			}
		}
		return false
	}
	if !due(now) {
		t.Fatal("issuing domain is not due")
	}
	retryAt := now.Add(time.Minute)
	failed, err := f.database.Queries().MarkTunnelDomainCertificateFailureV1(ctx, dbsqlc.MarkTunnelDomainCertificateFailureV1Params{Now: sql.NullTime{Time: now, Valid: true}, FailureCode: sql.NullString{String: "edge_certificate_not_ready", Valid: true}, RetryAt: sql.NullTime{Time: retryAt, Valid: true}, DomainID: domain, ExpectedGeneration: 1})
	if err != nil {
		t.Fatal(err)
	}
	if failed.CertificateFailureAttempts != 1 || !failed.CertificateRetryAt.Valid || !failed.CertificateRetryAt.Time.Equal(retryAt) {
		t.Fatalf("failed domain = attempts %d retry %+v, want 1 and %v", failed.CertificateFailureAttempts, failed.CertificateRetryAt, retryAt)
	}
	if due(now) || due(retryAt.Add(-time.Second)) {
		t.Fatal("failed domain is due before its retry time")
	}
	if !due(retryAt) || !due(retryAt.Add(time.Second)) {
		t.Fatal("failed domain is not due after its retry time")
	}
}
