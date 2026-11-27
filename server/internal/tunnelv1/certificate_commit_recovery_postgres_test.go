package tunnelv1

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/pem"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pinksaucepasta/paperboat/server/internal/testutil"
	"github.com/pinksaucepasta/paperboat/server/internal/tunnelcert"
)

// commitRecoveryIssuer serves each workspace domain a fresh self-signed certificate for its hostname, expiring at
// the time set for it, and counts the calls: the commit-recovery paths must never order again.
type commitRecoveryIssuer struct {
	mu       sync.Mutex
	hostname map[string]string
	notAfter map[string]time.Time
	calls    map[string]int
}

func (i *commitRecoveryIssuer) Issue(context.Context, tunnelcert.IssueRequest) (tunnelcert.CertificateBundle, error) {
	return tunnelcert.CertificateBundle{}, tunnelcert.ErrIssuerUnavailable
}

func (i *commitRecoveryIssuer) WorkspaceCertificate(_ context.Context, _, workspaceDomainID string) (tunnelcert.CertificateBundle, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.calls[workspaceDomainID]++
	hostname, notAfter := i.hostname[workspaceDomainID], i.notAfter[workspaceDomainID]
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return tunnelcert.CertificateBundle{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return tunnelcert.CertificateBundle{}, err
	}
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: hostname}, DNSNames: []string{hostname}, NotBefore: time.Now().Add(-time.Minute).UTC().Truncate(time.Second), NotAfter: notAfter, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	raw, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return tunnelcert.CertificateBundle{}, err
	}
	return tunnelcert.CertificateBundle{CertificatePEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: raw}), PrivateKeyPEM: pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), Issuer: "test-ca", NotBefore: template.NotBefore, NotAfter: notAfter}, nil
}

func (i *commitRecoveryIssuer) callsFor(workspaceDomainID string) int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.calls[workspaceDomainID]
}

type commitRecoveryFixture struct {
	f      *trk07PostgresFixture
	ctx    context.Context
	issuer *commitRecoveryIssuer
	worker *CertificateWorker
	// domain id → its workspace domain id
	workspace map[string]string
}

// newCommitRecoveryFixture: one tunnel route served by a live edge on the agent's live carrier, with a managed domain
// under its own verified workspace domain for each name, each with one running domain.create.
func newCommitRecoveryFixture(t *testing.T, names ...string) *commitRecoveryFixture {
	t.Helper()
	f := newTRK07PostgresFixture(t)
	account, host := f.addAccount(t, "certificate-commit")
	ctx := context.Background()
	now := f.now
	label := strings.ReplaceAll(f.suffix, "_", "-")
	tunnel, route, edge, epoch := "tun_commit_"+f.suffix, "rte_commit_"+f.suffix, "edge_commit_"+f.suffix, "epoch_commit_"+f.suffix
	connector, session := "con_commit_"+f.suffix, testutil.CarrierSessionID("commit:"+f.suffix)
	endpointID := testutil.EndpointID("certificate-commit:" + f.suffix)
	identity := sha256.Sum256([]byte("connector:" + connector))
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := f.database.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.tunnels (id,account_id,name,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id,summary_transitioned_at,created_at,updated_at)
VALUES ($1,$2,$1,$3,$4,$5,$2,$6,$6,$6)`, tunnel, account, endpointID, "https://"+testutil.EndpointLabel(endpointID)+".tunnels.example.test", host, now)
	exec(`INSERT INTO paperboat.tunnel_routes (id,tunnel_id,name,protocol,match_type,match_hostname,origin_scheme,origin_address,created_by_actor_id,updated_by_actor_id,created_at,updated_at)
VALUES ($1,$2,'web','http','exact',$3,'http','127.0.0.1:3000',$4,$4,$5,$5)`, route, tunnel, "route-"+label+".example.test", account, now)
	exec(`INSERT INTO paperboat.tunnel_config_generations(tunnel_id,generation,content_hash,snapshot,activation_state,created_by_actor_id,retained_until) VALUES($1,1,$2,$3,'pending',$4,$5)`, tunnel, identity[:], []byte(`{}`), account, now.Add(24*time.Hour))
	exec(`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,version,roles,region,failure_domain,transports,capacity_used,capacity_observed_at,registry_expires_at)
