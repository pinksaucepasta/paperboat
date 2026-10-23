package usermachines

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/helperruntime"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
)

// hostSessionUnknown is the host's answer for a session it does not have.
type hostSessionUnknown struct{}

func (hostSessionUnknown) Error() string                  { return "not_found_or_forbidden" }
func (hostSessionUnknown) DiagnosticProtocolCode() string { return "not_found_or_forbidden" }

// hostWithSessions answers snapshots for the sessions the host has, "unknown" for every other one, and counts asks.
type hostWithSessions struct {
	helperruntime.Client
	has         map[string]bool
	unreachable bool
	mu          sync.Mutex
	asked       map[string]int
}

func (h *hostWithSessions) askedCount(session string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.asked[session]
}

func (h *hostWithSessions) Terminal(_ context.Context, _, _, operation, session, _ string) (helperruntime.Snapshot, error) {
	h.mu.Lock()
	h.asked[session]++
	h.mu.Unlock()
	if h.unreachable {
		return helperruntime.Snapshot{}, errors.New("fixture host unavailable")
	}
	if !h.has[session] {
		return helperruntime.Snapshot{}, fmt.Errorf("terminal %s: %w", operation, hostSessionUnknown{})
	}
	return helperruntime.Snapshot{ID: session, State: "running", Generation: 1}, nil
}

