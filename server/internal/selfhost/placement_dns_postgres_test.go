package selfhost

import (
	"context"
	"crypto/sha256"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/previewdomain"
	"github.com/pinksaucepasta/paperboat/server/internal/testutil"
	"github.com/pinksaucepasta/paperboat/server/internal/tunnelv1"
)

func TestPlacementDNSCreationAndResetOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := t.Context()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	suffix := randomIdentity()
	owner := "dns_owner_" + suffix
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, owner, owner+"@example.test")
	installation := scopedInstallationFixture(t, store, "dns-placement-test-key", owner, "tunnel")
	// The node's verified public ingress is the self-hosted DNS target the pool publishes (publicationdns.Target).
	exec(`UPDATE paperboat.control_tunnel_nodes SET endpoint_host='140.245.11.41',public_ingress_ipv4='140.245.11.41',public_ingress_verified_at=now() WHERE id=$1`, installation.NodeID)
	service := New(store, "dns-placement-test-key")
	// A placement on an installation needs a self-hosted-only workspace pool; a
	// managed placement needs a managed-only pool (placement.go SetPlacement).
	setPool := func(policy Policy) {
		t.Helper()
		if _, err := service.SetPool(ctx, owner, "tunnel", policy); err != nil {
			t.Fatal(err)
		}
	}
	selfHosted := Policy{Mode: "self-hosted-only", InstallationIDs: []string{installation.InstallationID}}
	managed := Policy{Mode: "managed-only"}
	t.Cleanup(func() {

		for _, item := range []struct{ query, id string }{
			{`DELETE FROM paperboat.selfhost_placements WHERE installation_id=$1`, installation.InstallationID},
			{`DELETE FROM paperboat.preview_domains WHERE account_id=$1`, owner},
			{`DELETE FROM paperboat.preview_leases WHERE account_id=$1`, owner},
			{`DELETE FROM paperboat.tunnels WHERE account_id=$1`, owner},
			{`DELETE FROM paperboat.operations WHERE account_id=$1`, owner},
			{`DELETE FROM paperboat.selfhost_installations WHERE id=$1`, installation.InstallationID},
			{`DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, installation.NodeID},
			{`DELETE FROM paperboat.users WHERE id=$1`, owner},
		} {
			if _, err := store.SQL().ExecContext(context.Background(), item.query, item.id); err != nil {
				t.Errorf("cleanup: %v", err)
			}
		}

	})
	previewRepo, err := previewdomain.NewSQLRepository(store, previewdomain.Config{})
	if err != nil {
		t.Fatal(err)
	}
	tunnelRepo, err := tunnelv1.NewRepository(store)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"preview", "tunnel"} {
		id := "dns_" + kind + "_" + suffix
		route := "dns_route_" + suffix
		endpointID := testutil.EndpointID(id)
		stable := testutil.EndpointLabel(endpointID) + ".example.test"
		// A mode change is refused while the owner has live publications, so the pool moves before the
		// resource exists and the resource is stopped before the pool moves back (ErrLiveEdgeModeChange).
		setPool(selfHosted)
		if kind == "preview" {
			exec(`INSERT INTO paperboat.preview_leases(owner_session_kind,id,endpoint,account_id,actor_id,owner_machine_id,owner_session_id,target_scheme,target_address,lease_deadline) VALUES('foreground',$1,$2,$3,$3,'machine','session','http','127.0.0.1:3000',now()+interval '10 minutes')`, id, "https://"+stable, owner)
		} else {
			exec(`INSERT INTO paperboat.tunnels(id,account_id,name,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id) VALUES($1,$2,$1,$3,$4,'host','actor')`, id, owner, endpointID, "https://"+stable)
			exec(`INSERT INTO paperboat.tunnel_routes(id,tunnel_id,name,protocol,match_type,match_hostname,origin_scheme,origin_address,created_by_actor_id,updated_by_actor_id) VALUES($1,$2,$1,'http','managed','managed.example.test','http','127.0.0.1:3000','actor','actor')`, route, id)
		}
		domainID := "dns_domain_" + kind + "_" + suffix
		hostname := kind + "-" + testutil.EndpointLabel(endpointID) + ".example.test"
		now := time.Now().UTC()
		hash := sha256.Sum256([]byte(domainID))
		if kind == "preview" {
			_, err = previewRepo.Create(ctx, previewdomain.CreateRecord{OperationID: "op_" + domainID, AuditEventID: "aud_" + domainID, AccountID: owner, PreviewID: id, DomainID: domainID, PreviewGeneration: 1, Hostname: hostname, MatchType: "exact", ChallengeReference: "dns:test", DNSTarget: stable, DNSProvider: "generic", ExpectedRecords: []byte("[]"), CertificateStrategy: "managed", IdempotencyKey: domainID, RequestHash: hash, ActorID: owner, ActorType: "user", CorrelationID: domainID, Now: now})
		} else {
			_, err = tunnelRepo.CreateResourceDomain(ctx, tunnelv1.DomainRecord{OperationID: "op_" + domainID, AuditEventID: "aud_" + domainID, AccountID: owner, TunnelID: id, DomainID: domainID, RouteID: route, Hostname: hostname, MatchType: "exact", ChallengeReference: "dns:test", DNSProvider: "generic", CertificateStrategy: "managed", IdempotencyKey: domainID, RequestHash: hash, ActorID: owner, ActorType: "user", CorrelationID: domainID, Now: now})
		}
		if err != nil {
			t.Fatal(err)
		}
		table := "paperboat." + kind + "_domains"
		check := func(target, state, typ string, generation int64) {
			t.Helper()
			var actualTarget, actualState, actualType string
			var actualGeneration int64
			if err := store.SQL().QueryRowContext(ctx, `SELECT dns_target,ownership_state,expected_records->0->>'type',generation FROM `+table+` WHERE id=$1`, domainID).Scan(&actualTarget, &actualState, &actualType, &actualGeneration); err != nil || actualTarget != target || actualState != state || actualType != typ || actualGeneration != generation {
				t.Fatalf("%s target=%q state=%q type=%q generation=%d: %v", kind, actualTarget, actualState, actualType, actualGeneration, err)
			}
		}
		check("140.245.11.41", "pending", "A", 1)
		// A self-hosted placement needs a verified custom domain; each placement change resets the domain to pending.
		exec(`UPDATE `+table+` SET ownership_state='verified' WHERE id=$1`, domainID)
		if _, err = service.SetPlacement(ctx, owner, kind, id, &installation.InstallationID); err != nil {
			t.Fatal(err)
		}
		check("140.245.11.41", "pending", "A", 2)
		exec(`UPDATE `+table+` SET ownership_state='verified',conflict_state='quarantined',quarantine_until=now()+interval '1 hour' WHERE id=$1`, domainID)
		if kind == "preview" {
			exec(`UPDATE paperboat.preview_leases SET terminal_state='stopped',stopped_at=now() WHERE id=$1`, id)
		} else {
			exec(`UPDATE paperboat.tunnels SET desired_state='closed' WHERE id=$1`, id)
		}
		setPool(managed)
		if _, err = service.SetPlacement(ctx, owner, kind, id, nil); err != nil {
			t.Fatal(err)
		}
		check(stable, "pending", "CNAME", 3)
		var quarantineCleared bool
		if err := store.SQL().QueryRowContext(ctx, `SELECT quarantine_until IS NULL FROM `+table+` WHERE id=$1`, domainID).Scan(&quarantineCleared); err != nil || !quarantineCleared {
			t.Fatal("placement retained quarantine")
		}
		result, err := store.SQL().ExecContext(ctx, `UPDATE `+table+` SET ownership_state='verified' WHERE id=$1 AND generation=2`, domainID)
		if err != nil {
			t.Fatal(err)
		}
		if n, _ := result.RowsAffected(); n != 0 {
			t.Fatal("stale DNS observation crossed generation fence")
		}
		exec(`UPDATE `+table+` SET ownership_state='verified' WHERE id=$1`, domainID)
		setPool(selfHosted)
		if _, err = service.SetPlacement(ctx, owner, kind, id, &installation.InstallationID); err != nil {
			t.Fatal(err)
		}
		check("140.245.11.41", "pending", "A", 4)
		if _, err = service.SetPlacement(ctx, owner, kind, id, &installation.InstallationID); err != nil {
			t.Fatal(err)
		}
		check("140.245.11.41", "pending", "A", 4)
	}
}
