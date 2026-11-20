package runtimeattachment

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/previewattachment"
)

// Attach latency of a fleet that renews its carriers (PAPERBOAT_ATTACH_LATENCY=1; staging and the capacity runs, never
// the gate). PAPERBOAT_ATTACH_MACHINES machines of one account (default 120) each attach every
// PAPERBOAT_ATTACH_PERIOD_SECONDS (default 10, jittered) for PAPERBOAT_ATTACH_SECONDS (default 60); the first round
// creates the carriers and is not counted. It logs p50/p95/p99 of Service.Attach and the transaction counters; with
// pg_stat_statements on the database the per-statement time of the steady state is what is left to explain.
func TestAttachLatencyOfRenewingFleetOnPostgres(t *testing.T) {
	if os.Getenv("PAPERBOAT_ATTACH_LATENCY") != "1" {
		t.Skip("set PAPERBOAT_ATTACH_LATENCY=1")
	}
	number := func(name string, fallback int) int {
		if value, err := strconv.Atoi(os.Getenv(name)); err == nil && value > 0 {
			return value
		}
		return fallback
	}
	machines, period, window := number("PAPERBOAT_ATTACH_MACHINES", 120), time.Duration(number("PAPERBOAT_ATTACH_PERIOD_SECONDS", 10))*time.Second, time.Duration(number("PAPERBOAT_ATTACH_SECONDS", 60))*time.Second
	f := newAgentFixture(t, "latency")
	f.exec(t, `UPDATE control_tunnel_nodes SET public_ingress_verified_at=now() WHERE id IN ($1,$2)`, f.node, f.backupNode)
	proofs := []previewattachment.MachineProof{f.proof}
	for i := range machines - 1 {
		machine, env, route := fmt.Sprintf("%s_m%03d", f.prefix, i), fmt.Sprintf("%s_e%03d", f.prefix, i), fmt.Sprintf("%s_r%03d", f.prefix, i)
		public, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		f.exec(t, `INSERT INTO control_environments(id,workspace_id,owner_user_id,desired_state) VALUES($1,$1,$2,'active')`, env, f.account)
		f.exec(t, `INSERT INTO user_machines (id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,public_identity_key,installation_generation,worker_generation,configured_capabilities) VALUES ($1,$2,$3,$4,'linux','amd64','/workspace','online','occupied',$5,1,1,ARRAY['terminal_host']::text[])`, machine, f.account, env, fmt.Sprintf("latency-%03d", i), base64.RawURLEncoding.EncodeToString(public))
		f.exec(t, `INSERT INTO control_routes(id,environment_id,kind,public_host,target_host,target_port) VALUES($1,$2,'runtime_https_wss',$3,'127.0.0.1',38080)`, route, env, machine+".runtime.example.test")
		f.exec(t, `INSERT INTO control_connector_generations(environment_id,machine_id,connector_id,generation,edge_pool,state) VALUES($1,$2,'runtime',1,'default','pending')`, env, machine)
		proofs = append(proofs, previewattachment.MachineProof{UserID: f.account, MachineID: machine, OperationID: fmt.Sprintf("operation_latency_%03d", i), InstallationGeneration: 1})
	}
	t.Cleanup(func() {
		for _, q := range []string{`DELETE FROM runtime_carrier_attachments WHERE machine_id LIKE $1||'_m%'`, `DELETE FROM user_machines WHERE id LIKE $1||'_m%'`} {
			_, _ = f.database.SQL().ExecContext(f.ctx, q, f.prefix)
		}
	})
	// Real edges heartbeat every few seconds; an edge silent for ~40 s is not eligible and every attach would be unavailable.
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		for tick := time.NewTicker(5 * time.Second); ; {
			select {
			case <-stop:
				return
			case <-tick.C:
				_, _ = f.database.SQL().ExecContext(f.ctx, `UPDATE control_tunnel_nodes SET last_heartbeat_at=now(),capacity_observed_at=now() WHERE id IN ($1,$2)`, f.node, f.backupNode)
			}
		}
	}()
	var mu sync.Mutex
	var durations []time.Duration
	var wg sync.WaitGroup
	deadline := time.Now().Add(window)
	for i, proof := range proofs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			counter := 0
			for first := true; time.Now().Before(deadline); first = false {
				request := f.request
				counter++
				request.OperationID = fmt.Sprintf("%s_%d", proof.OperationID, counter)
				proof.OperationID = request.OperationID
				started := time.Now()
				if _, err := f.service.Attach(f.ctx, proof, request); err != nil {
					t.Errorf("attach: %v", err)
					return
				}
				if !first {
					mu.Lock()
					durations = append(durations, time.Since(started))
					mu.Unlock()
				}
				time.Sleep(period*9/10 + time.Duration(i%10)*period/50) // jitter within 10% of the period
			}
		}()
	}
	wg.Wait()
	slices.Sort(durations)
	if len(durations) == 0 {
		t.Fatal("no steady-state attach was timed")
	}
	percentile := func(p int) time.Duration {
		return durations[min(len(durations)-1, len(durations)*p/100)].Round(100 * time.Microsecond)
	}
	stats := f.database.TransactionStats()
	t.Logf("attach latency, %d machines every ~%v for %v: %d attaches, p50 %v p95 %v p99 %v max %v; serializable %+v, read committed %+v",
		machines, period, window, len(durations), percentile(50), percentile(95), percentile(99), durations[len(durations)-1].Round(100*time.Microsecond), stats.Serializable, stats.ReadCommitted)
}
