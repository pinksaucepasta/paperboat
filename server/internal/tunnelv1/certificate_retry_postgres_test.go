package tunnelv1

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/server/internal/testutil"
	"github.com/pinksaucepasta/paperboat/server/internal/tunnelcert"
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

// fanoutIssuer records every hostname it is asked to issue and answers each with a classified outage.
type fanoutIssuer struct {
	mu    sync.Mutex
	asked []string
}

func (i *fanoutIssuer) Issue(_ context.Context, request tunnelcert.IssueRequest) (tunnelcert.CertificateBundle, error) {
	return i.issue(request.Domain.Hostname)
}

func (i *fanoutIssuer) IssueDNS01(_ context.Context, domain tunnelcert.Domain) (tunnelcert.CertificateBundle, error) {
	return i.issue(domain.Hostname)
}

func (i *fanoutIssuer) issue(hostname string) (tunnelcert.CertificateBundle, error) {
	i.mu.Lock()
	i.asked = append(i.asked, hostname)
	i.mu.Unlock()
	return tunnelcert.CertificateBundle{}, tunnelcert.ErrIssuerUnavailable
}

func (i *fanoutIssuer) saw(hostname string) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	for _, asked := range i.asked {
		if asked == hostname {
			return true
		}
	}
	return false
}

// fanoutLock fails issuance-lock acquisition for one domain with an error that carries no
// certificate failure code. The coordinator returns lock errors unwrapped, so this is the code-less
// Ensure error a durable row can produce; issuer errors are always wrapped as classified outages.
type fanoutLock struct {
	*tunnelcert.MemoryLock
	failingDomain string
}

func (l fanoutLock) Acquire(ctx context.Context, domainID, owner string, generation uint64, now, until time.Time) (bool, error) {
	if domainID == l.failingDomain {
		return false, errors.New("issuance lock store rejected the domain")
	}
	return l.MemoryLock.Acquire(ctx, domainID, owner, generation, now, until)
}

// A due domain whose issuance fails without a classified code must not stop the reconciliation: the
// domains after it are issued in the same pass, and the failing one backs off like a classified failure.
func TestOneFailingCertificateDomainDoesNotBlockTheRestOnPostgres(t *testing.T) {
	f := newTRK07PostgresFixture(t)
	account, host := f.addAccount(t, "certificate-fanout")
	ctx := context.Background()
	now := f.now
	label := strings.ReplaceAll(f.suffix, "_", "-")
	tunnel, route, edge, epoch := "tun_fanout_"+f.suffix, "rte_fanout_"+f.suffix, "edge_fanout_"+f.suffix, "epoch_fanout_"+f.suffix
	connector, session := "con_fanout_"+f.suffix, "ses_fanout_"+f.suffix
	blocked, next := "dom_block_"+f.suffix, "dom_next_"+f.suffix
	blockedHost, nextHost := "block-"+label+".example.test", "next-"+label+".example.test"
	endpointID := testutil.EndpointID("certificate-fanout:" + f.suffix)
	identity := sha256.Sum256([]byte("connector:" + connector))
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := f.database.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`
INSERT INTO paperboat.tunnels
 (id,account_id,name,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id,summary_transitioned_at,created_at,updated_at)
VALUES ($1,$2,$1,$3,$4,$5,$2,$6,$6,$6)`, tunnel, account, endpointID, "https://"+testutil.EndpointLabel(endpointID)+".tunnels.example.test", host, now)
	exec(`
INSERT INTO paperboat.tunnel_routes
 (id,tunnel_id,name,protocol,match_type,match_hostname,origin_scheme,origin_address,created_by_actor_id,updated_by_actor_id,created_at,updated_at)
VALUES ($1,$2,'web','http','exact',$3,'http','127.0.0.1:3000',$4,$4,$5,$5)`, route, tunnel, blockedHost, account, now)
	exec(`INSERT INTO paperboat.tunnel_config_generations(tunnel_id,generation,content_hash,snapshot,activation_state,created_by_actor_id,retained_until) VALUES($1,1,$2,$3,'pending',$4,$5)`, tunnel, identity[:], []byte(`{}`), account, now.Add(24*time.Hour))
	exec(`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,version,roles,region,failure_domain,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at)
VALUES($1,'default','v1',$2,'ready',true,$3,1,ARRAY['edge']::text[],'test','domain',ARRAY['http2','http3']::text[],100,0,$3,$3::timestamptz+interval '1 hour')`, edge, epoch, now)
	// The edge serves the route on the agent's carrier: the endpoint's assignment, observed ready.
	exec(`INSERT INTO paperboat.endpoints(id,kind,account_id,agent_machine_id,access_mode,lifetime,tunnel_id) VALUES($1,'tunnel',$2,$3,'public','persistent',$1)`, tunnel, account, host)
	exec(`INSERT INTO paperboat.endpoint_routes(id,endpoint_id,hostname,protocol,tunnel_route_id) VALUES($1,$2,$3,'http',$1)`, route, tunnel, blockedHost)
	exec(`INSERT INTO paperboat.endpoint_edge_assignments(route_id,endpoint_id,edge_node_id,edge_process_epoch,carrier_session_id,observed_ready,observed_at) VALUES($1,$2,$3,$4,$5,true,$6)`, route, tunnel, edge, epoch, session, now)
	for _, domain := range []struct{ id, hostname string }{{blocked, blockedHost}, {next, nextHost}} {
		exec(`INSERT INTO paperboat.tunnel_domains (id,account_id,tunnel_id,route_id,hostname,match_type,ownership_challenge_reference,ownership_state,dns_target,certificate_strategy,certificate_state,caa_state,conflict_state)
VALUES ($1,$2,$3,$4,$5,'exact',$1,'verified',$5,'managed','issuing','ready','clear')`, domain.id, account, tunnel, route, domain.hostname)
	}

	issuer := &fanoutIssuer{}
	store, err := tunnelcert.NewSQLStore(f.database)
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := tunnelcert.NewCoordinator(tunnelcert.Config{Store: store, Locks: fanoutLock{MemoryLock: tunnelcert.NewMemoryLock(), failingDomain: blocked}, Keys: tunnelcert.ReferenceKeySource{Keys: map[string][]byte{"master/current": make([]byte, 32)}}, Issuer: issuer, CAA: tunnelcert.MemoryCAA{Result: tunnelcert.CAAResult{State: "ready"}}, Distributor: tunnelcert.NewMemoryDistributor(), MasterKeyReference: "master/current", OwnerID: "certificate-fanout-worker"})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := NewCertificateWorker(f.database, coordinator, 30*24*time.Hour, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.Reconcile(ctx, 500); err != nil {
		t.Fatalf("one failing certificate domain stopped the reconciliation: %v", err)
	}
	if !issuer.saw(nextHost) {
		t.Fatalf("the domain after the failing one was never attempted; issuer saw %v", issuer.asked)
	}
	var attempts int32
	var retryAt sql.NullTime
	var state, code string
	if err := f.database.SQL().QueryRowContext(ctx, `SELECT certificate_failure_attempts, certificate_retry_at, certificate_state, COALESCE(certificate_failure_code,'') FROM paperboat.tunnel_domains WHERE id=$1`, blocked).Scan(&attempts, &retryAt, &state, &code); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || !retryAt.Valid || !retryAt.Time.After(time.Now()) || state != "failed" || code != "certificate_issuance_failed" {
		t.Fatalf("failing domain = attempts %d retry %+v state %q code %q, want 1, a future retry, failed, certificate_issuance_failed", attempts, retryAt, state, code)
	}
}