VALUES($1,'default','v1',$2,'ready',true,$3,1,ARRAY['edge']::text[],'test','domain',ARRAY['http2','http3']::text[],0,$3,$3::timestamptz+interval '1 hour')`, edge, epoch, now)
	exec(`INSERT INTO paperboat.endpoints(id,kind,account_id,agent_machine_id,access_mode,lifetime,tunnel_id) VALUES($1,'tunnel',$2,$3,'public','persistent',$1)`, tunnel, account, host)
	exec(`INSERT INTO paperboat.endpoint_routes(id,endpoint_id,hostname,protocol,tunnel_route_id) VALUES($1,$2,$3,'http',$1)`, route, tunnel, "route-"+label+".example.test")
	exec(`INSERT INTO paperboat.endpoint_edge_assignments(route_id,endpoint_id,edge_node_id,edge_process_epoch,carrier_session_id,observed_ready,observed_at) VALUES($1,$2,$3,$4,$5,true,$6)`, route, tunnel, edge, epoch, session, now)
	carrierRoute := "crt_commit_" + f.suffix
	t.Cleanup(func() {
		_, _ = f.database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.control_routes WHERE id=$1`, carrierRoute)
	})
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id,desired_state) SELECT environment_id,environment_id,user_id,'active' FROM paperboat.user_machines WHERE id=$1 ON CONFLICT DO NOTHING`, host)
	exec(`INSERT INTO paperboat.control_routes(id,environment_id,kind,public_host,target_host,target_port) SELECT $1,environment_id,'runtime_https_wss',$1||'.runtime.example.test','127.0.0.1',38080 FROM paperboat.user_machines WHERE id=$2`, carrierRoute, host)
	exec(`INSERT INTO paperboat.runtime_carrier_attachments(route_id,machine_id,account_id,installation_generation,worker_generation,process_id,generation,edge_node_id,edge_process_epoch,public_identity_key,route_revision,session_id,preview_session_id,admission,expires_at,active,ready) SELECT $1,m.id,m.user_id,m.installation_generation,1,'process_commit',1,$2,$3,m.public_identity_key,1,$4,$4,'{}',$5::timestamptz+interval '1 hour',true,true FROM paperboat.user_machines m WHERE m.id=$6`, carrierRoute, edge, epoch, session, now, host)
	issuer := &commitRecoveryIssuer{hostname: map[string]string{}, notAfter: map[string]time.Time{}, calls: map[string]int{}}
	fixture := &commitRecoveryFixture{f: f, ctx: ctx, issuer: issuer, workspace: map[string]string{}}
	for _, name := range names {
		domain, hostname := "dom_"+name+"_"+f.suffix, name+"-"+label+".example.test"
		ws := "wsd_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		fixture.workspace[domain] = ws
		issuer.hostname[ws], issuer.notAfter[ws] = hostname, time.Now().Add(60*24*time.Hour).UTC().Truncate(time.Second)
		exec(`INSERT INTO paperboat.workspace_domains(id,account_id,hostname,challenge_reference,ownership_state,created_by_actor_id) VALUES($1,$2,$3,'x','verified',$2)`, ws, account, ws[4:]+".example.test")
		exec(`INSERT INTO paperboat.tunnel_domains (id,account_id,tunnel_id,route_id,hostname,match_type,ownership_challenge_reference,dns_target,certificate_strategy,certificate_state,caa_state,conflict_state,workspace_domain_id)
