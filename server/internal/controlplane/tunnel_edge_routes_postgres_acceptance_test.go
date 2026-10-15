package controlplane

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/browseraccess"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat-server/internal/testutil"
	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

// TestTunnelEdgeRouteAssignmentsOnPostgres is deliberately opt-in. It uses
// only a caller-provided isolated PostgreSQL database and never starts Docker,
// a server, an edge, or a host runtime.
func TestTunnelEdgeRouteAssignmentsOnPostgres(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("PAPERBOAT_TEST_DATABASE_DSN"))
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN to run TRK-15 PostgreSQL acceptance")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	if !safeTunnelEdgeAcceptanceDSN(dsn) {
		t.Fatal("PAPERBOAT_TEST_DATABASE_DSN must name an isolated *_test database")
	}
	if production := strings.TrimSpace(os.Getenv("PAPERBOAT_DATABASE_DSN")); production != "" && sameTunnelEdgeAcceptanceDatabase(dsn, production) {
		t.Fatal("refusing to run TRK-15 acceptance against PAPERBOAT_DATABASE_DSN")
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

	now := time.Now().UTC().Truncate(time.Microsecond)
	fixture := newTunnelEdgeAcceptanceFixture(fmt.Sprintf("%d", time.Now().UnixNano()), now)
	t.Cleanup(func() {
		_, _ = database.SQL().ExecContext(ctx, `DELETE FROM paperboat.users WHERE id=$1`, fixture.accountID)
		_, _ = database.SQL().ExecContext(ctx, `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, fixture.nodeID)
		_ = database.Close()
	})
	fixture.insertBase(t, database)
	fixture.insertConnector(t, database, fixture.connectorOne, fixture.hostOne, fixture.sessionOne, 11)
	fixture.insertConnector(t, database, fixture.connectorTwo, fixture.hostTwo, fixture.sessionTwo, 22)
	if _, err := database.SQL().ExecContext(ctx, `UPDATE paperboat.tunnel_config_generations SET activation_state='pending', activated_at=NULL WHERE tunnel_id=$1`, fixture.tunnelID); err != nil {
		t.Fatal(err)
	}
	// The connector has authenticated and ACKed the exact snapshot, but the
	// carrier cannot report readiness until this staged assignment is visible
	// at the edge. This is the bootstrap state that prevents the assignment /
	// carrier readiness cycle from deadlocking.
	fixture.markConnectorPreReady(t, database, fixture.connectorOne, fixture.sessionOne)
	fixture.markConnectorPreReady(t, database, fixture.connectorTwo, fixture.sessionTwo)

	service := &EdgeService{store: database, clock: func() time.Time { return now }}
	if staged, err := service.ReconcileTunnelEdgeRouteAssignments(ctx, 10); err != nil || staged != 2 {
		t.Fatalf("initial reconcile = %d, %v; want two staged replicas", staged, err)
	}
	first := fixture.loadAssignment(t, database, 1)
	second := fixture.loadAssignment(t, database, 2)
	byConnector := map[string]tunnelEdgeAcceptanceRow{first.connectorID: first, second.connectorID: second}
	first = byConnector[fixture.connectorOne]
	second = byConnector[fixture.connectorTwo]
	assertTunnelEdgeAssignmentBinding(t, first, fixture, fixture.connectorOne, fixture.hostOne, fixture.sessionOne, 11, "staged", "pending")
	assertTunnelEdgeAssignmentBinding(t, second, fixture, fixture.connectorTwo, fixture.hostTwo, fixture.sessionTwo, 22, "staged", "pending")
	rows, err := service.ListTunnelEdgeRouteAssignmentsForNodeV1(ctx, fixture.nodeID, fixture.epoch)
	if err != nil || len(rows) != 2 {
		t.Fatalf("pending-config staged edge snapshot = %d, %v; want two bootstrap admissions", len(rows), err)
	}

	if staged, err := service.ReconcileTunnelEdgeRouteAssignments(ctx, 10); err != nil || staged != 0 {
		t.Fatalf("replay reconcile = %d, %v; want deterministic no-op", staged, err)
	}
	replayedFirst := fixture.loadAssignment(t, database, first.generation)
	replayedSecond := fixture.loadAssignment(t, database, second.generation)
	if replayedFirst.assignmentID != first.assignmentID || !replayedFirst.assignedAt.Equal(first.assignedAt) || replayedSecond.assignmentID != second.assignmentID || !replayedSecond.assignedAt.Equal(second.assignedAt) {
		t.Fatalf("replay changed replica identity/assigned_at: before=%+v/%+v after=%+v/%+v", first, second, replayedFirst, replayedSecond)
	}
	if _, err := database.SQL().ExecContext(ctx, `UPDATE paperboat.tunnel_config_generations SET activation_state='active', activated_at=$2 WHERE tunnel_id=$1`, fixture.tunnelID, now); err != nil {
		t.Fatal(err)
	}
	fixture.markConnectorReady(t, database, fixture.connectorOne, fixture.sessionOne)
	fixture.markConnectorReady(t, database, fixture.connectorTwo, fixture.sessionTwo)

	observation := fixture.observation(first, "ready")
	for name, mutate := range map[string]func(*RouteObservation){
		"assignment generation": func(v *RouteObservation) { v.AssignmentGeneration++ },
		"route revision":        func(v *RouteObservation) { v.RouteRevision++ },
		"host":                  func(v *RouteObservation) { v.HostID = fixture.hostTwo },
		"connector":             func(v *RouteObservation) { v.ConnectorID = fixture.connectorTwo },
		"connector generation":  func(v *RouteObservation) { v.ConnectorGeneration++ },
		"session":               func(v *RouteObservation) { v.ConnectorSessionID = fixture.sessionTwo },
		"process generation":    func(v *RouteObservation) { v.ConnectorProcessGeneration++ },
		"config generation":     func(v *RouteObservation) { v.ConfigGeneration++ },
		"config hash":           func(v *RouteObservation) { v.ConfigContentHash = "sha256:" + strings.Repeat("00", sha256.Size) },
		"access mode":           func(v *RouteObservation) { v.AccessMode = "private" },
		"viewer policy generation": func(v *RouteObservation) {
			v.ViewerPolicyGeneration++
		},
		"edge node":          func(v *RouteObservation) { v.EdgeNodeID = fixture.nodeID + "-stale" },
		"edge process epoch": func(v *RouteObservation) { v.EdgeProcessEpoch = fixture.epoch + "-stale" },
	} {
		t.Run("reject stale "+name, func(t *testing.T) {
			stale := observation
			mutate(&stale)
			err := service.ObserveRoutes(ctx, stale.EdgeNodeID, []RouteObservation{stale})
			if !errors.Is(err, ErrAssignmentConflict) && !errors.Is(err, ErrInvalidUsageReport) {
				t.Fatalf("error = %v, want assignment rejection", err)
			}
		})
	}
	if _, err := database.SQL().ExecContext(ctx, `UPDATE paperboat.control_tunnel_nodes SET last_heartbeat_at=$2 WHERE id=$1`, fixture.nodeID, now.Add(-3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := service.ObserveRoutes(ctx, fixture.nodeID, []RouteObservation{observation}); !errors.Is(err, ErrAssignmentConflict) {
		t.Fatalf("stale heartbeat observation error = %v, want ErrAssignmentConflict", err)
	}
	if _, err := database.SQL().ExecContext(ctx, `UPDATE paperboat.control_tunnel_nodes SET last_heartbeat_at=$2 WHERE id=$1`, fixture.nodeID, now); err != nil {
		t.Fatal(err)
	}
	if err := service.ObserveRoutes(ctx, fixture.nodeID, []RouteObservation{observation}); err != nil {
		t.Fatalf("activate first assignment: %v", err)
	}
	if err := service.ObserveRoutes(ctx, fixture.nodeID, []RouteObservation{fixture.observation(second, "ready")}); err != nil {
		t.Fatalf("activate second replica: %v", err)
	}

	replacementSession := "ses_trk16_replacement_" + fixture.suffix
	fixture.replaceConnectorSession(t, database, fixture.connectorOne, replacementSession, 33)
	if staged, err := service.ReconcileTunnelEdgeRouteAssignments(ctx, 10); err != nil || staged != 1 {
		t.Fatalf("same-connector replacement reconcile = %d, %v; want one staged assignment", staged, err)
	}
	replacement := fixture.loadAssignment(t, database, 3)
	assertTunnelEdgeAssignmentBinding(t, replacement, fixture, fixture.connectorOne, fixture.hostOne, replacementSession, 33, "staged", "pending")
	if err := service.ObserveRoutes(ctx, fixture.nodeID, []RouteObservation{fixture.observation(replacement, "ready")}); err != nil {
		t.Fatalf("activate replacement: %v", err)
	}

	restarted := &EdgeService{store: database, clock: func() time.Time { return now }}
	rows, err = restarted.ListTunnelEdgeRouteAssignmentsForNodeV1(ctx, fixture.nodeID, fixture.epoch)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[0].AssignmentGeneration != 1 || rows[0].State != "draining" || rows[1].AssignmentGeneration != 2 || rows[1].State != "active" || rows[2].AssignmentGeneration != 3 || rows[2].State != "active" {
		t.Fatalf("restart snapshot = %#v, want old connector-one draining with connector-two and replacement active", rows)
	}

	detached := fixture.observation(first, "detached")
	if err := restarted.ObserveRoutes(ctx, fixture.nodeID, []RouteObservation{detached}); err != nil {
		t.Fatalf("detach old assignment: %v", err)
	}
	if err := restarted.ObserveRoutes(ctx, fixture.nodeID, []RouteObservation{detached}); err != nil {
		t.Fatalf("idempotent detach replay: %v", err)
	}
	var state string
	var releasedAt sql.NullTime
	if err := database.SQL().QueryRowContext(ctx, `SELECT state,released_at FROM paperboat.tunnel_edge_route_assignments WHERE assignment_id=$1`, first.assignmentID).Scan(&state, &releasedAt); err != nil || state != "detached" || !releasedAt.Valid {
		t.Fatalf("detached state = %q/%v, error %v", state, releasedAt.Valid, err)
	}
}

func TestMeasuredMachinePlacementOrdersTunnelEdgesOnPostgres(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("PAPERBOAT_TEST_DATABASE_DSN"))
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	if err = db.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	f := newTunnelEdgeAcceptanceFixture(fmt.Sprintf("placement-%d", time.Now().UnixNano()), now)
	defer func() {
		_, _ = database.SQL().ExecContext(ctx, `DELETE FROM paperboat.users WHERE id=$1`, f.accountID)
		for _, node := range []string{f.nodeID, f.nodeID + "-m", f.nodeID + "-z"} {
			_, _ = database.SQL().ExecContext(ctx, `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, node)
		}
	}()
	f.insertBase(t, database)
	f.insertConnector(t, database, f.connectorOne, f.hostOne, f.sessionOne, 11)
	if _, err = database.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET installation_generation=1,worker_generation=1 WHERE id=$1`, f.hostOne); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []struct{ id, epoch, domain string }{{f.nodeID + "-m", "epoch-m", "domain-m"}, {f.nodeID + "-z", "epoch-z", "domain-z"}} {
		if _, err = database.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at)
VALUES($1,'trk15-zone-a','1.0',$2,'ready',true,$4,'region-b',$3,ARRAY['edge'],ARRAY['http3','http2'],100,0,$4,$4::timestamptz+interval '1 hour')`, entry.id, entry.epoch, entry.domain, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = database.SQL().ExecContext(ctx, `INSERT INTO paperboat.machine_edge_placements(machine_id,account_id,installation_generation,worker_generation,primary_node_id,primary_process_epoch,secondary_node_id,secondary_process_epoch,observed_at)
SELECT id,user_id,installation_generation,worker_generation,$2,'epoch-z',$3,'epoch-m',$4 FROM paperboat.user_machines WHERE id=$1`, f.hostOne, f.nodeID+"-z", f.nodeID+"-m", now); err != nil {
		t.Fatal(err)
	}
	rows, err := database.Queries().ListReadyTunnelEdgeRouteCandidatesV1(ctx, dbsqlc.ListReadyTunnelEdgeRouteCandidatesV1Params{Now: now, RowLimit: 10})
	if err != nil {
		t.Fatal(err)
	}
	selected := map[string]bool{}
	for _, row := range rows {
		if row.RouteID == f.routeID && row.HostID == f.hostOne {
			selected[row.EdgeNodeID] = true
		}
	}
	if len(selected) != 2 || !selected[f.nodeID+"-z"] || !selected[f.nodeID+"-m"] {
		t.Fatalf("tunnel edges ignored measured placement: %v", selected)
	}
}

