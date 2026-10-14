package previewattachment

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat-server/internal/previewtunnelstore"
)

// This test is deliberately opt-in. It exercises the real SQL repository on
// an isolated PostgreSQL database and never starts a local edge or host.
func TestPreviewCarrierEdgeProcessEpochReplacementOnPostgres(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("PAPERBOAT_TEST_DATABASE_DSN"))
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN to run the preview carrier PostgreSQL test")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	if !safePreviewCarrierTestDSN(dsn) {
		t.Fatal("refusing to run preview carrier PostgreSQL test unless PAPERBOAT_TEST_DATABASE_DSN names a *_test database")
	}
	if productionDSN := strings.TrimSpace(os.Getenv("PAPERBOAT_DATABASE_DSN")); productionDSN != "" && samePreviewCarrierDatabase(dsn, productionDSN) {
		t.Fatal("refusing to run preview carrier PostgreSQL test against PAPERBOAT_DATABASE_DSN")
	}

	ctx := context.Background()
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, database); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	otherNode := "edge-carrier-other-" + suffix
	fixture := previewCarrierPostgresFixture{suffix: suffix}
	t.Cleanup(func() {
		// User deletion cascades the lease, operation, attachment, and outbox
		// rows. The edge node is independent and is deleted second because the
		// attachment FK deliberately uses ON DELETE RESTRICT.
		_, _ = database.SQL().ExecContext(ctx, `DELETE FROM paperboat.users WHERE id=$1`, fixture.accountID)
		_, _ = database.SQL().ExecContext(ctx, `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, fixture.nodeID)
		_, _ = database.SQL().ExecContext(ctx, `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, otherNode)
		_ = database.Close()
	})

	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := fixture.insert(ctx, database, now); err != nil {
		t.Fatal(err)
	}
	repository, err := NewSQLRepository(database)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.SetClock(func() time.Time { return now }); err != nil {
		t.Fatal(err)
	}

	first := fixture.attachment(1, fixture.epochOne)
	if _, err := repository.CreatePending(ctx, first); err != nil {
		t.Fatalf("create first attachment: %v", err)
	}
	released, err := repository.Release(ctx, first, now.Add(time.Second))
	if err != nil {
		t.Fatalf("release first attachment: %v", err)
	}
	if released.State != StateReleased || released.AttachmentGeneration != first.AttachmentGeneration+1 {
		t.Fatalf("released first attachment = %#v", released)
	}

	second := fixture.attachment(2, fixture.epochOne)
	if _, err := repository.CreatePending(ctx, second); err != nil {
		t.Fatalf("create second attachment: %v", err)
	}
	oldSnapshot, err := repository.PullPreviewCarrierSnapshot(ctx, fixture.nodeID, fixture.epochOne)
	if err != nil {
		t.Fatalf("initial epoch snapshot: %v", err)
	}
	if len(oldSnapshot) != 1 || oldSnapshot[0].OperationID != second.OperationID {
		t.Fatalf("initial snapshot = %#v, want only second operation", oldSnapshot)
	}

	// The node row is the source of truth for the process fence. Replacing the
	// process keeps the stable node ID but invalidates every old edge request.
	if _, err := database.SQL().ExecContext(ctx, `
UPDATE paperboat.control_tunnel_nodes
SET process_epoch=$2, state='ready', ready=true, last_heartbeat_at=$3, updated_at=$3
WHERE id=$1`, fixture.nodeID, fixture.epochTwo, now.Add(2*time.Second)); err != nil {
		t.Fatalf("replace edge process epoch: %v", err)
	}

	for name, call := range map[string]func() error{
		"old snapshot pull": func() error {
			_, err := repository.PullPreviewCarrierSnapshot(ctx, fixture.nodeID, fixture.epochOne)
			return err
		},
		"old detach pull and claim": func() error {
			_, err := repository.PullPreviewCarrierDetachOutbox(ctx, fixture.nodeID, fixture.epochOne)
			return err
		},
		"old detach ACK": func() error {
			return repository.AcknowledgePreviewCarrierOutbox(ctx, fixture.nodeID, fixture.epochOne, first.AccountID, first.OperationID, released.AttachmentGeneration, "detach")
		},
	} {
		if err := call(); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("%s error = %v, want ErrUnauthorized", name, err)
		}
	}

	currentDetach, err := repository.PullPreviewCarrierDetachOutbox(ctx, fixture.nodeID, fixture.epochTwo)
	if err != nil {
		t.Fatalf("current detach pull: %v", err)
	}
	if len(currentDetach) != 0 {
		t.Fatalf("current epoch received old detach commands = %#v", currentDetach)
	}
	if err := repository.AcknowledgePreviewCarrierOutbox(ctx, fixture.nodeID, fixture.epochTwo, first.AccountID, first.OperationID, released.AttachmentGeneration, "detach"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("current epoch old detach ACK error = %v, want ErrNotFound", err)
	}

	// Renewing through the real SQL CAS rebinds the live route to the current
	// process epoch without changing its stable tunnel/connector/route IDs.
	next := second
	next.Binding.EdgeProcessEpoch = fixture.epochTwo
	next.ExpiresAt = now.Add(20 * time.Minute)
	renewed, err := repository.Renew(ctx, second, next, now.Add(3*time.Second))
	if err != nil {
		t.Fatalf("renew to current edge epoch: %v", err)
	}
	if renewed.State != StatePending || renewed.AttachmentGeneration != second.AttachmentGeneration+1 || renewed.Binding.EdgeProcessEpoch != fixture.epochTwo {
		t.Fatalf("renewed attachment = %#v", renewed)
	}
	if renewed.TunnelID != second.TunnelID || renewed.ConnectorID != second.ConnectorID || renewed.RouteID != second.RouteID {
		t.Fatalf("renew changed stable carrier identity: before=%#v after=%#v", second.Binding, renewed.Binding)
	}

	currentSnapshot, err := repository.PullPreviewCarrierSnapshot(ctx, fixture.nodeID, fixture.epochTwo)
	if err != nil {
		t.Fatalf("current epoch snapshot: %v", err)
	}
	if len(currentSnapshot) != 1 || currentSnapshot[0].OperationID != second.OperationID || currentSnapshot[0].AttachmentGeneration != renewed.AttachmentGeneration {
		t.Fatalf("current snapshot = %#v, want renewed second operation", currentSnapshot)
	}
	if err := repository.AcknowledgePreviewCarrierOutbox(ctx, fixture.nodeID, fixture.epochTwo, renewed.AccountID, renewed.OperationID, renewed.AttachmentGeneration, "admit"); err != nil {
		t.Fatalf("current epoch admit ACK: %v", err)
	}
	if err := repository.AcknowledgePreviewCarrierOutbox(ctx, fixture.nodeID, fixture.epochTwo, renewed.AccountID, renewed.OperationID, renewed.AttachmentGeneration, "admit"); err != nil {
		t.Fatalf("current epoch lost-response admit ACK replay: %v", err)
	}
	admitted, err := repository.Get(ctx, renewed.AccountID, renewed.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if admitted.State != StateAdmitted || admitted.EdgeReady || admitted.Binding.EdgeProcessEpoch != fixture.epochTwo {
		t.Fatalf("admitted current attachment = %#v", admitted)
	}

	// The migration trigger is the final database-level guard. Even a direct
	// stale update cannot put an active attachment back under the old epoch.
	if _, err := database.SQL().ExecContext(ctx, `
UPDATE paperboat.preview_lease_carrier_attachments
SET edge_process_epoch=$1
WHERE account_id=$2 AND operation_id=$3`, fixture.epochOne, admitted.AccountID, admitted.OperationID); err == nil {
		t.Fatal("stale active attachment update unexpectedly succeeded")
	}

	// A confirmed active runtime move changes the edge-specific carrier IDs
	// without changing the preview URL or route identity.
	otherEpoch := "edge-process-other-" + suffix
	t.Cleanup(func() {
		_, _ = database.SQL().ExecContext(ctx, `DELETE FROM paperboat.runtime_carrier_attachments WHERE machine_id=$1`, fixture.machineID)
		_, _ = database.SQL().ExecContext(ctx, `DELETE FROM paperboat.control_routes WHERE environment_id=$1`, fixture.envID)
		_, _ = database.SQL().ExecContext(ctx, `DELETE FROM paperboat.control_environments WHERE id=$1`, fixture.envID)
	})
	for _, step := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id,desired_state) VALUES($1,$2,$3,'active')`, []any{fixture.envID, fixture.accountID, fixture.accountID}},
		{`INSERT INTO paperboat.control_routes(id,environment_id,kind,public_host,target_host,target_port,desired_revision,desired_state) VALUES($1,$2,'runtime_https_wss',$3,'127.0.0.1',38080,1,'attached')`, []any{"runtime-route-" + suffix, fixture.envID, fixture.machineID + ".runtime.example.test"}},
		{`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at,carrier_endpoint_host,carrier_endpoint_tcp_port,carrier_endpoint_quic_port,carrier_server_spki_sha256,carrier_server_certificate_chain_pem) VALUES($1,'test','1.0',$2,'ready',true,now(),'test','other',ARRAY['edge'],ARRAY['http3','http2'],100,0,now(),now()+interval '1 hour','edge.example.test',25001,25002,$3,$4)`, []any{otherNode, otherEpoch, "sha256:" + strings.Repeat("c", 64), "test-public-certificate-chain"}},
		{`INSERT INTO paperboat.machine_edge_placements(machine_id,account_id,installation_generation,worker_generation,primary_node_id,primary_process_epoch,secondary_node_id,secondary_process_epoch,observed_at) VALUES($1,$2,1,1,$3,$4,$5,$6,now())`, []any{fixture.machineID, fixture.accountID, otherNode, otherEpoch, fixture.nodeID, fixture.epochTwo}},
		{`INSERT INTO paperboat.runtime_carrier_attachments(route_id,machine_id,account_id,installation_generation,worker_generation,process_id,generation,edge_node_id,edge_process_epoch,public_identity_key,route_revision,session_id,preview_session_id,admission,expires_at,ready,observed_at,active) VALUES($1,$2,$3,1,1,'process',1,$4,$5,$6,1,'session_01234567-89ab-4cde-8fab-0123456789ab','session_11234567-89ab-4cde-8fab-0123456789ab','{}',now()+interval '1 hour',true,now(),true)`, []any{"runtime-route-" + suffix, fixture.machineID, fixture.accountID, otherNode, otherEpoch, fixture.publicKey}},
	} {
		if _, err := database.SQL().ExecContext(ctx, step.query, step.args...); err != nil {
			t.Fatal(err)
		}
	}
	moved := admitted
	moved.Binding.EdgeNodeID, moved.Binding.EdgeProcessEpoch = otherNode, otherEpoch
	moved.Binding.TunnelID, moved.Binding.ConnectorID, moved.Binding.SessionID = "pvc-tunnel-other-"+suffix, "pvc-connector-other-"+suffix, "pvc-session-other-"+suffix
	moved.Binding.EdgeCarrierServerSPKISHA256 = "sha256:" + strings.Repeat("c", 64)
	moved.ConfigContentHash = "sha256:" + strings.Repeat("d", 64)
	moved.State, moved.EdgeReady, moved.OriginReady, moved.ReadyAt = StatePending, false, false, nil
	moved.ExpiresAt = now.Add(25 * time.Minute)
	rebound, err := repository.Rebind(ctx, admitted, moved, now.Add(4*time.Second))
	if err != nil {
		t.Fatalf("rebind to confirmed active edge: %v", err)
	}
	if rebound.State != StatePending || rebound.AttachmentGeneration != admitted.AttachmentGeneration+1 || rebound.EdgeNodeID != otherNode || rebound.RouteID != admitted.RouteID || rebound.Endpoint != admitted.Endpoint {
		t.Fatalf("rebound attachment = %#v", rebound)
	}
	if _, err := repository.ObserveEdge(ctx, admitted, now.Add(5*time.Second)); !errors.Is(err, ErrStaleBinding) {
		t.Fatalf("old edge observation = %v, want stale binding", err)
	}
	if err := repository.SetClock(func() time.Time { return now.Add(5 * time.Second) }); err != nil {
		t.Fatal(err)
	}
	detach, err := repository.PullPreviewCarrierDetachOutbox(ctx, fixture.nodeID, fixture.epochTwo)
	if err != nil || len(detach) != 1 || detach[0].Binding.EdgeNodeID != fixture.nodeID || detach[0].AttachmentGeneration != rebound.AttachmentGeneration {
		t.Fatalf("old edge detach = %#v, error %v", detach, err)
	}
	newSnapshot, err := repository.PullPreviewCarrierSnapshot(ctx, otherNode, otherEpoch)
	if err != nil || len(newSnapshot) != 1 || newSnapshot[0].AttachmentGeneration != rebound.AttachmentGeneration {
		t.Fatalf("new edge snapshot = %#v, error %v", newSnapshot, err)
	}
	installation := "installation_carrier_alias_" + suffix
	identity := sha256.Sum256([]byte(installation))
	t.Cleanup(func() {
		for _, query := range []string{`DELETE FROM paperboat.selfhost_pool_policies WHERE scope_kind='account' AND scope_id=$1`, `DELETE FROM paperboat.selfhost_installations WHERE scope_kind='account' AND scope_id=$1`} {
			if _, err := database.SQL().ExecContext(context.Background(), query, fixture.accountID); err != nil {
				t.Error(err)
			}
		}
	})
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := database.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE paperboat.control_tunnel_nodes SET tls_spki_sha256=$2,region='test',failure_domain='alias',roles=ARRAY['edge'],transports=ARRAY['http2','http3'],capacity_limit=8,capacity_used=0,capacity_observed_at=now(),registry_expires_at=now()+interval '1 hour',last_heartbeat_at=now(),public_ingress_ipv4='140.245.11.41',public_ingress_verified_at=now() WHERE id=$1`, otherNode, identity[:])
	exec(`INSERT INTO paperboat.selfhost_installations(id,node_id,installation_public_key,runtime_hash,runtime_ciphertext,name,capability,scope_kind,scope_id,claim_id,claim_complete) VALUES($1,$2,$3,$3,$3,'Snapshot alias fixture','tunnel','account',$4,$1,true)`, installation, otherNode, identity[:], fixture.accountID)
	exec(`INSERT INTO paperboat.selfhost_pool_policies(scope_kind,scope_id,capability,mode,installation_ids) VALUES('account',$1,'tunnel','self-hosted-only',ARRAY[$2])`, fixture.accountID, installation)
	customerHostname := "carrier-alias-" + suffix + ".example.test"
	exec(`INSERT INTO paperboat.preview_domains(id,account_id,preview_id,preview_generation,hostname,match_type,ownership_challenge_reference,ownership_state,dns_target,expected_records,certificate_strategy,certificate_state,certificate_reference,caa_state,conflict_state) SELECT $1,account_id,id,generation,$3,'exact','dns:snapshot','verified','140.245.11.41','[{"type":"A","value":"140.245.11.41"}]'::jsonb,'managed','ready','certificate:snapshot','ready','clear' FROM paperboat.preview_leases WHERE id=$2`, "domain_carrier_alias_"+suffix, rebound.PreviewID, customerHostname)
	aliasSnapshot, err := repository.PullPreviewCarrierSnapshot(ctx, otherNode, otherEpoch)
	if err != nil || len(aliasSnapshot) != 1 || aliasSnapshot[0].Endpoint != "https://"+customerHostname {
		t.Fatalf("selfhost snapshot endpoint = %+v %v", aliasSnapshot, err)
	}
	admission, err := carrierAdmissionFromOutbox(aliasSnapshot[0], now.Add(5*time.Second))
	if err != nil || admission.Hostname != customerHostname {
		t.Fatalf("selfhost signed admission hostname = %+v %v", admission, err)
	}
	persisted, err := repository.Get(ctx, rebound.AccountID, rebound.OperationID)
	if err != nil || persisted.Endpoint != rebound.Endpoint {
		t.Fatalf("snapshot overwrote internal endpoint identity: %+v %v", persisted, err)
	}
	exec(`UPDATE paperboat.preview_domains SET certificate_state='issuing',certificate_reference=NULL WHERE id=$1`, "domain_carrier_alias_"+suffix)
	aliasSnapshot, err = repository.PullPreviewCarrierSnapshot(ctx, otherNode, otherEpoch)
	if err != nil || len(aliasSnapshot) != 0 {
		t.Fatalf("not-ready alias snapshot = %+v %v", aliasSnapshot, err)
	}
	// A pending authenticated attachment may receive its certificate before
	// the browser publication is ready; otherwise issuance and admission deadlock.
	stagingTargets, err := database.Queries().ListReadyPreviewCertificateEdgesV1(ctx, dbsqlc.ListReadyPreviewCertificateEdgesV1Params{DomainID: "domain_carrier_alias_" + suffix, Now: now.Add(5 * time.Second)})
	if err != nil || len(stagingTargets) != 1 || stagingTargets[0].EdgeNodeID != otherNode || stagingTargets[0].EdgeProcessEpoch != otherEpoch || uint64(stagingTargets[0].AttachmentGeneration) != rebound.AttachmentGeneration {
		t.Fatalf("pre-certificate staging authority = %+v %v", stagingTargets, err)
	}
	lease, err := database.Queries().GetPreviewLeaseForReconciliationV1(ctx, rebound.PreviewID)
	if err != nil {
		t.Fatal(err)
	}
	store, err := previewtunnelstore.New(database)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("certificate-renew-" + suffix))
	renewedLease, err := store.RenewPreviewLeaseV1(ctx, previewtunnelstore.RenewPreviewLeaseV1Input{OperationID: "op_cert_renew_" + suffix, AuditEventID: "audit_cert_renew_" + suffix, AccountID: fixture.accountID, ActorID: fixture.accountID, ActorType: "user", PreviewID: rebound.PreviewID, OwnerMachineID: fixture.machineID, OwnerSessionID: lease.OwnerSessionID, ExpectedGeneration: lease.Generation, LeaseDeadline: now.Add(30 * time.Minute), RequestHash: hash[:], IdempotencyKey: "cert-renew-" + suffix, CorrelationID: "cor_cert_renew_" + suffix, RequestID: "req_cert_renew_" + suffix, SourceMachineID: fixture.machineID, Now: now.Add(6 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	assertTargets := func(want int) {
		t.Helper()
		targets, e := database.Queries().ListReadyPreviewCertificateEdgesV1(ctx, dbsqlc.ListReadyPreviewCertificateEdgesV1Params{DomainID: "domain_carrier_alias_" + suffix, Now: now.Add(7 * time.Second)})
		if e != nil || len(targets) != want {
			t.Fatalf("renewal staging targets=%+v err=%v want=%d", targets, e, want)
		}
	}
	assertTargets(1)
	exec(`UPDATE paperboat.preview_lease_carrier_attachments SET lease_generation=$2 WHERE account_id=$1 AND operation_id=$3`, fixture.accountID, renewedLease.Lease.Generation+1, rebound.OperationID)
	assertTargets(0)
	exec(`UPDATE paperboat.preview_lease_carrier_attachments SET lease_generation=$2 WHERE account_id=$1 AND operation_id=$3`, fixture.accountID, rebound.LeaseGeneration, rebound.OperationID)
	_, ownerErr := database.SQL().ExecContext(ctx, `UPDATE paperboat.preview_leases SET owner_session_id='other-owner' WHERE id=$1`, rebound.PreviewID)
	var ownerConstraint *pgconn.PgError
	if !errors.As(ownerErr, &ownerConstraint) || ownerConstraint.Code != "23514" {
		t.Fatalf("immutable preview owner mutation = %v", ownerErr)
	}
	_, attachmentOwnerErr := database.SQL().ExecContext(ctx, `UPDATE paperboat.preview_lease_carrier_attachments SET owner_session_id='other-owner' WHERE account_id=$1 AND operation_id=$2`, fixture.accountID, rebound.OperationID)
	var attachmentConstraint *pgconn.PgError
	if !errors.As(attachmentOwnerErr, &attachmentConstraint) || attachmentConstraint.Code != "P0001" {
		t.Fatalf("mismatched attachment owner mutation = %v", attachmentOwnerErr)
	}
	assertTargets(1)
	exec(`UPDATE paperboat.preview_domains SET ownership_state='pending' WHERE id=$1`, "domain_carrier_alias_"+suffix)
	stagingTargets, err = database.Queries().ListReadyPreviewCertificateEdgesV1(ctx, dbsqlc.ListReadyPreviewCertificateEdgesV1Params{DomainID: "domain_carrier_alias_" + suffix, Now: now.Add(5 * time.Second)})
	if err != nil || len(stagingTargets) != 0 {
		t.Fatalf("unverified DNS staging authority = %+v %v", stagingTargets, err)
	}

}

type previewCarrierPostgresFixture struct {
	suffix           string
	accountID        string
	machineID        string
	envID            string
	nodeID           string
	previewID        [2]string
	operation        [2]string
	sessionID        string
	ownerSessionKind string
	lazyPolicyID     string
	lazyHostname     string
	publicKey        string
	thumbprint       string
	epochOne         string
	epochTwo         string
}

func (f *previewCarrierPostgresFixture) initialize() {
	f.accountID = "usr_carrier_epoch_" + f.suffix
	f.machineID = "mch_carrier_epoch_" + f.suffix
	f.envID = "env_carrier_epoch_" + f.suffix
	f.nodeID = "edge-carrier-epoch-" + f.suffix
	f.previewID = [2]string{"prv_carrier_epoch_a_" + f.suffix, "prv_carrier_epoch_b_" + f.suffix}
	f.operation = [2]string{"op_carrier_epoch_a_" + f.suffix, "op_carrier_epoch_b_" + f.suffix}
	if f.sessionID == "" {
		f.sessionID = "owner-session-carrier-epoch-" + f.suffix
	}
	if f.ownerSessionKind == "" {
		f.ownerSessionKind = "foreground"
	}
	f.epochOne = "edge-process-one-" + f.suffix
	f.epochTwo = "edge-process-two-" + f.suffix
	key := sha256.Sum256([]byte("carrier-machine-key:" + f.suffix))
	f.publicKey = base64.RawURLEncoding.EncodeToString(key[:])
	thumbprint := sha256.Sum256(key[:])
	f.thumbprint = "sha256:" + base64.RawURLEncoding.EncodeToString(thumbprint[:])
}

func (f *previewCarrierPostgresFixture) insert(ctx context.Context, database *db.DB, now time.Time) error {
	f.initialize()
	if _, err := database.SQL().ExecContext(ctx, `
INSERT INTO paperboat.users (id, workos_subject, primary_email, status)
VALUES ($1, $2, $3, 'active')`, f.accountID, "workos_"+f.suffix, "carrier-epoch-"+f.suffix+"@example.test"); err != nil {
		return err
	}
	if _, err := database.SQL().ExecContext(ctx, `
INSERT INTO paperboat.user_machines
  (id, user_id, environment_id, description, platform, architecture, workspace_root,
   state, seat_state, online, public_identity_key, installation_generation,
   worker_generation, worker_service_scope, connector_state)
VALUES ($1, $2, $3, $4, 'linux', 'amd64', '/workspace', 'online', 'occupied', true,
        $5, 1, 1, 'system', 'ready')`, f.machineID, f.accountID, f.envID, "Carrier epoch "+f.suffix, f.publicKey); err != nil {
		return err
	}
	if _, err := database.SQL().ExecContext(ctx, `
INSERT INTO paperboat.control_tunnel_nodes
  (id, edge_pool, protocol_version, process_epoch, endpoint_host, endpoint_tcp_port,
   endpoint_quic_port, carrier_endpoint_host,
   carrier_endpoint_tcp_port, carrier_endpoint_quic_port, carrier_server_spki_sha256, carrier_server_certificate_chain_pem, state, ready, capacity, last_heartbeat_at)
VALUES ($1, 'test', '1.0', $2, 'edge.example.test', 24001, 24002,
        'edge.example.test', 25001, 25002,
        'sha256:' || repeat('b',64), 'test-public-certificate-chain', 'ready', true, '{}', $3)`, f.nodeID, f.epochOne, now); err != nil {
		return err
	}
	for index := range f.previewID {
		// One machine/port lazy policy has one active publication. Foreground
		// cases retain independent previews for their lifecycle scenarios.
		if f.ownerSessionKind == "lazy_runtime" && index > 0 {
			break
		}
		previewEndpoint := fmt.Sprintf("https://carrier-epoch-%s-%c.preview.example.test", f.suffix, 'a'+rune(index))
		if f.ownerSessionKind == "lazy_runtime" {
			if f.lazyHostname == "" {
				if err := database.SQL().QueryRowContext(ctx, `SELECT paperboat.reserve_machine_hostname($1,3000,$2)`, f.machineID, "calm-coastal-harbor-"+f.suffix+".preview.example.test").Scan(&f.lazyHostname); err != nil {
					return err
				}
				if _, err := database.SQL().ExecContext(ctx, `INSERT INTO lazy_access_policies(id,hostname,account_id,environment_id,machine_id,installation_generation,generation,target_scheme,target_address,access_mode,ownership_mode,expires_at) VALUES($1,$2,$3,$4,$5,1,1,'http','127.0.0.1:3000','private','persistent_port',$6)`, f.lazyPolicyID, f.lazyHostname, f.accountID, f.envID, f.machineID, now.Add(time.Hour)); err != nil {
					return err
				}
			}
			previewEndpoint = "https://" + f.lazyHostname
		}
		if _, err := database.SQL().ExecContext(ctx, `
INSERT INTO paperboat.preview_leases
  (owner_session_kind,id, endpoint, account_id, actor_id, owner_machine_id, owner_session_id,
   target_scheme, target_address, access_mode, lease_deadline, allocation_state,
   edge_state, origin_state, terminal_state, created_at, last_renewed_at)
VALUES ($8,$1, $2, $3, $3, $4, $5, 'http', '127.0.0.1:3000', 'public',
        $6, 'pending', 'pending', 'unknown', 'active', $7, $7)`, f.previewID[index], previewEndpoint, f.accountID, f.machineID, f.sessionID, now.Add(30*time.Minute), now, f.ownerSessionKind); err != nil {
			return err
		}
		requestHash := sha256.Sum256([]byte("carrier-epoch-operation:" + f.operation[index]))
		if _, err := database.SQL().ExecContext(ctx, `
INSERT INTO paperboat.operations
  (id, account_id, idempotency_key, request_hash, operation_type, resource_kind,
   resource_id, phase, state, progress, retrying, outcome, correlation_id,
   created_at, updated_at)
VALUES ($1, $2, $1, $3, 'preview.create', 'preview_lease', $4,
        'connecting', 'running', 60, false, 'changed', $5, $6, $6)`, f.operation[index], f.accountID, requestHash[:], f.previewID[index], "cor_"+f.operation[index], now); err != nil {
			return err
		}
		if _, err := database.SQL().ExecContext(ctx, `
INSERT INTO paperboat.preview_lease_create_operations (account_id, preview_id, operation_id)
VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, f.accountID, f.previewID[index], f.operation[index]); err != nil {
			return err
		}
	}
	return nil
}

func (f previewCarrierPostgresFixture) attachment(index int, processEpoch string) Attachment {
	operationID := f.operation[index-1]
	request := Request{
		PreviewID: f.previewID[index-1], OperationID: operationID,
		OwnerMachineID: f.machineID, OwnerSessionID: f.sessionID,
		IdempotencyKey: operationID, RequestID: "req_" + operationID,
		CorrelationID: "cor_" + operationID,
	}
	hash, err := request.Hash(f.accountID)
	if err != nil {
		panic(err)
	}
	return Attachment{
		Schema: Schema, Kind: Kind,
		Binding: Binding{
			AccountID: f.accountID, PreviewID: request.PreviewID, OperationID: operationID,
			OwnerMachineID: f.machineID, OwnerSessionID: f.sessionID, HostID: f.machineID,
			LeaseGeneration: 1, TunnelID: f.machineID,
			ConnectorID: f.machineID, SessionID: "session_01234567-89ab-4cde-8fab-0123456789ac",
			ProcessGeneration: 1, ConfigGeneration: 1, RouteID: request.PreviewID,
			RouteGeneration: 1, EdgeNodeID: f.nodeID, EdgeProcessEpoch: processEpoch,
			EdgeCarrierServerSPKISHA256: "sha256:" + strings.Repeat("b", 64), EdgeCarrierServerCertificateChainPEM: "test-public-certificate-chain",
			MachineIdentityPublicKey: f.publicKey, MachineIdentityThumbprint: f.thumbprint,
		},
		IdempotencyKey: request.IdempotencyKey, RequestID: request.RequestID,
		CorrelationID: request.CorrelationID, RequestHash: hash,
		Endpoint: fmt.Sprintf("https://carrier-epoch-%s-%c.preview.example.test", f.suffix, 'a'+rune(index-1)),
		Target:   Target{Scheme: "http", Address: "127.0.0.1:3000"}, AccessMode: "public",
		ConfigContentHash:    "sha256:" + strings.Repeat("a", 64),
		EdgeEndpoints:        []string{"h2://edge.example.test:25001", "h3://edge.example.test:25002"},
		AttachmentGeneration: 1, IssuedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(20 * time.Minute), State: StatePending,
	}
}

func safePreviewCarrierTestDSN(dsn string) bool {
	u, err := url.Parse(dsn)
	if err != nil {
		return false
	}
	database := strings.Trim(strings.TrimSpace(u.Path), "/")
	return strings.HasSuffix(strings.ToLower(database), "_test")
}

func samePreviewCarrierDatabase(left, right string) bool {
	l, lerr := url.Parse(left)
	r, rerr := url.Parse(right)
	return lerr == nil && rerr == nil && strings.EqualFold(l.Host, r.Host) && strings.EqualFold(strings.Trim(l.Path, "/"), strings.Trim(r.Path, "/"))
}