VALUES ($1,$2,$3,$4,$5,'exact',$1,$5,'managed','issuing','ready','clear',$6)`, domain, account, tunnel, route, hostname, ws)
		fixture.addCreateOperation(t, domain, "a")
	}
	store, err := tunnelcert.NewSQLStore(f.database)
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := tunnelcert.NewCoordinator(tunnelcert.Config{Store: store, Locks: tunnelcert.NewMemoryLock(), Keys: tunnelcert.ReferenceKeySource{Keys: map[string][]byte{"master/current": make([]byte, 32)}}, Issuer: issuer, WorkspaceCertificates: issuer, CAA: tunnelcert.MemoryCAA{Result: tunnelcert.CAAResult{State: "ready"}}, Distributor: tunnelcert.NewMemoryDistributor(), MasterKeyReference: "master/current", OwnerID: "certificate-commit-worker"})
	if err != nil {
		t.Fatal(err)
	}
	if fixture.worker, err = NewCertificateWorker(f.database, coordinator, time.Now); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (x *commitRecoveryFixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := x.f.database.SQL().ExecContext(x.ctx, query, args...); err != nil {
		t.Fatal(err)
	}
}

// addCreateOperation adds a running domain.create for domain. Two of them make CommitDomainCertificateReady refuse
// (it completes exactly one): a commit conflict that persists until one is removed.
func (x *commitRecoveryFixture) addCreateOperation(t *testing.T, domain, tag string) {
	t.Helper()
	op := "op_" + tag + "_" + domain
	sum := sha256.Sum256([]byte(op))
	x.exec(t, `INSERT INTO paperboat.operations (id,account_id,idempotency_key,request_hash,operation_type,resource_kind,resource_id,phase,state,progress,outcome,correlation_id,created_at,updated_at)
