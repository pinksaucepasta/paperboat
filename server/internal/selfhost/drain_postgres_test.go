package selfhost

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

type drainFixture struct {
	t       *testing.T
	store   *db.DB
	service *Service
	stamp   string
}

func newDrainFixture(t *testing.T) *drainFixture {
	t.Helper()
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
	if err = db.Migrate(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	return &drainFixture{t: t, store: store, service: New(store, "drain-test-key-with-at-least-32-bytes"), stamp: randomIdentity()}
}

func (f *drainFixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.store.SQL().ExecContext(context.Background(), query, args...); err != nil {
		f.t.Fatalf("%v: %s", err, query)
	}
}

func (f *drainFixture) user(name string) string {
	id := "drain_" + name + "_" + f.stamp
	f.exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, id, id+"@example.test")
	f.t.Cleanup(func() { f.exec(`DELETE FROM paperboat.users WHERE id=$1`, id) })
	return id
}

func (f *drainFixture) nodeState(node string) (state string, deadline *time.Time) {
	f.t.Helper()
	if err := f.store.SQL().QueryRowContext(context.Background(), `SELECT state,drain_deadline FROM paperboat.control_tunnel_nodes WHERE id=$1`, node).Scan(&state, &deadline); err != nil {
		f.t.Fatal(err)
	}
	return
}

func TestOwnerDrainOfOwnNode(t *testing.T) {
	f := newDrainFixture(t)
	ctx := t.Context()
	owner := f.user("owner")
	own := scopedInstallationFixture(t, f.store, "drain-test-key-with-at-least-32-bytes", owner, "tunnel")
	f.exec(`UPDATE paperboat.control_tunnel_nodes SET state='ready',ready=true WHERE id=$1`, own.NodeID)
	item, err := f.service.DrainScoped(ctx, owner, own.InstallationID, false, false)
	if err != nil || item.InstallationID != own.InstallationID || item.Ready {
		t.Fatalf("drain: %+v err=%v", item, err)
	}
	state, deadline := f.nodeState(own.NodeID)
	if state != "draining" || deadline == nil || time.Until(*deadline) < 9*time.Minute {
		t.Fatalf("after drain: %s %v", state, deadline)
	}
	if _, err = f.service.ResumeScoped(ctx, owner, own.InstallationID, false); err != nil {
		t.Fatal(err)
	}
	if state, deadline = f.nodeState(own.NodeID); state != "registered" || deadline != nil {
		t.Fatalf("after resume: %s %v", state, deadline)
	}
}

func TestOwnerCannotDrainOtherScopeNode(t *testing.T) {
	f := newDrainFixture(t)
	ctx := t.Context()
	owner, other := f.user("owner"), f.user("other")
	own := scopedInstallationFixture(t, f.store, "drain-test-key-with-at-least-32-bytes", owner, "tunnel")
	f.exec(`UPDATE paperboat.control_tunnel_nodes SET state='ready',ready=true WHERE id=$1`, own.NodeID)
	if _, err := f.service.DrainScoped(ctx, other, own.InstallationID, false, true); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("another account drained the node: %v", err)
	}
	if _, err := f.service.ResumeScoped(ctx, other, own.InstallationID, false); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("another account resumed the node: %v", err)
	}
	if _, err := f.service.DrainScoped(ctx, owner, own.InstallationID, true, false); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("a non-operator drained through the operator path: %v", err)
	}
	// A node that another scope also holds an installation on is not the owner's to drain.
	f.exec(`INSERT INTO paperboat.selfhost_installations(id,node_id,installation_public_key,runtime_hash,runtime_ciphertext,name,capability,scope_kind,scope_id,claim_id,claim_complete) SELECT $1,node_id,decode(md5($1)||md5($1),'hex'),decode(md5($1||'h')||md5($1||'hh'),'hex'),runtime_ciphertext,'Shared',capability,'account',$2,$1,true FROM paperboat.selfhost_installations WHERE id=$3`, "shi_shared_"+f.stamp, other, own.InstallationID)
	t.Cleanup(func() { f.exec(`DELETE FROM paperboat.selfhost_installations WHERE id=$1`, "shi_shared_"+f.stamp) })
	if _, err := f.service.DrainScoped(ctx, owner, own.InstallationID, false, false); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("drained a node shared with another scope: %v", err)
	}
	if state, deadline := f.nodeState(own.NodeID); state != "ready" || deadline != nil {
		t.Fatalf("a refused drain changed the node: %s %v", state, deadline)
	}
}

func TestOwnerDrainMoveWakesMachines(t *testing.T) {
	f := newDrainFixture(t)
	ctx := t.Context()
	owner := f.user("owner")
	own := scopedInstallationFixture(t, f.store, "drain-test-key-with-at-least-32-bytes", owner, "tunnel")
	f.exec(`UPDATE paperboat.control_tunnel_nodes SET state='ready',ready=true WHERE id=$1`, own.NodeID)
	env, machine, route := "drain_env_"+f.stamp, "drain_m_"+f.stamp, "drain_r_"+f.stamp
	f.exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id) VALUES($1,$1,$2)`, env, owner)
	f.exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES($1,$2,$3,'linux','amd64','/workspace','online','occupied',true,1)`, machine, owner, env)
	t.Cleanup(func() {
		f.exec(`DELETE FROM paperboat.user_machines WHERE id=$1`, machine)
		f.exec(`DELETE FROM paperboat.control_environments WHERE id=$1`, env)
	})
	f.exec(`INSERT INTO paperboat.control_routes(id,environment_id,kind,public_host,target_host,target_port) VALUES($1,$2,'runtime_https_wss',$1||'.runtime.example.test','127.0.0.1',38080)`, route, env)
	f.exec(`INSERT INTO paperboat.runtime_carrier_attachments(route_id,machine_id,account_id,installation_generation,worker_generation,process_id,generation,edge_node_id,edge_process_epoch,public_identity_key,route_revision,session_id,preview_session_id,admission,expires_at,active) SELECT $1,m.id,m.user_id,1,1,'process',1,$2,'epoch',m.public_identity_key,1,$1,$1,'{}',now()+interval '1 hour',true FROM paperboat.user_machines m WHERE m.id=$3`, route, own.NodeID, machine)
	topics := func() (n int) {
		f.t.Helper()
		if err := f.store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.machine_topic_generations WHERE machine_id=$1 AND topic IN ('tunnels','previews')`, machine).Scan(&n); err != nil {
			f.t.Fatal(err)
		}
		return
	}
	if _, err := f.service.DrainScoped(ctx, owner, own.InstallationID, false, false); err != nil || topics() != 0 {
		t.Fatalf("a plain drain woke the machine: %v", err)
	}
	if _, err := f.service.DrainScoped(ctx, owner, own.InstallationID, false, true); err != nil {
		t.Fatal(err)
	}
	if n := topics(); n != 2 {
		t.Fatalf("drain and move woke the machine on %d topics, want 2", n)
	}
	if _, deadline := f.nodeState(own.NodeID); deadline == nil || time.Until(*deadline) > 2*time.Minute {
		t.Fatalf("drain and move left the deadline at %v", deadline)
	}
}
