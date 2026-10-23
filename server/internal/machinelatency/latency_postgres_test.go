package machinelatency_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/machinelatency"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
)

func TestMachineLatencyPostgresLifecycle(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN")
	}
	if e := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); e != nil {
		t.Fatal(e)
	}
	store, e := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if os.Getenv("PAPERBOAT_TEST_SCHEMA_READY") != "1" {
		if e = db.Migrate(ctx, store); e != nil {
			t.Fatal(e)
		}
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	owner, other, machine, environment := "lat-owner-"+suffix, "lat-other-"+suffix, "lat-machine-"+suffix, "lat-env-"+suffix
	edgeNode, relayNode, privateRelay, plainNode := "lat-edge-"+suffix, "lat-relay-"+suffix, "lat-private-relay-"+suffix, "lat-plain-"+suffix
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := store.SQL().ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@invalid.test','active'),($2,$2,$2||'@invalid.test','active')`, owner, other)
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id) VALUES($1,$1,$2)`, environment, owner)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES($1,$2,$3,'office','linux','amd64','/workspace','online','occupied',true,1)`, machine, owner, environment)
	for _, node := range []struct {
		id, role string
		allowed  []string
	}{{edgeNode, "edge", nil}, {relayNode, "relay", nil}, {privateRelay, "relay", []string{other}}, {plainNode, "", nil}} {
		if node.role == "" {
			exec(`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch) VALUES($1,'p','v1','epoch_1')`, node.id)
			continue
		}
		exec(`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at,allowed_account_ids)
VALUES($1,'p','v1','epoch_1','india','fd-1',$2,ARRAY['http2'],10,0,now(),now()+interval '1 hour',$3)`, node.id, []string{node.role}, node.allowed)
	}
	// Paperboat's own nodes are installed with the self-host tool too (global scope); only a customer's
	// account-scoped installation makes a node "self-hosted" in the listing.
	for node, scope := range map[string]string{relayNode: "global", privateRelay: "account", edgeNode: "global"} {
		installationKey := sha256.Sum256([]byte("lat-key-" + node))
		exec(`INSERT INTO paperboat.selfhost_installations(id,node_id,installation_public_key,runtime_hash,runtime_ciphertext,name,capability,scope_kind,scope_id,claim_id,claim_complete) VALUES($1,$2,$3,$3,'fixture','lat node','relay',$4,$5,$1,true)`, "lat-inst-"+node, node, installationKey[:], scope, map[string]string{"global": "global", "account": other}[scope])
	}
	now := time.Now().UTC()
	service := &machinelatency.Service{DB: store.SQL(), Now: func() time.Time { return now }}

	// Relay reports: only relays this account may use, in range, without duplicates.
	for name, samples := range map[string][]machinelatency.RelaySample{
		"an edge node":              {{NodeID: edgeNode, RTTMillis: 5}},
		"a node with no role":       {{NodeID: plainNode, RTTMillis: 5}},
		"another account's relay":   {{NodeID: privateRelay, RTTMillis: 5}},
		"an unknown node":           {{NodeID: "lat-missing-" + suffix, RTTMillis: 5}},
		"an out-of-range time":      {{NodeID: relayNode, RTTMillis: 10001}},
		"a negative time":           {{NodeID: relayNode, RTTMillis: -1}},
		"a duplicated node":         {{NodeID: relayNode, RTTMillis: 5}, {NodeID: relayNode, RTTMillis: 6}},
		"nothing":                   {},
		"a good and a rejected one": {{NodeID: relayNode, RTTMillis: 5}, {NodeID: edgeNode, RTTMillis: 5}},
	} {
		if err := service.SaveRelays(ctx, owner, machine, samples); !errors.Is(err, machinelatency.ErrInvalid) {
			t.Fatalf("%s was accepted: %v", name, err)
		}
	}
	if items, err := service.List(ctx, machine); err != nil || len(items) != 0 {
		t.Fatalf("a rejected report left rows: %v %v", items, err)
	}
	if err := service.SaveRelays(ctx, owner, machine, []machinelatency.RelaySample{{NodeID: relayNode, RTTMillis: 42}}); err != nil {
		t.Fatal(err)
	}
	// The account a relay is reserved for can report it, for its own machine.
	otherEnvironment, otherMachine := "lat-env2-"+suffix, "lat-machine2-"+suffix
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id) VALUES($1,$1,$2)`, otherEnvironment, other)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES($1,$2,$3,'laptop','linux','amd64','/workspace','online','occupied',true,1)`, otherMachine, other, otherEnvironment)
	if err := service.SaveRelays(ctx, other, otherMachine, []machinelatency.RelaySample{{NodeID: privateRelay, RTTMillis: 7}}); err != nil {
		t.Fatalf("the account a relay is reserved for must be able to report it: %v", err)
	}
	if items, err := service.List(ctx, machine); err != nil || len(items) != 1 {
		t.Fatalf("another machine's report leaked into this listing: %v %v", items, err)
	}
	if items, err := service.List(ctx, otherMachine); err != nil || len(items) != 1 || !items[0].SelfHosted {
		t.Fatalf("a customer's account-scoped relay must be listed as self-hosted: %+v %v", items, err)
	}

	// Edge reports ride the runtime attachment: only candidates issued in that request count.
	candidates := []runtimewire.EdgeCandidate{{NodeID: edgeNode, ProcessEpoch: "epoch_1", Region: "india"}}
	err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		return machinelatency.SaveEdgesTx(ctx, tx, machine, []runtimewire.EdgeLatency{
			{NodeID: edgeNode, ProcessEpoch: "epoch_1", RTTMilliseconds: 20},
			{NodeID: relayNode, ProcessEpoch: "epoch_1", RTTMilliseconds: 1},    // not issued: ignored
			{NodeID: edgeNode, ProcessEpoch: "epoch_other", RTTMilliseconds: 2}, // wrong epoch: ignored
			{NodeID: edgeNode, ProcessEpoch: "epoch_1", RTTMilliseconds: 99999}, // out of range: ignored
		}, candidates, now)
	})
	if err != nil {
		t.Fatal(err)
	}
	items, err := service.List(ctx, machine)
	if err != nil || len(items) != 2 {
		t.Fatalf("items=%v err=%v", items, err)
	}
	if items[0].SelfHosted || items[1].SelfHosted {
		t.Fatalf("Paperboat's own (global) nodes were listed as self-hosted: %+v", items)
	}
	if items[0].Role != "edge" || items[0].RTTMillis != 20 || items[0].Region != "india" || items[1].Role != "relay" || items[1].RTTMillis != 42 {
		t.Fatalf("listing wrong or unordered: %+v", items)
	}

	// A newer report replaces the older number; a stale measurement is hidden.
	if err := service.SaveRelays(ctx, owner, machine, []machinelatency.RelaySample{{NodeID: relayNode, RTTMillis: 17}}); err != nil {
		t.Fatal(err)
	}
	if items, _ = service.List(ctx, machine); items[1].RTTMillis != 17 {
		t.Fatalf("report did not replace the earlier number: %+v", items)
	}
	later := &machinelatency.Service{DB: store.SQL(), Now: func() time.Time { return now.Add(machinelatency.Fresh + time.Minute) }}
	if items, err = later.List(ctx, machine); err != nil || len(items) != 0 {
		t.Fatalf("stale measurements are still listed: %v %v", items, err)
	}
	// Deleting the machine removes its measurements.
	exec(`DELETE FROM paperboat.user_machines WHERE id=ANY($1)`, []string{machine, otherMachine})
	var left int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.machine_node_latency WHERE machine_id=$1`, machine).Scan(&left); err != nil || left != 0 {
		t.Fatalf("rows left after machine delete: %d %v", left, err)
	}
}