SELECT $1,account_id,$2,$3,'domain.create','domain_binding',id,'issuing_certificate','running',60,'changed',$4,now(),now() FROM paperboat.tunnel_domains WHERE id=$5`, op, "idem_"+op, sum[:], "cor_"+op, domain)
}

type commitRecoveryState struct {
	state     string
	reference string
	expires   sql.NullTime
	attempts  int32
	retryAt   sql.NullTime
	code      string
}

func (x *commitRecoveryFixture) state(t *testing.T, domain string) commitRecoveryState {
	t.Helper()
	var s commitRecoveryState
	var reference sql.NullString
	if err := x.f.database.SQL().QueryRowContext(x.ctx, `SELECT certificate_state,certificate_reference,certificate_expires_at,certificate_failure_attempts,certificate_retry_at,COALESCE(certificate_failure_code,'') FROM paperboat.tunnel_domains WHERE id=$1`, domain).Scan(&s.state, &reference, &s.expires, &s.attempts, &s.retryAt, &s.code); err != nil {
		t.Fatal(err)
	}
	s.reference = reference.String
	return s
}

func (x *commitRecoveryFixture) reconcile(t *testing.T) {
	t.Helper()
	if _, err := x.worker.Reconcile(x.ctx, 500); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

// A domain whose first-certificate commit conflicts is backed off on its own: the rest of the batch still gets its
// certificate (the conflict aborted the whole pass). Once the conflict clears, its activated certificate is recorded
// without ordering it again.
func TestCertificateCommitConflictBacksOffOneDomainAndRecoversWithoutReorderingOnPostgres(t *testing.T) {
	x := newCommitRecoveryFixture(t, "alpha", "beta")
	alpha, beta := "dom_alpha_"+x.f.suffix, "dom_beta_"+x.f.suffix
	x.addCreateOperation(t, alpha, "b")
	x.addCreateOperation(t, beta, "b")
	x.exec(t, `DELETE FROM paperboat.operations WHERE id=$1`, "op_b_"+beta)
	x.reconcile(t)
	if got := x.state(t, beta); got.state != "ready" || got.reference == "" {
		t.Fatalf("the domain beside a conflicting one = %+v, want ready with a certificate", got)
	}
	got := x.state(t, alpha)
	if got.state != "failed" || got.reference != "" || got.attempts != 1 || !got.retryAt.Valid || !got.retryAt.Time.After(time.Now()) {
		t.Fatalf("conflicting domain = %+v, want failed, no certificate, 1 attempt, a future retry", got)
	}
	// The conflict clears and the retry is due: the certificate it already activated is recorded, no new order.
	x.exec(t, `DELETE FROM paperboat.operations WHERE id=$1`, "op_b_"+alpha)
	x.exec(t, `UPDATE paperboat.tunnel_domains SET certificate_retry_at=now()-interval '1 minute' WHERE id=$1`, alpha)
	x.reconcile(t)
	if got := x.state(t, alpha); got.state != "ready" || got.reference == "" {
		t.Fatalf("domain after its conflict cleared = %+v, want ready with its certificate", got)
	}
	if calls := x.issuer.callsFor(x.workspace[alpha]); calls != 1 {
		t.Fatalf("certificate orders for the recovered domain = %d, want 1", calls)
	}
	var created string
	if err := x.f.database.SQL().QueryRowContext(x.ctx, `SELECT state FROM paperboat.operations WHERE id=$1`, "op_a_"+alpha).Scan(&created); err != nil || created != "succeeded" {
		t.Fatalf("domain.create = %q, %v; want succeeded", created, err)
	}
}

// A renewal whose commit is lost (the domain keeps its old certificate reference while the renewed certificate is
// active) is recorded on the next due pass. It was refused as a stale publication until the old certificate
// expired, and a renewal after any failed attempt (state renewing) could not commit at all.
func TestCertificateRenewalWhoseCommitWasLostIsRecordedOnPostgres(t *testing.T) {
	x := newCommitRecoveryFixture(t, "gamma")
	gamma := "dom_gamma_" + x.f.suffix
	ws := x.workspace[gamma]
	x.reconcile(t)
	first := x.state(t, gamma)
	if first.state != "ready" || first.reference == "" {
		t.Fatalf("first certificate = %+v, want ready", first)
	}
	// The workspace domain renews its certificate: the hostname is due to serve the new copy. Its commit conflicts.
	renewed := x.issuer.notAfter[ws].Add(10 * 24 * time.Hour)
	x.issuer.mu.Lock()
	x.issuer.notAfter[ws] = renewed
	x.issuer.mu.Unlock()
	// (placeholder material: the test issuer serves the bundle; only the expiry drives the due query)
	x.exec(t, `UPDATE paperboat.workspace_domains SET certificate_expires_at=$2,master_key_reference='master/current',certificate_ciphertext='\x00'::bytea,private_key_ciphertext='\x00'::bytea,certificate_fingerprint=decode(repeat('ab',32),'hex') WHERE id=$1`, ws, renewed)
	x.addCreateOperation(t, gamma, "r1")
	x.addCreateOperation(t, gamma, "r2")
	x.reconcile(t)
	lost := x.state(t, gamma)
	if lost.reference != first.reference || lost.attempts != 1 {
		t.Fatalf("renewal with a conflicting commit = %+v, want the old reference kept and one failed attempt", lost)
	}
	if calls := x.issuer.callsFor(ws); calls != 2 {
		t.Fatalf("workspace certificate reads = %d, want 2 (first issue, renewal)", calls)
	}
	// The conflict clears and the retry is due: the renewed certificate, already active, is recorded.
	x.exec(t, `DELETE FROM paperboat.operations WHERE id IN ($1,$2)`, "op_r1_"+gamma, "op_r2_"+gamma)
	x.exec(t, `UPDATE paperboat.tunnel_domains SET certificate_retry_at=now()-interval '1 minute' WHERE id=$1`, gamma)
	x.reconcile(t)
	got := x.state(t, gamma)
	if got.state != "ready" || got.reference == "" || got.reference == first.reference || !got.expires.Valid || !got.expires.Time.Equal(renewed) {
		t.Fatalf("renewal after its lost commit = %+v, want ready with the renewed certificate expiring %v", got, renewed)
	}
	if calls := x.issuer.callsFor(ws); calls != 2 {
		t.Fatalf("workspace certificate reads after recovery = %d, want still 2", calls)
	}
}

// A passthrough `domain add --wait` that outlived the 15-minute window of the edge-fetch settlement (a long edge
// outage) is completed by the certificate worker's tick once the edges serve it; one whose domain or route went away
// fails with a clear code (both stayed running for good). A paused tunnel's add waits; other operations are untouched.
func TestPassthroughDomainCreateIsSettledByTheWorkerAfterALongOutageOnPostgres(t *testing.T) {
	x := newCommitRecoveryFixture(t)
	suffix := x.f.suffix
	tunnel, route, edge := "tun_commit_"+suffix, "rte_commit_"+suffix, "edge_commit_"+suffix
	label := strings.ReplaceAll(suffix, "_", "-")
	var account string
	if err := x.f.database.SQL().QueryRowContext(x.ctx, `SELECT account_id FROM paperboat.tunnels WHERE id=$1`, tunnel).Scan(&account); err != nil {
		t.Fatal(err)
	}
	addPassthrough := func(name string) (string, string) {
		t.Helper()
		domain := "dom_" + name + "_" + suffix
		ws := "wsd_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		x.exec(t, `INSERT INTO paperboat.workspace_domains(id,account_id,hostname,challenge_reference,ownership_state,created_by_actor_id) VALUES($1,$2,$3,'x','verified',$2)`, ws, account, ws[4:]+".example.test")
		x.exec(t, `INSERT INTO paperboat.tunnel_domains(id,account_id,tunnel_id,route_id,hostname,match_type,ownership_challenge_reference,dns_target,certificate_strategy,certificate_state,caa_state,conflict_state,workspace_domain_id)
