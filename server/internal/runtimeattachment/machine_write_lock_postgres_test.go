package runtimeattachment

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/endpoints"
	"github.com/pinksaucepasta/paperboat/server/internal/previewattachment"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
)

// Prod 2026-11-20: a machine's carrier row (10 in 30 min) and its route assignments (18) were written by two
// transactions at once, and the later one failed its serializable commit with 40001. Writers of a machine now queue on
// its write lock before their snapshot. Here another writer holds the lock and an uncommitted change of the contended
// row while the machine attaches: the attach must wait for it and commit without a single serialization retry.
func TestAttachQueuesBehindAnotherWriterOfTheMachineOnPostgres(t *testing.T) {
	for _, c := range []struct{ name, change string }{
		{"carrier row", `UPDATE runtime_carrier_attachments SET generation=generation WHERE machine_id=$1`},
		{"route assignments", `UPDATE endpoint_edge_assignments SET generation=generation WHERE endpoint_id IN (SELECT id FROM endpoints WHERE agent_machine_id=$1)`},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newAgentFixture(t, "mlock_"+strings.ReplaceAll(c.name, " ", "_"))
			ctx := f.ctx
			lease := f.prefix + "_preview"
			f.exec(t, `INSERT INTO preview_leases(id,endpoint,account_id,actor_id,owner_machine_id,owner_session_id,owner_session_kind,target_scheme,target_address,access_mode,lease_deadline)
VALUES($1,$2,$3,$3,$4,'owner_session_mlock','foreground','http','127.0.0.1:3000','team',now()+interval '10 minutes')`, lease, "https://"+strings.ReplaceAll(f.prefix, "_", "-")+".preview.example.test", f.account, f.machine)
			if err := f.database.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
				return endpoints.RegisterPreviewTx(ctx, tx, endpoints.Preview{LeaseID: lease, AccountID: f.account, AgentMachineID: f.machine, AccessMode: "team", Endpoint: "https://" + strings.ReplaceAll(f.prefix, "_", "-") + ".preview.example.test"})
			}); err != nil {
				t.Fatal(err)
			}
			request := f.request
			request.Capabilities = []string{runtimewire.CapabilityEndpoints}
			if _, err := f.service.Attach(ctx, f.proof, request); err != nil { // creates the carrier row and the assignments
				t.Fatal(err)
			}
			var rows int
			if err := f.database.SQL().QueryRowContext(ctx, `SELECT (SELECT count(*) FROM runtime_carrier_attachments WHERE machine_id=$1)+(SELECT count(*) FROM endpoint_edge_assignments a JOIN endpoints e ON e.id=a.endpoint_id WHERE e.agent_machine_id=$1)`, f.machine).Scan(&rows); err != nil || rows < 2 {
				t.Fatalf("fixture: %d carrier and assignment rows (%v)", rows, err)
			}

			// The other writer: the machine's write lock, then an uncommitted change of the contended row.
			unlock, err := endpoints.LockMachine(ctx, f.machine)
			if err != nil {
				t.Fatal(err)
			}
			conn, err := f.database.SQL().Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if _, err := conn.ExecContext(ctx, `BEGIN ISOLATION LEVEL SERIALIZABLE`); err != nil {
				t.Fatal(err)
			}
			if _, err := conn.ExecContext(ctx, c.change, f.machine); err != nil {
				t.Fatal(err)
			}

			before := f.database.TransactionStats().Serializable.Retries
			done := make(chan error, 1)
			go func() { _, err := f.service.Attach(ctx, f.proof, request); done <- err }()
			time.Sleep(300 * time.Millisecond) // the attach is now waiting: on the lock (fixed) or on the row (before)
			if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
				t.Fatal(err)
			}
			unlock()
			if err := <-done; err != nil {
				t.Fatalf("attach behind another writer of the machine: %v", err)
			}
			if retries := f.database.TransactionStats().Serializable.Retries - before; retries != 0 {
				t.Fatalf("the attach was retried %d times after a serialization failure, want 0 (it must queue)", retries)
			}
		})
	}
}

// Writers of different machines never wait for each other, and waiting for a machine takes no database connection:
// 50 machines attach at once through one server, every attach succeeds, and the direct pool (6 connections, used by
// session locks) is not touched.
func TestFiftyMachinesAttachAtOnceOnPostgres(t *testing.T) {
	f := newAgentFixture(t, "fifty")
	ctx := f.ctx
	// Real edges have verified public ingress, so the workspace's edge set exists after the first attach (the fixture's
	// nodes are unverified, and an edge set over no usable edge is never created: every attach rebuilt it).
	f.exec(t, `UPDATE control_tunnel_nodes SET public_ingress_verified_at=now() WHERE id IN ($1,$2)`, f.node, f.backupNode)
	proofs := []previewattachment.MachineProof{f.proof}
	for i := range 49 {
		machine, env, route := fmt.Sprintf("%s_m%02d", f.prefix, i), fmt.Sprintf("%s_e%02d", f.prefix, i), fmt.Sprintf("%s_r%02d", f.prefix, i)
		public, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		f.exec(t, `INSERT INTO control_environments(id,workspace_id,owner_user_id,desired_state) VALUES($1,$1,$2,'active')`, env, f.account)
		f.exec(t, `INSERT INTO user_machines (id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,public_identity_key,installation_generation,worker_generation,configured_capabilities) VALUES ($1,$2,$3,$4,'linux','amd64','/workspace','online','occupied',$5,1,1,ARRAY['terminal_host']::text[])`, machine, f.account, env, fmt.Sprintf("fifty-%02d", i), base64.RawURLEncoding.EncodeToString(public))
		f.exec(t, `INSERT INTO control_routes(id,environment_id,kind,public_host,target_host,target_port) VALUES($1,$2,'runtime_https_wss',$3,'127.0.0.1',38080)`, route, env, machine+".runtime.example.test")
		f.exec(t, `INSERT INTO control_connector_generations(environment_id,machine_id,connector_id,generation,edge_pool,state) VALUES($1,$2,'runtime',1,'default','pending')`, env, machine)
		proofs = append(proofs, previewattachment.MachineProof{UserID: f.account, MachineID: machine, OperationID: fmt.Sprintf("operation_fifty_%02d", i), InstallationGeneration: 1})
	}
	t.Cleanup(func() {
		for _, q := range []string{`DELETE FROM runtime_carrier_attachments WHERE machine_id LIKE $1||'_m%'`, `DELETE FROM user_machines WHERE id LIKE $1||'_m%'`, `DELETE FROM control_environments WHERE id LIKE $1||'_e%'`} {
			_, _ = f.database.SQL().ExecContext(context.Background(), q, f.prefix)
		}
	})
	direct := f.database.Direct().Stat().AcquireCount()
	errs := make(chan error, len(proofs))
	for _, proof := range proofs {
		request := f.request
		request.OperationID = proof.OperationID
		go func() { _, err := f.service.Attach(ctx, proof, request); errs <- err }()
	}
	for range proofs {
		if err := <-errs; err != nil {
			t.Fatalf("attach: %v", err)
		}
	}
	if used := f.database.Direct().Stat().AcquireCount() - direct; used != 0 {
		t.Fatalf("50 attaches took %d direct connections, want 0", used)
	}
	t.Logf("50 attaches at once: %+v", f.database.TransactionStats().Serializable)
}
