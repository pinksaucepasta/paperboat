package tunnelv1

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"time"
)

// `pb tunnel logs` reads these rows: a desired-state change and an origin reachability change each leave
// one line, and a repeat of the same state leaves none.
func TestTunnelLogsRecordStateAndOriginChangesOnPostgres(t *testing.T) {
	f := newTRK07PostgresFixture(t)
	ctx := context.Background()
	account, host := f.addAccount(t, "logs")
	tunnel := "tun_logs_" + f.suffix
	created := f.createTunnel(t, account, host, tunnel, "logs-"+f.suffix)
	route := "rte_logs_" + f.suffix
	if _, err := f.database.SQL().ExecContext(ctx, `INSERT INTO paperboat.tunnel_routes(id,tunnel_id,name,protocol,match_type,origin_scheme,origin_address,created_by_actor_id,updated_by_actor_id) VALUES($1,$2,'web','http','catch_all','http','127.0.0.1:3000',$3,$3)`, route, tunnel, account); err != nil {
		t.Fatal(err)
	}
	logs := func() []string {
		t.Helper()
		rows, err := f.repository.ListResourceTunnelLogs(ctx, account, tunnel, 0, 50)
		if err != nil {
			t.Fatal(err)
		}
		var codes []string
		for _, row := range rows {
			codes = append(codes, row.Code)
		}
		return codes
	}
	count := func(code string) int {
		n := 0
		for _, c := range logs() {
			if c == code {
				n++
			}
		}
		return n
	}
	generation := created.Tunnel.Generation
	transition := func(state string) {
		t.Helper()
		hash := sha256.Sum256([]byte("state:" + state + f.suffix))
		tag := fmt.Sprintf("%s_%d_%s", state, generation, f.suffix)
		result, err := f.repository.Transition(ctx, StateRecord{OperationID: "op_" + tag, AuditEventID: "aud_" + tag, TunnelID: tunnel, AccountID: account, DesiredState: state,
			ExpectedGeneration: generation, IdempotencyKey: "state-" + tag, RequestHash: hash, ActorID: account, AuditActorID: account, ActorType: "user", CorrelationID: "corr_" + tag})
		if err != nil {
			t.Fatal(err)
		}
		generation = result.Tunnel.Generation
	}
	transition(DesiredPaused)
	if count("tunnel_paused") != 1 {
		t.Fatalf("pause left %v", logs())
	}
	transition(DesiredPaused) // already paused: no change, no new line
	if count("tunnel_paused") != 1 {
		t.Fatalf("a repeated pause logged again: %v", logs())
	}
	transition(DesiredActive)
	if count("tunnel_resumed") != 1 {
		t.Fatalf("resume left %v", logs())
	}

	// A state change syncs the agent's endpoints, and this fixture agent serves none: add the endpoint
	// the origin reports are about only now.
	if _, err := f.database.SQL().ExecContext(ctx, `INSERT INTO paperboat.endpoints(id,kind,account_id,agent_machine_id,access_mode,lifetime,tunnel_id) VALUES($1,'tunnel',$2,$3,'public','persistent',$1)`, tunnel, account, host); err != nil {
		t.Fatal(err)
	}
	report := func(state, code string, at time.Duration) {
		t.Helper()
		if err := f.repository.RecordOriginHealth(ctx, account, host, "corr_origin_"+f.suffix, []OriginRouteReport{{TunnelID: tunnel, RouteID: route, State: state, Code: code, ObservedAt: f.now.Add(at)}}, f.now.Add(at)); err != nil {
			t.Fatal(err)
		}
	}
	report("unreachable", "refused", 0)
	report("unreachable", "refused", time.Second) // the heartbeat repeat is not a new event
	if count("tunnel_origin_unreachable") != 1 {
		t.Fatalf("unreachable logged %d times: %v", count("tunnel_origin_unreachable"), logs())
	}
	report("ready", "", 2*time.Second)
	if count("tunnel_origin_recovered") != 1 {
		t.Fatalf("recovery logged %d times: %v", count("tunnel_origin_recovered"), logs())
	}
	for _, code := range logs() {
		if strings.TrimSpace(code) == "" {
			t.Fatal("a log line has no code")
		}
	}

	transition(DesiredDeleted)
	if count("tunnel_deleted") != 1 {
		t.Fatalf("delete left %v", logs())
	}
}