VALUES($1,$2,$3,$4,$5,'exact',$1,$5,'none','not_applicable','ready','clear',$6)`, domain, account, tunnel, route, name+"-"+label+".example.test", ws)
		op := "op_pass_" + domain
		sum := sha256.Sum256([]byte(op))
		// Begun an hour ago: the edges were out for longer than the settlement window.
		x.exec(t, `INSERT INTO paperboat.operations (id,account_id,idempotency_key,request_hash,operation_type,resource_kind,resource_id,phase,state,progress,outcome,correlation_id,created_at,updated_at)
VALUES ($1,$2,$3,$4,'domain.create','domain_binding',$5,'connecting','running',60,'changed',$6,now()-interval '1 hour',now()-interval '1 hour')`, op, account, "idem_"+op, sum[:], domain, "cor_"+op)
		return domain, op
	}
	operation := func(op string) (string, string) {
		t.Helper()
		var state, code string
		if err := x.f.database.SQL().QueryRowContext(x.ctx, `SELECT state,COALESCE(error_code,'') FROM paperboat.operations WHERE id=$1`, op).Scan(&state, &code); err != nil {
			t.Fatal(err)
		}
		return state, code
	}
	_, served := addPassthrough("served")
	removed, gone := addPassthrough("removed")
	x.exec(t, `UPDATE paperboat.tunnel_domains SET deleted_at=now() WHERE id=$1`, removed)
	// The edges are back: live, serving the route, and have fetched a route snapshot since.
	x.exec(t, `UPDATE paperboat.control_tunnel_nodes SET last_heartbeat_at=now(),registry_expires_at=now()+interval '1 hour',tunnel_endpoints_epoch=process_epoch,tunnel_snapshot_at=now() WHERE id=$1`, edge)
	x.exec(t, `UPDATE paperboat.endpoint_edge_assignments SET observed_ready=true,observed_at=now() WHERE edge_node_id=$1`, edge)
	x.reconcile(t)
	if state, code := operation(served); state != "succeeded" {
		t.Fatalf("passthrough domain.create served by every edge after a long outage = %s (%s), want succeeded", state, code)
	}
	if state, code := operation(gone); state != "failed" || code != "domain_binding_removed" {
		t.Fatalf("passthrough domain.create whose domain was removed = %s (%s), want failed domain_binding_removed", state, code)
	}
	// A paused tunnel's add waits (resuming serves it): neither failed nor completed.
	_, waiting := addPassthrough("waiting")
	managed := "op_managed_" + suffix
	managedSum := sha256.Sum256([]byte(managed))
	x.exec(t, `INSERT INTO paperboat.operations (id,account_id,idempotency_key,request_hash,operation_type,resource_kind,resource_id,phase,state,progress,outcome,correlation_id,created_at,updated_at)