func TestTunnelViewerPolicyPublicToRestrictedPostgres(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("PAPERBOAT_TEST_DATABASE_DSN"))
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN to run tunnel viewer-policy transition acceptance")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	if !safeTunnelEdgeAcceptanceDSN(dsn) {
		t.Fatal("PAPERBOAT_TEST_DATABASE_DSN must name an isolated *_test database")
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
	now := time.Now().UTC().Truncate(time.Microsecond)
	fixture := newTunnelEdgeAcceptanceFixture(fmt.Sprintf("viewer-policy-%d", time.Now().UnixNano()), now)
	t.Cleanup(func() {
		_, _ = database.SQL().ExecContext(ctx, `DELETE FROM paperboat.users WHERE id=$1`, fixture.accountID)
		_, _ = database.SQL().ExecContext(ctx, `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, fixture.nodeID)
		_ = database.Close()
	})
	fixture.insertBase(t, database)
	fixture.insertConnector(t, database, fixture.connectorOne, fixture.hostOne, fixture.sessionOne, 11)
	fixture.insertConnector(t, database, fixture.connectorTwo, fixture.hostTwo, fixture.sessionTwo, 22)
	service := &EdgeService{store: database, clock: func() time.Time { return now }}
	if staged, err := service.ReconcileTunnelEdgeRouteAssignments(ctx, 10); err != nil || staged != 2 {
		t.Fatalf("initial public reconcile = %d, %v; want two staged assignments", staged, err)
	}
	first, second := fixture.loadAssignment(t, database, 1), fixture.loadAssignment(t, database, 2)
	if first.accessMode != "public" || second.accessMode != "public" || first.viewerPolicyGeneration != 1 || second.viewerPolicyGeneration != 1 {
		t.Fatalf("default assignments did not bind native-public authority: %+v / %+v", first, second)
	}
	if err := service.ObserveRoutes(ctx, fixture.nodeID, []RouteObservation{fixture.observation(first, "ready")}); err != nil {
		t.Fatalf("ready default-public assignment: %v", err)
	}
	if err := service.ObserveRoutes(ctx, fixture.nodeID, []RouteObservation{fixture.observation(second, "ready")}); err != nil {
		t.Fatalf("ready second default-public assignment: %v", err)
	}

	if _, err := database.SQL().ExecContext(ctx, `INSERT INTO paperboat.browser_viewer_policies(resource_kind,resource_id,route_id,owner_account_id,audience,allowed_emails,generation,updated_by_account,created_at,updated_at) VALUES('tunnel',$1,$2,$3,'owner','{}',1,$3,$4,$4)`, fixture.tunnelID, fixture.routeID, fixture.accountID, now); err != nil {
		t.Fatal(err)
	}
	viewerPolicies, err := browseraccess.NewService(database, "https://login.example.test")
	if err != nil {
		t.Fatal(err)
	}
	policyState, err := viewerPolicies.GetViewerPolicy(ctx, fixture.accountID, "tunnel", fixture.tunnelID, fixture.routeID)
	if err != nil || policyState.ServingState != "fencing" {
		t.Fatalf("policy state before edge fence = %#v, %v; want fencing", policyState, err)
	}
	// The projection stops matching the old public assignment as soon as the
	// policy transaction commits, before the worker stages its replacement.
	var servingPublicRows int
	if err := database.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.tunnel_edge_route_assignments a JOIN paperboat.current_tunnel_route_viewer_policy p ON p.route_id=a.route_id WHERE a.route_id=$1 AND a.state='active' AND a.observed_state='ready' AND p.effective_access_mode='public' AND a.access_mode=p.effective_access_mode AND a.viewer_policy_generation=p.viewer_policy_generation`, fixture.routeID).Scan(&servingPublicRows); err != nil {
		t.Fatal(err)
	}
	if servingPublicRows != 0 {
		t.Fatalf("stale public assignment remained in current ingress projection: %d", servingPublicRows)
	}
	if staged, err := service.ReconcileTunnelEdgeRouteAssignments(ctx, 10); err != nil || staged != 2 {
		t.Fatalf("restricted reconcile = %d, %v; want two immutable replacements", staged, err)
	}
	newFirst, newSecond := fixture.loadAssignment(t, database, 3), fixture.loadAssignment(t, database, 4)
	for _, row := range []tunnelEdgeAcceptanceRow{newFirst, newSecond} {
		if row.accessMode != "private" || row.viewerPolicyGeneration != 1 || row.state != "staged" || row.observedState != "pending" {
			t.Fatalf("restricted replacement assignment = %+v", row)
		}
	}
	for _, old := range []tunnelEdgeAcceptanceRow{first, second} {
		state := fixture.loadAssignment(t, database, old.generation)
		if state.state != "draining" || state.accessMode != "public" {
			t.Fatalf("old public assignment state = %+v; want draining", state)
		}
		wrongDetach := fixture.observation(old, "detached")
		wrongDetach.ViewerPolicyGeneration++
		if err := service.ObserveRoutes(ctx, fixture.nodeID, []RouteObservation{wrongDetach}); !errors.Is(err, ErrAssignmentConflict) {
			t.Fatalf("wrong-generation detached ACK = %v, want ErrAssignmentConflict", err)
		}
		if err := service.ObserveRoutes(ctx, fixture.nodeID, []RouteObservation{fixture.observation(old, "detached")}); err != nil {
			t.Fatalf("exact old-public detached ACK: %v", err)
		}
	}
	policyState, err = viewerPolicies.GetViewerPolicy(ctx, fixture.accountID, "tunnel", fixture.tunnelID, fixture.routeID)
	if err != nil || policyState.ServingState != "pending" {
		t.Fatalf("policy state after public detached ACK = %#v, %v; want pending until restricted ready", policyState, err)
	}
	wrong := fixture.observation(newFirst, "ready")
	wrong.AccessMode = "public"
	if err := service.ObserveRoutes(ctx, fixture.nodeID, []RouteObservation{wrong}); !errors.Is(err, ErrAssignmentConflict) {
		t.Fatalf("wrong-mode ready ACK = %v, want ErrAssignmentConflict", err)
	}
	if err := service.ObserveRoutes(ctx, fixture.nodeID, []RouteObservation{fixture.observation(newFirst, "ready")}); err != nil {
		t.Fatalf("exact restricted ready ACK: %v", err)
	}
	if err := service.ObserveRoutes(ctx, fixture.nodeID, []RouteObservation{fixture.observation(newSecond, "ready")}); err != nil {
		t.Fatalf("exact second restricted ready ACK: %v", err)
	}
	for _, row := range []tunnelEdgeAcceptanceRow{fixture.loadAssignment(t, database, 3), fixture.loadAssignment(t, database, 4)} {
		if row.state != "active" || row.observedState != "ready" || row.accessMode != "private" || row.viewerPolicyGeneration != 1 {
			t.Fatalf("serving restricted assignment = %+v", row)
		}
	}
	policyState, err = viewerPolicies.GetViewerPolicy(ctx, fixture.accountID, "tunnel", fixture.tunnelID, fixture.routeID)
	if err != nil || policyState.ServingState != "ready" {
		t.Fatalf("policy state after restricted ready ACK = %#v, %v; want ready", policyState, err)
	}
}

// TestStagedTunnelEdgeAssignmentIsReplacedAfterConnectorProcessRestart
// guards the recovery path used when a connector dies after the server has
// staged its assignment but before the edge acknowledges it. The old staged
// row must remain in history for exact detach fencing while no longer
// occupying the per-route/per-connector staged slot.
func TestStagedTunnelEdgeAssignmentIsReplacedAfterConnectorProcessRestart(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("PAPERBOAT_TEST_DATABASE_DSN"))
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN to run TRK-15 PostgreSQL acceptance")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	if !safeTunnelEdgeAcceptanceDSN(dsn) {
		t.Fatal("PAPERBOAT_TEST_DATABASE_DSN must name an isolated *_test database")
	}
	if production := strings.TrimSpace(os.Getenv("PAPERBOAT_DATABASE_DSN")); production != "" && sameTunnelEdgeAcceptanceDatabase(dsn, production) {
		t.Fatal("refusing to run TRK-15 acceptance against PAPERBOAT_DATABASE_DSN")
	}

	ctx := context.Background()
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	fixture := newTunnelEdgeAcceptanceFixture(fmt.Sprintf("restart-%d", time.Now().UnixNano()), now)
	t.Cleanup(func() {
		_, _ = database.SQL().ExecContext(ctx, `DELETE FROM paperboat.users WHERE id=$1`, fixture.accountID)
		_, _ = database.SQL().ExecContext(ctx, `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, fixture.nodeID)
		_ = database.Close()
	})
	if err := db.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	fixture.insertBase(t, database)
	fixture.insertConnector(t, database, fixture.connectorOne, fixture.hostOne, fixture.sessionOne, 5)

	service := &EdgeService{store: database, clock: func() time.Time { return now }}
	if staged, err := service.ReconcileTunnelEdgeRouteAssignments(ctx, 10); err != nil || staged != 1 {
		t.Fatalf("initial reconcile = %d, %v; want one staged assignment", staged, err)
	}
	old := fixture.loadAssignment(t, database, 1)
	if old.state != "staged" || old.observedState != "pending" || old.sessionID != fixture.sessionOne || old.processGeneration != 5 {
		t.Fatalf("initial staged assignment = %+v", old)
	}

	replacementSession := "ses_trk15_restart_" + fixture.suffix
	fixture.replaceConnectorSession(t, database, fixture.connectorOne, replacementSession, 10)
	if staged, err := service.ReconcileTunnelEdgeRouteAssignments(ctx, 10); err != nil || staged != 1 {
		t.Fatalf("replacement reconcile = %d, %v; want one staged assignment", staged, err)
	}
	replaced := fixture.loadAssignment(t, database, 1)
	current := fixture.loadAssignment(t, database, 2)
	if replaced.state != "draining" || replaced.observedState != "draining" {
		t.Fatalf("stale staged assignment = %+v; want draining history", replaced)
	}
	if current.state != "staged" || current.observedState != "pending" || current.sessionID != replacementSession || current.processGeneration != 10 {
		t.Fatalf("replacement staged assignment = %+v", current)
	}
	if staged, err := service.ReconcileTunnelEdgeRouteAssignments(ctx, 10); err != nil || staged != 0 {
		t.Fatalf("replacement replay = %d, %v; want idempotent no-op", staged, err)
	}
}

type tunnelEdgeAcceptanceFixture struct {
	suffix, accountID, tunnelID, routeID, nodeID, epoch string
	hostOne, hostTwo, connectorOne, connectorTwo        string
	sessionOne, sessionTwo                              string
	publicKeyOne, publicKeyTwo                          string
	thumbprintOne, thumbprintTwo                        string
	configHash                                          []byte
	now                                                 time.Time
}

func newTunnelEdgeAcceptanceFixture(suffix string, now time.Time) tunnelEdgeAcceptanceFixture {
	publicKey := func(label string) (string, string) {
		seed := sha256.Sum256([]byte("trk15-machine:" + label + ":" + suffix))
		private := ed25519.NewKeyFromSeed(seed[:])
		public := private.Public().(ed25519.PublicKey)
		thumbprint, err := connectorprotocol.IdentityThumbprint(public)
		if err != nil {
			panic(err)
		}
		return base64.RawURLEncoding.EncodeToString(public), thumbprint
	}
	publicKeyOne, thumbprintOne := publicKey("one")
	publicKeyTwo, thumbprintTwo := publicKey("two")
	configHash := sha256.Sum256([]byte("trk15-config:" + suffix))
	return tunnelEdgeAcceptanceFixture{
		suffix: suffix, accountID: "usr_trk15_" + suffix, tunnelID: "tun_trk15_" + suffix,
		routeID: "rte_trk15_" + suffix, nodeID: "edge-trk15-" + suffix, epoch: "edge-process-trk15-" + suffix,
		hostOne: "mch_trk15_a_" + suffix, hostTwo: "mch_trk15_b_" + suffix,
		connectorOne: "con_trk15_a_" + suffix, connectorTwo: "con_trk15_b_" + suffix,
		sessionOne: "ses_trk15_a_" + suffix, sessionTwo: "ses_trk15_b_" + suffix,
		publicKeyOne: publicKeyOne, publicKeyTwo: publicKeyTwo,
		thumbprintOne: thumbprintOne, thumbprintTwo: thumbprintTwo,
		configHash: configHash[:], now: now,
	}
}

func (f tunnelEdgeAcceptanceFixture) insertBase(t *testing.T, database *db.DB) {
	t.Helper()
	ctx := context.Background()
	endpointID := testutil.EndpointID("tunnel-edge:" + f.suffix)
	endpoint := "https://" + testutil.EndpointLabel(endpointID) + ".tunnels.example.test"
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, []any{f.accountID, "workos_" + f.suffix, f.accountID + "@example.test"}},
		{`INSERT INTO paperboat.user_machines (id,user_id,environment_id,description,platform,architecture,workspace_root,state,seat_state,public_identity_key) VALUES ($1,$2,$3,$4,'linux','amd64','/workspace','online','occupied',$5),($6,$2,$7,$8,'linux','amd64','/workspace','online','occupied',$9)`, []any{f.hostOne, f.accountID, "env_" + f.hostOne, "TRK15 host one", f.publicKeyOne, f.hostTwo, "env_" + f.hostTwo, "TRK15 host two", f.publicKeyTwo}},
		{`INSERT INTO paperboat.tunnels (id,account_id,name,desired_state,access_mode,generation,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id,summary_code,created_at,updated_at) VALUES ($1,$2,$3,'active','public',1,$4,$5,$6,$2,'pending',$7,$7)`, []any{f.tunnelID, f.accountID, "trk15-" + f.suffix, endpointID, endpoint, f.hostOne, f.now}},
		{`INSERT INTO paperboat.tunnel_routes (id,tunnel_id,name,protocol,match_type,match_hostname,path_prefix,origin_scheme,origin_address,created_by_actor_id,updated_by_actor_id,created_at,updated_at) VALUES ($1,$2,'web','http','exact',$3,'/','http','127.0.0.1:3000',$4,$4,$5,$5)`, []any{f.routeID, f.tunnelID, "trk15-" + f.suffix + ".example.test", f.accountID, f.now}},
		{`INSERT INTO paperboat.tunnel_config_generations (tunnel_id,generation,content_hash,snapshot,activation_state,created_by_actor_id,created_at,activated_at,retained_until) VALUES ($1,1,$2,convert_to(jsonb_build_object('routes',jsonb_build_array(jsonb_build_object('id',$6::text)))::text,'UTF8'),'active',$3,$4,$4,$5)`, []any{f.tunnelID, f.configHash, f.accountID, f.now, f.now.Add(time.Hour), f.routeID}},
		{`INSERT INTO paperboat.control_tunnel_nodes (id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at) VALUES ($1,'trk15-zone-a','1.0',$2,'ready',true,$3,'region-a','domain-a',ARRAY['edge'],ARRAY['http3','http2'],100,0,$3,$3::timestamptz+interval '1 hour')`, []any{f.nodeID, f.epoch, f.now}},
	}
	for _, statement := range statements {
		if _, err := database.SQL().ExecContext(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
}

func (f tunnelEdgeAcceptanceFixture) insertConnector(t *testing.T, database *db.DB, connectorID, hostID, sessionID string, processGeneration int64) {
	t.Helper()
	ctx := context.Background()
	_, thumbprint := f.machineIdentity(hostID)
	if _, err := database.SQL().ExecContext(ctx, `INSERT INTO paperboat.tunnel_connectors (id,tunnel_id,host_id,credential_reference,credential_thumbprint,rotation_generation,desired_state,protocol_version,last_session_id,last_heartbeat_at,ready_at,last_applied_config_generation,drain_state,generation,created_at,updated_at) VALUES ($1,$2,$3,$4,$5,1,'active','1.0',NULL,$6,$6,1,'accepting',1,$6,$6)`, connectorID, f.tunnelID, hostID, "keychain://trk15/"+connectorID, thumbprint+connectorID, f.now); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SQL().ExecContext(ctx, `INSERT INTO paperboat.tunnel_connector_sessions (id,connector_id,process_generation,protocol_version,capabilities,credential_generation,state,lease_deadline,last_heartbeat_at,ready_at,applied_config_generation,retained_until,created_at) VALUES ($1,$2,$3,'1.0',ARRAY['config.snapshot.v1']::text[],1,'ready',$4,$5,$5,1,$6,$5)`, sessionID, connectorID, processGeneration, f.now.Add(20*time.Minute), f.now, f.now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SQL().ExecContext(ctx, `UPDATE paperboat.tunnel_connectors SET last_session_id=$1 WHERE id=$2`, sessionID, connectorID); err != nil {
		t.Fatal(err)
	}
}

func (f tunnelEdgeAcceptanceFixture) markConnectorPreReady(t *testing.T, database *db.DB, connectorID, sessionID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := database.SQL().ExecContext(ctx, `UPDATE paperboat.tunnel_connector_sessions SET state='authenticating', ready_at=NULL WHERE id=$1 AND connector_id=$2`, sessionID, connectorID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SQL().ExecContext(ctx, `UPDATE paperboat.tunnel_connectors SET ready_at=NULL WHERE id=$1`, connectorID); err != nil {
		t.Fatal(err)
	}
}

func (f tunnelEdgeAcceptanceFixture) markConnectorReady(t *testing.T, database *db.DB, connectorID, sessionID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := database.SQL().ExecContext(ctx, `UPDATE paperboat.tunnel_connector_sessions SET state='ready', ready_at=$3 WHERE id=$1 AND connector_id=$2`, sessionID, connectorID, f.now); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SQL().ExecContext(ctx, `UPDATE paperboat.tunnel_connectors SET ready_at=$2 WHERE id=$1`, connectorID, f.now); err != nil {
		t.Fatal(err)
	}
}

func (f tunnelEdgeAcceptanceFixture) replaceConnectorSession(t *testing.T, database *db.DB, connectorID, sessionID string, processGeneration int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := database.SQL().ExecContext(ctx, `INSERT INTO paperboat.tunnel_connector_sessions (id,connector_id,process_generation,protocol_version,capabilities,credential_generation,state,lease_deadline,last_heartbeat_at,ready_at,applied_config_generation,retained_until,created_at) VALUES ($1,$2,$3,'1.0',ARRAY['config.snapshot.v1']::text[],1,'ready',$4,$5,$5,1,$6,$5)`, sessionID, connectorID, processGeneration, f.now.Add(20*time.Minute), f.now, f.now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SQL().ExecContext(ctx, `UPDATE paperboat.tunnel_connectors SET last_session_id=$1,last_heartbeat_at=$2,ready_at=$2 WHERE id=$3`, sessionID, f.now, connectorID); err != nil {
		t.Fatal(err)
	}
}

type tunnelEdgeAcceptanceRow struct {
	assignmentID, hostID, connectorID, sessionID, state, observedState, publicKey, thumbprint, nodeID, epoch string
	accessMode                                                                                               string
	generation, routeRevision, connectorGeneration, processGeneration, configGeneration                      int64
	viewerPolicyGeneration                                                                                   int64
	configHash                                                                                               []byte
	assignedAt                                                                                               time.Time
}

func (f tunnelEdgeAcceptanceFixture) loadAssignment(t *testing.T, database *db.DB, generation int64) tunnelEdgeAcceptanceRow {
	t.Helper()
	var row tunnelEdgeAcceptanceRow
	err := database.SQL().QueryRowContext(context.Background(), `SELECT assignment_id,host_id,connector_id,connector_session_id,state,observed_state,machine_identity_public_key,machine_identity_thumbprint,edge_node_id,edge_process_epoch,assignment_generation,route_revision,connector_generation,connector_process_generation,config_generation,config_content_hash,assigned_at,access_mode,viewer_policy_generation FROM paperboat.tunnel_edge_route_assignments WHERE route_id=$1 AND assignment_generation=$2`, f.routeID, generation).Scan(&row.assignmentID, &row.hostID, &row.connectorID, &row.sessionID, &row.state, &row.observedState, &row.publicKey, &row.thumbprint, &row.nodeID, &row.epoch, &row.generation, &row.routeRevision, &row.connectorGeneration, &row.processGeneration, &row.configGeneration, &row.configHash, &row.assignedAt, &row.accessMode, &row.viewerPolicyGeneration)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func (f tunnelEdgeAcceptanceFixture) observation(row tunnelEdgeAcceptanceRow, state string) RouteObservation {
	return RouteObservation{RouteID: f.routeID, AssignmentID: row.assignmentID, AssignmentGeneration: row.generation, RouteRevision: row.routeRevision, EdgeNodeID: row.nodeID, EdgeProcessEpoch: row.epoch, ConnectorID: row.connectorID, HostID: row.hostID, ConnectorGeneration: row.connectorGeneration, ConnectorSessionID: row.sessionID, ConnectorProcessGeneration: row.processGeneration, ConfigGeneration: row.configGeneration, ConfigContentHash: "sha256:" + hex.EncodeToString(row.configHash), AccessMode: row.accessMode, ViewerPolicyGeneration: row.viewerPolicyGeneration, State: state}
}

func assertTunnelEdgeAssignmentBinding(t *testing.T, row tunnelEdgeAcceptanceRow, fixture tunnelEdgeAcceptanceFixture, connectorID, hostID, sessionID string, processGeneration int64, state, observed string) {
	t.Helper()
	publicKey, thumbprint := fixture.machineIdentity(hostID)
	if row.generation < 1 || row.hostID != hostID || row.connectorID != connectorID || row.sessionID != sessionID || row.connectorGeneration != 1 || row.processGeneration != processGeneration || row.configGeneration != 1 || row.nodeID != fixture.nodeID || row.epoch != fixture.epoch || row.publicKey != publicKey || row.thumbprint != thumbprint || row.accessMode != "public" || row.viewerPolicyGeneration != 1 || !strings.EqualFold(hex.EncodeToString(row.configHash), hex.EncodeToString(fixture.configHash)) || row.state != state || row.observedState != observed {
		t.Fatalf("assignment binding = %+v", row)
	}
}

func (f tunnelEdgeAcceptanceFixture) machineIdentity(hostID string) (string, string) {
	if hostID == f.hostTwo {
		return f.publicKeyTwo, f.thumbprintTwo
	}
	return f.publicKeyOne, f.thumbprintOne
}

func safeTunnelEdgeAcceptanceDSN(dsn string) bool {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return false
	}
	name := strings.TrimPrefix(parsed.Path, "/")
	return strings.HasSuffix(name, "_test") && name != "_test"
}
func sameTunnelEdgeAcceptanceDatabase(left, right string) bool {
	l, le := url.Parse(left)
	r, re := url.Parse(right)
	return le == nil && re == nil && strings.EqualFold(l.Hostname(), r.Hostname()) && l.Port() == r.Port() && strings.TrimPrefix(l.Path, "/") == strings.TrimPrefix(r.Path, "/")
}
