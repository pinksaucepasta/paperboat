package runtimeattachment

import (
	"context"
	"encoding/hex"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"

	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/endpoints"
	"github.com/pinksaucepasta/paperboat/server/internal/ingressprojection"
)

// Edge phases of a worker handover: the route is on the old carrier only; on both (from the new worker's
// attach the edge may hold the route on the new carrier before it reports anything, and prefers it; the old
// one is the fallback), so both must be authorized; on the new carrier only.
const (
	handoverOld int32 = iota
	handoverBoth
	handoverNew
)

// checkWorkerHandover runs pb update's worker handover for a public preview and an engine tunnel while
// the edge sends each a request every 50 ms on the carrier it holds the route on. Every request needs
// an ingress decision for that carrier (pbvm4 prefix1, 2026-11-05: the next worker's attachment
// withdrew the old carrier's decisions before the new carrier served the routes, and 20 preview and 16
// tunnel requests failed for 4.6 s). After the run f.request is the new worker's.
func checkWorkerHandover(t *testing.T, f *agentFixture, control *controlplane.EdgeService, tunnelRoute string) {
	t.Helper()
	both := []string{runtimewire.CapabilityEndpoints, runtimewire.CapabilityTunnelEndpoints}
	f.request.Capabilities = both
	lease := f.prefix + "_handover"
	host := "handover-" + strings.ReplaceAll(f.prefix, "_", "-") + ".preview.example.test"
	f.exec(t, `INSERT INTO preview_leases(id,endpoint,account_id,actor_id,owner_machine_id,owner_session_id,owner_session_kind,target_scheme,target_address,access_mode,lease_deadline,origin_state,ready_at)
VALUES($1,$2,$3,$3,$4,'owner_session_handover','foreground','http','127.0.0.1:3000','public',now()+interval '10 minutes','ready',now())`, lease, "https://"+host, f.account, f.machine)
	if err := f.database.InTx(f.ctx, func(ctx context.Context, tx *db.Tx) error {
		return endpoints.RegisterPreviewTx(ctx, tx, endpoints.Preview{LeaseID: lease, AccountID: f.account, AgentMachineID: f.machine, AccessMode: "public", Endpoint: "https://" + host})
	}); err != nil {
		t.Fatal(err)
	}
	carrierOn := func(a runtimewire.HostAdmission) runtimewire.Admission {
		t.Helper()
		if a.Backup != nil && a.Backup.Binding.EdgeNodeID == f.node {
			return *a.Backup
		}
		if a.Admission.Binding.EdgeNodeID != f.node {
			t.Fatalf("no carrier to %s in %+v", f.node, a)
		}
		return a.Admission
	}
	// The edge holds the carrier up and the preview and tunnel routes on it ready.
	serve := func(carrier runtimewire.Admission, routes bool) {
		t.Helper()
		observations := []runtimewire.Observation{{RouteID: f.route, SessionID: carrier.Binding.SessionID, AttachmentGeneration: carrier.AttachmentGeneration, Ready: true}}
		if routes {
			values, err := f.service.Pull(f.ctx, f.edge, both)
			if err != nil {
				t.Fatal(err)
			}
			for _, v := range values {
				if v.EndpointID == lease {
					observations = append(observations, runtimewire.Observation{RouteID: v.Binding.RouteID, SessionID: v.Binding.SessionID, AttachmentGeneration: v.AttachmentGeneration, Ready: true})
				}
			}
			if len(observations) != 2 {
				t.Fatalf("preview %s not pulled by the edge: %+v", lease, values)
			}
		}
		if err := f.service.Observe(f.ctx, f.edge, observations); err != nil {
			t.Fatal(err)
		}
		if !routes {
			return
		}
		rows, err := control.ListTunnelEdgeRouteAssignmentsForNodeV1(f.ctx, f.node, f.edge.ProcessEpoch)
		if err != nil || len(rows) != 1 {
			t.Fatalf("tunnel route snapshot = %d rows, %v", len(rows), err)
		}
		row := rows[0]
		if err := control.ObserveRoutes(f.ctx, f.node, []controlplane.RouteObservation{{
			RouteID: tunnelRoute, AssignmentID: row.AssignmentID, AssignmentGeneration: row.AssignmentGeneration, RouteRevision: row.RouteRevision,
			EdgeNodeID: f.node, EdgeProcessEpoch: f.edge.ProcessEpoch, ConnectorID: row.ConnectorID, HostID: row.HostID,
			ConnectorGeneration: 1, ConnectorSessionID: row.ConnectorSessionID, ConnectorProcessGeneration: row.ConnectorProcessGeneration,
			ConfigGeneration: row.ConfigGeneration, ConfigContentHash: "sha256:" + hex.EncodeToString(row.ConfigContentHash),
			AccessMode: row.AccessMode, ViewerPolicyGeneration: row.ViewerPolicyGeneration, ObservedState: "ready",
		}}); err != nil {
			t.Fatal(err)
		}
	}
	attach := func(request runtimewire.Request) runtimewire.Admission {
		t.Helper()
		a, err := f.service.Attach(f.ctx, f.proof, request)
		if err != nil {
			t.Fatal(err)
		}
		return carrierOn(a)
	}
	// authorized reports, per route, the carrier sessions the edge's ingress authority admits now.
	authorized := func() (map[string]map[string]bool, error) {
		decisions, err := ingressprojection.SQLIngressSource{DB: f.database, Quota: f.quota}.Edge(f.ctx, f.node, f.edge.ProcessEpoch)
		out := map[string]map[string]bool{lease: {}, tunnelRoute: {}}
		for _, d := range decisions {
			if sessions, ok := out[d.Binding.RouteID]; ok {
				sessions[d.SessionID] = true
			}
		}
		return out, err
	}

	old := attach(f.request)
	serve(old, true)
	if got, err := authorized(); err != nil || !got[lease][old.Binding.SessionID] || !got[tunnelRoute][old.Binding.SessionID] {
		t.Fatalf("before the handover the old carrier is not authorized: %v, %v", got, err)
	}

	var phase atomic.Int32
	var next atomic.Value
	next.Store("")
	var mu sync.Mutex
	var failures []string
	var slowest time.Duration
	stop := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
			at, p, fresh := time.Now(), phase.Load(), next.Load().(string)
			got, err := authorized()
			took := time.Since(at)
			mu.Lock()
			slowest = max(slowest, took)
			for _, route := range []string{lease, tunnelRoute} {
				onOld, onNew := got[route][old.Binding.SessionID], fresh != "" && got[route][fresh]
				if err != nil || (p == handoverOld && !onOld) || (p == handoverBoth && (!onOld || !onNew)) || (p == handoverNew && !onNew) {
					failures = append(failures, route+" in phase "+string('0'+rune(p))+": "+at.Format("05.000"))
				}
			}
			mu.Unlock()
		}
	}()
	step := func() { time.Sleep(300 * time.Millisecond) }
	step()
	// The next worker generation registers, then attaches while the old one still serves.
	f.exec(t, `UPDATE user_machines SET worker_generation=2 WHERE id=$1`, f.machine)
	step()
	request := f.request
	request.ProcessID, request.WorkerGeneration = "process_next", 2
	fresh := attach(request)
	if fresh.Binding.SessionID == old.Binding.SessionID {
		t.Fatal("the next worker reused the old carrier session")
	}
	next.Store(fresh.Binding.SessionID)
	// pbvm1 vm1-1a6479a4-124638: the edge had the preview route on the new carrier at once and was denied
	// there for 8 s until the server learned the carrier was up, so every request fell back to the old one.
	phase.Store(handoverBoth)
	step()
	// The new carrier is up at the edge.
	serve(fresh, false)
	step()
	// The edge observed the routes on the new carrier and sends every request there.
	serve(fresh, true)
	phase.Store(handoverNew)
	step()
	close(stop)
	<-finished
	if len(failures) != 0 || slowest > time.Second {
		t.Fatalf("handover: %d failed requests %v, slowest decision %v (want 0 and at most 1 s)", len(failures), failures, slowest)
	}
	f.request = request

	// The overlap is bounded, and revocation still wins over it.
	f.exec(t, `UPDATE user_machines SET revoked_at=now() WHERE id=$1`, f.machine)
	if got, err := authorized(); err != nil || len(got[lease])+len(got[tunnelRoute]) != 0 {
		t.Fatalf("a revoked machine's carriers are still authorized: %v, %v", got, err)
	}
	f.exec(t, `UPDATE user_machines SET revoked_at=NULL WHERE id=$1`, f.machine)
	f.exec(t, `UPDATE runtime_carrier_attachments SET previous_until=now() WHERE machine_id=$1`, f.machine)
	if got, err := authorized(); err != nil || got[lease][old.Binding.SessionID] || got[tunnelRoute][old.Binding.SessionID] || !got[lease][fresh.Binding.SessionID] || !got[tunnelRoute][fresh.Binding.SessionID] {
		t.Fatalf("after the overlap: %v, %v; want only the new carrier", got, err)
	}
	f.exec(t, `DELETE FROM endpoints WHERE id=$1`, lease)
	f.exec(t, `DELETE FROM preview_leases WHERE id=$1`, lease)
}