// terminalOrphanFixture is a machine with a live route to a host, owned by one account.
func terminalOrphanFixture(t *testing.T, store *db.DB) (*Service, *hostWithSessions, string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	suffix := fmt.Sprint(time.Now().UnixNano())
	owner, machine, environment := "ti_owner_"+suffix, "ti_machine_"+suffix, "ti_environment_"+suffix
	edge, epoch, route := "ti_edge_"+suffix, "ti_epoch_"+suffix, "ti_route_"+suffix
	identity := newIntegrationPublicIdentityKey(t)
	insert := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	insert(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@invalid.test','active')`, owner)
	insert(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,public_identity_key,worker_generation,configured_capabilities,observed_capabilities) VALUES($1,$2,$3,'orphan-host','linux','amd64','/launch','online','occupied',true,$4,1,ARRAY['terminal_host'],ARRAY['terminal_host'])`, machine, owner, environment, identity)
	insert(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id,desired_state) VALUES($1,$2,$3,'active')`, environment, machine, owner)
	insert(`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,roles,transports,registry_expires_at,region,failure_domain,capacity_limit,capacity_observed_at) VALUES($1,'development','1.0',$2,'ready',true,now(),ARRAY['edge'],ARRAY['http2','http3'],now()+interval '1 hour','test-region','test-domain',100,now())`, edge, epoch)
	insert(`UPDATE paperboat.control_tunnel_nodes SET public_ingress_verified_at=now(),endpoint_host='edge.example.test' WHERE id=$1`, edge)
	insert(`INSERT INTO paperboat.control_connector_generations(environment_id,machine_id,generation,edge_pool,edge_node_id,state) VALUES($1,$2,1,'development',$3,'admitted')`, environment, machine, edge)
	host := "orphan-" + suffix + ".example.test"
	insert(`INSERT INTO paperboat.control_routes(id,environment_id,kind,public_host,target_host,target_port,desired_revision,applied_revision,applied_node_id,applied_generation) VALUES($1,$2,'runtime_https_wss',$3,'127.0.0.1',8080,1,1,$4,1)`, route, environment, host, edge)
	insert(`INSERT INTO paperboat.runtime_carrier_attachments(route_id,machine_id,account_id,installation_generation,worker_generation,process_id,generation,edge_node_id,edge_process_epoch,public_identity_key,route_revision,session_id,preview_session_id,admission,expires_at,ready,observed_at,active) VALUES($1,$2,$3,1,1,$4,1,$5,$6,$7,1,$8,$9,'{}',now()+interval '1 hour',true,now(),true)`, route, machine, owner, "ti_process_"+suffix, edge, epoch, identity, "session_"+uuid.NewString(), "session_"+uuid.NewString())
	var carrierReady bool
	if err := store.SQL().QueryRowContext(ctx, `SELECT paperboat.runtime_carrier_current($1,true)`, route).Scan(&carrierReady); err != nil || !carrierReady {
		t.Fatalf("fixture carrier readiness=%t error=%v", carrierReady, err)
	}
	signer, err := mint.NewEphemeral(5 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	service := New(store, audit.NewWriter(store), Policy{}, nil)
	service.issuer = "https://api.paperboat.test"
	service.ConfigureTerminalSessions(signer, nil)
	runtime := &hostWithSessions{asked: map[string]int{}}
	service.controlRuntime = runtime
	return service, runtime, owner, machine
}

// insertOrphanSession adds a live session the server has never confirmed, created at the given age.
func insertOrphanSession(t *testing.T, store *db.DB, id, owner, machine, name string, age time.Duration) {
	t.Helper()
	if _, err := store.SQL().ExecContext(context.Background(), `INSERT INTO paperboat.user_machine_terminal_sessions(id,user_machine_id,owner_account,name,launch_cwd,desired_state,runtime_state,created_at,updated_at) VALUES($1,$2,$3,$4,'/launch','open','running',now()-$5::interval,now()-$5::interval)`, id, machine, owner, name, fmt.Sprintf("%d seconds", int(age.Seconds()))); err != nil {
		t.Fatal(err)
	}
}

// A refused or lost create leaves a live row the host never had. The sweep closes it once the host says unknown,
// leaves a host that is unreachable alone, confirms a session the host has, and does not ask a session younger
// than the bound.
func TestPostgresOrphanTerminalSessionsCloseOnlyOnHostUnknown(t *testing.T) {
	store := testStore(t)
	service, host, owner, machine := terminalOrphanFixture(t, store)
	suffix := fmt.Sprint(time.Now().UnixNano())
	unknown, known, young := "terminal_"+uuid.NewString(), "terminal_"+uuid.NewString(), "terminal_"+uuid.NewString()
	// Old rows sort before any rows other tests left behind in the shared database.
	insertOrphanSession(t, store, unknown, owner, machine, "orphan-"+suffix, 24*time.Hour)
	insertOrphanSession(t, store, known, owner, machine, "known-"+suffix, 24*time.Hour)
	insertOrphanSession(t, store, young, owner, machine, "young-"+suffix, time.Second)
	host.has = map[string]bool{known: true}
	state := func(id string) (string, bool) {
		t.Helper()
		var runtimeState string
		var confirmed bool
		if err := store.SQL().QueryRowContext(context.Background(), `SELECT runtime_state, last_runtime_sync_at IS NOT NULL FROM paperboat.user_machine_terminal_sessions WHERE id=$1`, id).Scan(&runtimeState, &confirmed); err != nil {
			t.Fatal(err)
		}
		return runtimeState, confirmed
	}

	// An unreachable host closes nothing and confirms nothing.
	host.unreachable = true
	if err := service.reconcileOrphanTerminalSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, _ := state(unknown); got != "running" {
		t.Fatalf("unreachable host changed an orphan to %s", got)
	}
	if _, confirmed := state(known); confirmed {
		t.Fatal("unreachable host confirmed a session")
	}
	// An unanswered row goes to the back of the queue, so it cannot hold the batch for the orphans behind it.
	var requeued bool
	if err := store.SQL().QueryRowContext(context.Background(), `SELECT updated_at > now()-interval '1 minute' FROM paperboat.user_machine_terminal_sessions WHERE id=$1`, unknown).Scan(&requeued); err != nil || !requeued {
		t.Fatalf("unanswered orphan was not moved to the back of the queue: %v", err)
	}
	// A host that is offline cannot answer, so its rows are not asked at all.
	if _, err := store.SQL().ExecContext(context.Background(), `UPDATE paperboat.user_machines SET online=false WHERE id=$1`, machine); err != nil {
		t.Fatal(err)
	}
	asked := host.askedCount(unknown)
	if err := service.reconcileOrphanTerminalSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if host.askedCount(unknown) != asked {
		t.Fatal("orphan on an offline machine was asked")
	}
	if _, err := store.SQL().ExecContext(context.Background(), `UPDATE paperboat.user_machines SET online=true WHERE id=$1`, machine); err != nil {
		t.Fatal(err)
	}

	// One reachable sweep: the unknown row closes, the known row is confirmed, the young row is not asked.
	host.unreachable = false
	if err := service.reconcileOrphanTerminalSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, _ := state(unknown); got != "closed" {
		t.Fatalf("orphan the host does not know = %s, want closed", got)
	}
	if got, confirmed := state(known); got != "running" || !confirmed {
		t.Fatalf("session the host knows = %s confirmed=%v, want running and confirmed", got, confirmed)
	}
	if got, confirmed := state(young); got != "running" || confirmed || host.askedCount(young) != 0 {
		t.Fatalf("young session = %s confirmed=%v asked=%d, want running, unconfirmed and not asked", got, confirmed, host.askedCount(young))
	}

	// Confirmed and closed rows are not asked again.
	asksBefore := host.askedCount(known) + host.askedCount(unknown)
	if err := service.reconcileOrphanTerminalSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if asksAfter := host.askedCount(known) + host.askedCount(unknown); asksAfter != asksBefore {
		t.Fatalf("confirmed or closed sessions were asked again: %d -> %d", asksBefore, asksAfter)
	}
}