VALUES ($1,$2,$3,$4,'domain.create','domain_binding','dom_managed_not_passthrough','issuing_certificate','running',60,'changed',$5,now()-interval '1 hour',now()-interval '1 hour')`, managed, account, "idem_"+managed, managedSum[:], "cor_"+managed)
	x.exec(t, `UPDATE paperboat.tunnels SET desired_state='paused' WHERE id=$1`, tunnel)
	x.reconcile(t)
	if state, code := operation(waiting); state != "running" {
		t.Fatalf("passthrough domain.create on a paused tunnel = %s (%s), want running", state, code)
	}
	// Its route is removed: it can never be served, so it fails; another resource's operation is left alone.
	x.exec(t, `UPDATE paperboat.tunnels SET desired_state='active' WHERE id=$1`, tunnel)
	x.exec(t, `UPDATE paperboat.tunnel_routes SET desired_state='deleted',deleted_at=now() WHERE id=$1`, route)
	x.reconcile(t)
	if state, code := operation(waiting); state != "failed" || code != "domain_binding_removed" {
		t.Fatalf("passthrough domain.create whose route was removed = %s (%s), want failed domain_binding_removed", state, code)
	}
	if state, _ := operation(managed); state != "running" {
		t.Fatalf("an unrelated domain.create = %s, want untouched (running)", state)
	}
}

// Passthrough adds that cannot finish yet (here: on paused tunnels) never fill the settlement batches: a new add
// on a serving tunnel still completes beside more than a batch of them.
func TestPassthroughDomainCreateBatchIsNotStarvedByWaitingAddsOnPostgres(t *testing.T) {
	x := newCommitRecoveryFixture(t)
	suffix := x.f.suffix
	tunnel, route, edge := "tun_commit_"+suffix, "rte_commit_"+suffix, "edge_commit_"+suffix
	label := strings.ReplaceAll(suffix, "_", "-")
	var account string
	if err := x.f.database.SQL().QueryRowContext(x.ctx, `SELECT account_id FROM paperboat.tunnels WHERE id=$1`, tunnel).Scan(&account); err != nil {
		t.Fatal(err)
	}
	ws := "wsd_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	x.exec(t, `INSERT INTO paperboat.workspace_domains(id,account_id,hostname,challenge_reference,ownership_state,created_by_actor_id) VALUES($1,$2,$3,'x','verified',$2)`, ws, account, ws[4:]+".example.test")
	// 120 old waiting adds on a paused tunnel of the same account (more than one batch), each with its own route.
	paused := "tun_paused_" + suffix
	x.exec(t, `INSERT INTO paperboat.tunnels (id,account_id,name,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id,desired_state,summary_transitioned_at,created_at,updated_at)
SELECT $1,account_id,$1,$2,$3,created_by_host_id,account_id,'paused',now(),now(),now() FROM paperboat.tunnels WHERE id=$4`, paused, testutil.EndpointID("paused:"+suffix), "https://"+testutil.EndpointLabel(testutil.EndpointID("paused:"+suffix))+".tunnels.example.test", tunnel)
	x.exec(t, `INSERT INTO paperboat.tunnel_routes (id,tunnel_id,name,protocol,match_type,match_hostname,origin_scheme,origin_address,created_by_actor_id,updated_by_actor_id,created_at,updated_at)
