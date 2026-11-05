package runtimeattachment

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
)

// An edge restart must not fail the server's requests to a machine: the dialer sends them to the
// edge whose carrier can serve now (valid, node not draining, active first), and only when no edge
// can does it report ErrRouteMoving, after its bounded wait.
func TestEdgeDialerPrefersAReadyCarrierOnANonDrainingEdgePostgres(t *testing.T) {
	f := newAgentFixture(t, "edgedial")
	a, err := f.service.Attach(f.ctx, f.proof, f.request)
	if err != nil || a.Backup == nil {
		t.Fatalf("attach with a backup carrier: %+v %v", a, err)
	}
	observe := func(admission runtimewire.Admission) {
		t.Helper()
		edge, identity := admission.Binding.EdgeNodeID, f.edge
		if edge == f.backupNode {
			identity = f.backupEdge
		}
		if err := f.service.Observe(f.ctx, identity, []runtimewire.Observation{{RouteID: f.route, SessionID: admission.Binding.SessionID, AttachmentGeneration: admission.AttachmentGeneration, Ready: true}}); err != nil {
			t.Fatalf("observe %s: %v", edge, err)
		}
	}
	primaryNode, backupNode := a.Binding.EdgeNodeID, a.Backup.Binding.EdgeNodeID
	observe(a.Admission)
	observe(*a.Backup)
	f.exec(t, `UPDATE control_tunnel_nodes SET public_ingress_ipv4=CASE WHEN id=$1 THEN '192.0.2.1'::inet ELSE '192.0.2.2'::inet END,public_ingress_verified_at=now() WHERE id IN ($1,$2)`, primaryNode, backupNode)
	addresses := map[string]string{primaryNode: "192.0.2.1:443", backupNode: "192.0.2.2:443"}
	host := f.machine + ".runtime.example.test"

	var dialed []string
	dialer := EdgeDialer{DB: f.database, Wait: 600 * time.Millisecond, Dial: func(_ context.Context, _, addr string) (net.Conn, error) {
		dialed = append(dialed, addr)
		client, server := net.Pipe()
		_ = server.Close()
		return client, nil
	}}
	dial := func() (string, error) {
		t.Helper()
		dialed = nil
		conn, err := dialer.DialContext(f.ctx, "tcp", net.JoinHostPort(host, "443"))
		if conn != nil {
			_ = conn.Close()
		}
		if len(dialed) == 0 {
			return "", err
		}
		return dialed[0], err
	}
	if got, err := dial(); err != nil || got != addresses[primaryNode] {
		t.Fatalf("both edges ready: dialed %q, %v; want the active edge %s", got, err, addresses[primaryNode])
	}
	f.exec(t, `UPDATE control_tunnel_nodes SET state='draining' WHERE id=$1`, primaryNode)
	if got, err := dial(); err != nil || got != addresses[backupNode] {
		t.Fatalf("active edge draining: dialed %q, %v; want the other edge %s", got, err, addresses[backupNode])
	}
	f.exec(t, `UPDATE control_tunnel_nodes SET state='draining' WHERE id=$1`, backupNode)
	started := time.Now()
	if got, err := dial(); !errors.Is(err, ErrRouteMoving) || got != "" {
		t.Fatalf("no edge can serve: dialed %q, %v; want ErrRouteMoving and no dial", got, err)
	}
	if waited := time.Since(started); waited < 500*time.Millisecond {
		t.Fatalf("gave up after %v, before its wait", waited)
	}
	// A host that is not a runtime host (a self-hosted edge's own name) is dialed unchanged.
	dialed = nil
	if conn, err := dialer.DialContext(f.ctx, "tcp", "edge.example.test:443"); err != nil || len(dialed) != 1 || dialed[0] != "edge.example.test:443" {
		t.Fatalf("other host: %v %v", dialed, err)
	} else {
		_ = conn.Close()
	}
}

// Support support_50a7f460 (2026-11-05 12:06:42Z): an edge restart answered a preview create 503
// machine_unavailable while the machine's carrier on the other edge was ready, because the helper route
// lookups required the route's applied edge itself to be ready. Every lookup must find the route while
// any edge's carrier can serve it; the dialer then picks that edge.
func TestHelperRouteSurvivesItsAppliedEdgeRestartPostgres(t *testing.T) {
	f := newAgentFixture(t, "helperroute")
	a, err := f.service.Attach(f.ctx, f.proof, f.request)
	if err != nil || a.Backup == nil {
		t.Fatalf("attach with a backup carrier: %+v %v", a, err)
	}
	for _, admission := range []runtimewire.Admission{a.Admission, *a.Backup} {
		identity := f.edge
		if admission.Binding.EdgeNodeID == f.backupNode {
			identity = f.backupEdge
		}
		if err := f.service.Observe(f.ctx, identity, []runtimewire.Observation{{RouteID: f.route, SessionID: admission.Binding.SessionID, AttachmentGeneration: admission.AttachmentGeneration, Ready: true}}); err != nil {
			t.Fatalf("observe %s: %v", admission.Binding.EdgeNodeID, err)
		}
	}
	lookups := func() error {
		q := f.database.Queries()
		if _, err := q.GetActiveHelperRouteForMachine(f.ctx, dbsqlc.GetActiveHelperRouteForMachineParams{MachineID: f.machine, AccountID: f.account}); err != nil {
			return fmt.Errorf("for machine: %w", err)
		}
		if _, err := q.GetActiveHelperRouteForEnvironment(f.ctx, f.env); err != nil {
			return fmt.Errorf("for environment: %w", err)
		}
		if hosts, err := q.ListActiveHelperRouteHostsForEnvironments(f.ctx, []string{f.env}); err != nil || len(hosts) != 1 {
			return fmt.Errorf("hosts for environments: %d %v", len(hosts), err)
		}
		return nil
	}
	if err := lookups(); err != nil {
		t.Fatalf("both edges ready: %v", err)
	}
	// The applied edge restarts: a new process registers and is not ready yet.
	f.exec(t, `UPDATE control_tunnel_nodes SET process_epoch='epoch_restarted',state='registered',ready=false WHERE id=$1`, a.Binding.EdgeNodeID)
	if err := lookups(); err != nil {
		t.Fatalf("applied edge restarting, other carrier ready: %v", err)
	}
	// No edge can serve: the lookups report no route.
	f.exec(t, `UPDATE control_tunnel_nodes SET state='draining',drain_deadline=now() WHERE id=$1`, a.Backup.Binding.EdgeNodeID)
	if _, err := f.database.Queries().GetActiveHelperRouteForMachine(f.ctx, dbsqlc.GetActiveHelperRouteForMachineParams{MachineID: f.machine, AccountID: f.account}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("no edge can serve: %v, want no rows", err)
	}
}