SELECT 'rte_p'||g||'_'||$1,$2,'r'||g,'http','exact','p'||g||'-'||$3||'.example.test','http','127.0.0.1:3000',$4,$4,now(),now() FROM generate_series(1,120) g`, suffix, paused, label, account)
	x.exec(t, `INSERT INTO paperboat.tunnel_domains(id,account_id,tunnel_id,route_id,hostname,match_type,ownership_challenge_reference,dns_target,certificate_strategy,certificate_state,caa_state,conflict_state,workspace_domain_id)
SELECT 'dom_p'||g||'_'||$1,$2,$3,'rte_p'||g||'_'||$1,'d'||g||'-'||$4||'.example.test','exact','dom_p'||g||'_'||$1,'d'||g||'-'||$4||'.example.test','none','not_applicable','ready','clear',$5 FROM generate_series(1,120) g`, suffix, account, paused, label, ws)
	x.exec(t, `INSERT INTO paperboat.operations (id,account_id,idempotency_key,request_hash,operation_type,resource_kind,resource_id,phase,state,progress,outcome,correlation_id,created_at,updated_at)
SELECT 'op_p'||g||'_'||$1,$2,'idem_p'||g||'_'||$1,sha256(convert_to('op_p'||g||'_'||$1::text,'UTF8')),'domain.create','domain_binding','dom_p'||g||'_'||$1,'connecting','running',60,'changed','cor_p'||g||'_'||$1,now()-interval '2 hours',now()-interval '2 hours' FROM generate_series(1,120) g`, suffix, account)
	// A new add (recent) and an old one (long outage) on the serving tunnel both complete beside them.
	x.exec(t, `INSERT INTO paperboat.tunnel_domains(id,account_id,tunnel_id,route_id,hostname,match_type,ownership_challenge_reference,dns_target,certificate_strategy,certificate_state,caa_state,conflict_state,workspace_domain_id)
VALUES($1,$2,$3,$4,$5,'exact',$1,$5,'none','not_applicable','ready','clear',$6),($7,$2,$3,$4,$8,'exact',$7,$8,'none','not_applicable','ready','clear',$6)`, "dom_new_"+suffix, account, tunnel, route, "new-"+label+".example.test", ws, "dom_old_"+suffix, "old-"+label+".example.test")
	for _, op := range []struct{ id, domain, age string }{{"op_new_" + suffix, "dom_new_" + suffix, "1 minute"}, {"op_old_" + suffix, "dom_old_" + suffix, "3 hours"}} {
		x.exec(t, `INSERT INTO paperboat.operations (id,account_id,idempotency_key,request_hash,operation_type,resource_kind,resource_id,phase,state,progress,outcome,correlation_id,created_at,updated_at)
VALUES ($1::text,$2,$3,sha256(convert_to($1::text,'UTF8')),'domain.create','domain_binding',$4,'connecting','running',60,'changed',$5,now()-$6::interval,now()-$6::interval)`, op.id, account, "idem_"+op.id, op.domain, "cor_"+op.id, op.age)
	}
	x.exec(t, `UPDATE paperboat.control_tunnel_nodes SET last_heartbeat_at=now(),registry_expires_at=now()+interval '1 hour',tunnel_endpoints_epoch=process_epoch,tunnel_snapshot_at=now() WHERE id=$1`, edge)
	x.exec(t, `UPDATE paperboat.endpoint_edge_assignments SET observed_ready=true,observed_at=now() WHERE edge_node_id=$1`, edge)
	x.reconcile(t)
	for _, op := range []string{"op_new_" + suffix, "op_old_" + suffix} {
		var state string
		if err := x.f.database.SQL().QueryRowContext(x.ctx, `SELECT state FROM paperboat.operations WHERE id=$1`, op).Scan(&state); err != nil || state != "succeeded" {
			t.Fatalf("%s beside 120 waiting adds = %q, %v; want succeeded", op, state, err)
		}
	}
}
