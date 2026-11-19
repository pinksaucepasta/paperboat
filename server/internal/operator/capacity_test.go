package operator

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

func capacityService() *Service {
	return &Service{nodeStaleAfter: 2 * time.Minute, capacity: config.Default().NodeCapacity}
}

func codes(w []NodeWarning) map[string]string {
	out := map[string]string{}
	for _, x := range w {
		out[x.Code] = x.Severity
	}
	return out
}

func ptr[T any](v T) *T { return &v }

func TestNodeWarningsApplyTheThresholds(t *testing.T) {
	s := capacityService()
	now := time.Now()
	fresh := now.Add(-10 * time.Second)
	cases := []struct {
		name string
		node NodeCapacity
		want map[string]string
	}{
		{"healthy edge", NodeCapacity{Kind: "edge", LastHeartbeatAt: &fresh, CPUSustainedPercent: ptr(20.0), BytesMonth: ptr(int64(1e12))}, map[string]string{}},
		{"edge cpu at the threshold", NodeCapacity{Kind: "edge", LastHeartbeatAt: &fresh, CPUSustainedPercent: ptr(50.0), BytesMonth: ptr(int64(0))}, map[string]string{"edge_cpu_high": "warning"}},
		{"relay cpu is not an edge warning", NodeCapacity{Kind: "relay", LastHeartbeatAt: &fresh, CPUSustainedPercent: ptr(90.0), RSSBytes: ptr(int64(1 << 29)), BytesMonth: ptr(int64(0))}, map[string]string{}},
		{"relay memory over 1 GiB", NodeCapacity{Kind: "relay", LastHeartbeatAt: &fresh, RSSBytes: ptr(int64(1 << 30)), BytesMonth: ptr(int64(0))}, map[string]string{"relay_memory_high": "warning"}},
		{"edge memory is not a relay warning", NodeCapacity{Kind: "edge", LastHeartbeatAt: &fresh, RSSBytes: ptr(int64(4 << 30)), CPUSustainedPercent: ptr(1.0), BytesMonth: ptr(int64(0))}, map[string]string{}},
		{"traffic at 80% of 20 TB", NodeCapacity{Kind: "edge", LastHeartbeatAt: &fresh, CPUSustainedPercent: ptr(1.0), BytesMonth: ptr(int64(16e12))}, map[string]string{"traffic_allowance_high": "warning"}},
		{"traffic just under 80%", NodeCapacity{Kind: "relay", LastHeartbeatAt: &fresh, RSSBytes: ptr(int64(1)), BytesMonth: ptr(int64(16e12 - 1))}, map[string]string{}},
		{"traffic past the allowance", NodeCapacity{Kind: "relay", LastHeartbeatAt: &fresh, RSSBytes: ptr(int64(1)), BytesMonth: ptr(int64(21e12))}, map[string]string{"traffic_allowance_exceeded": "critical"}},
		{"stale heartbeat", NodeCapacity{Kind: "edge", LastHeartbeatAt: ptr(now.Add(-5 * time.Minute)), CPUSustainedPercent: ptr(1.0), BytesMonth: ptr(int64(0))}, map[string]string{"heartbeat_stale": "critical"}},
		{"never heartbeat", NodeCapacity{Kind: "edge"}, map[string]string{"no_heartbeat": "critical"}},
		{"server has no traffic allowance", NodeCapacity{Kind: "server", LastHeartbeatAt: &fresh, BytesMonth: ptr(int64(99e12))}, map[string]string{}},
	}
	for _, c := range cases {
		reports := c.node.BytesMonth != nil
		got := codes(s.nodeWarnings(c.node, now, reports))
		if c.name == "never heartbeat" {
			got = codes(s.nodeWarnings(c.node, now, false))
			delete(got, "no_metrics")
		}
		if len(got) != len(c.want) {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
			continue
		}
		for code, severity := range c.want {
			if got[code] != severity {
				t.Errorf("%s: got %v want %v", c.name, got, c.want)
			}
		}
	}
	if got := codes(s.nodeWarnings(NodeCapacity{Kind: "edge", LastHeartbeatAt: &fresh}, now, false)); got["no_metrics"] != "info" {
		t.Errorf("an older build without metrics is an info line: %v", got)
	}
}

func TestCapacityConfigDefaults(t *testing.T) {
	c := config.Default().NodeCapacity
	if c.MonthlyTrafficBytes != 20e12 || c.TrafficWarnFraction != 0.8 || c.EdgeCPUWarnPercent != 50 || c.RelayRSSWarnBytes != 1<<30 {
		t.Fatalf("defaults: %+v", c)
	}
}

// The page reads the registry, the latest sample and the day totals together: today's and the month's bytes, the
// kind from the node's roles or its report, the control-plane server alongside, and warnings from the thresholds.
func TestCapacityPageJoinsRegistryReportsAndDayTotalsOnPostgres(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("PAPERBOAT_TEST_DATABASE_DSN"))
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() }) // registered first, so it runs after the cleanups that use the store
	ctx := context.Background()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	stamp := strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "")
	actorID, edgeID, relayID, silentID, serverID := "usr_cap_op_"+stamp, "cap_edge_"+stamp, "cap_relay_"+stamp, "cap_silent_"+stamp, "server-cap-"+stamp
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO operator_accounts(id,email,password_hash,status,role) VALUES($1,$2,'unused test login','active','operations')`, actorID, actorID+"@example.test")
	now := time.Now().UTC()
	for _, node := range []struct{ id, role string }{{edgeID, "edge"}, {relayID, "relay"}, {silentID, "edge"}} {
		exec(`INSERT INTO control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,endpoint_host,endpoint_tcp_port,endpoint_quic_port,state,ready,last_heartbeat_at,node_generation,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at,site)
 VALUES($1,'default','1.0','e',$1||'.example.test',443,444,'ready',true,$2,1,'hel','hel-a',ARRAY[$3],ARRAY['http3'],1000,12,$2,$4,'Helsinki')`, node.id, now, node.role, now.Add(time.Hour))
	}
	t.Cleanup(func() {
		bg := context.Background()
		store.Pool().Exec(bg, `DELETE FROM control_tunnel_nodes WHERE id IN($1,$2,$3)`, edgeID, relayID, silentID)
		store.Pool().Exec(bg, `DELETE FROM node_capacity WHERE node_id IN($1,$2,$3)`, edgeID, relayID, serverID)
		store.Pool().Exec(bg, `DELETE FROM node_capacity_daily WHERE node_id IN($1,$2,$3)`, edgeID, relayID, serverID)
		store.Pool().Exec(bg, `DELETE FROM operator_accounts WHERE id=$1`, actorID)
	})
	report := func(id, role string, cpu, sustained float64, rss int64) {
		exec(`INSERT INTO node_capacity(node_id,role,process_epoch,version,cpu_percent,cpu_sustained_percent,rss_bytes,connections,bytes_in_total,bytes_out_total,reported_at) VALUES($1,$2,'e','dev-x',$3,$4,$5,9,0,0,$6)`, id, role, cpu, sustained, rss, now)
	}
	report(edgeID, "edge", 80, 65, 200<<20)   // sustained CPU over 50%
	report(relayID, "relay", 5, 5, 2<<30)     // RSS over 1 GiB
	report(serverID, "server", 3, 3, 500<<20) // the control plane itself
	exec(`INSERT INTO node_capacity_daily(node_id,day,bytes_in,bytes_out) VALUES($1,$2::date,$3,$4),($1,$5::date,$6,0)`, edgeID, now.Format("2006-01-02"), int64(1e12), int64(2e12), now.Format("2006-01")+"-01", int64(14e12))
	service := NewService(store)
	service.nodeStaleAfter = time.Hour
	// Walks every page three at a time: the shared database holds other tests' nodes, and paging is under test too.
	byID := map[string]NodeCapacity{}
	for after := ""; ; {
		v, err := service.Read(ctx, auth.User{ID: actorID, Role: auth.Role("operations")}, "capacity", "", after, 3)
		if err != nil {
			t.Fatal(err)
		}
		page := v.(CapacityPage)
		if len(page.Items) > 3 || page.Total < int64(len(byID)+len(page.Items)) {
			t.Fatalf("page of %d items, total %d, after %d seen", len(page.Items), page.Total, len(byID))
		}
		for _, n := range page.Items {
			if n.ID <= after {
				t.Fatalf("page out of order: %q after %q", n.ID, after)
			}
			byID[n.ID] = n
		}
		if page.NextAfter == "" {
			// The summary counts the whole fleet with SQL; it must agree with nodeWarnings over every row walked.
			var want CapacitySummary
			for _, n := range byID {
				switch n.Kind {
				case "edge":
					want.Edges++
				case "relay":
					want.Relays++
				case "server":
					want.Servers++
				}
				for _, w := range n.Warnings {
					if w.Severity != "info" {
						want.Attention++
						break
					}
				}
			}
			if page.Summary != want || page.Total != want.Edges+want.Relays+want.Servers {
				t.Fatalf("summary %+v, nodeWarnings over every page %+v", page.Summary, want)
			}
			break
		}
		after = page.NextAfter
	}
	edge, relay, silent, server := byID[edgeID], byID[relayID], byID[silentID], byID[serverID]
	if edge.Kind != "edge" || relay.Kind != "relay" || server.Kind != "server" || edge.Site != "Helsinki" || edge.Region != "hel" || edge.Version != "dev-x" {
		t.Fatalf("identity: edge=%+v relay=%+v server=%+v", edge, relay, server)
	}
	if edge.BytesToday == nil || *edge.BytesToday != 3e12 || *edge.BytesMonth != 17e12 || edge.Connections == nil || *edge.Connections != 9 {
		t.Fatalf("edge bytes: %+v", edge)
	}
	if got := codes(edge.Warnings); got["edge_cpu_high"] != "warning" || got["traffic_allowance_high"] != "warning" {
		t.Fatalf("edge warnings %v", got)
	}
	if got := codes(relay.Warnings); got["relay_memory_high"] != "warning" || len(got) != 1 {
		t.Fatalf("relay warnings %v", got)
	}
	if silent.BytesMonth != nil || codes(silent.Warnings)["no_metrics"] != "info" || silent.Connections == nil || *silent.Connections != 12 {
		t.Fatalf("a node that reports nothing: %+v", silent)
	}
	if server.LastHeartbeatAt == nil || len(server.Warnings) != 0 {
		t.Fatalf("server row: %+v", server)
	}
	if _, err := service.Read(ctx, auth.User{ID: actorID, Role: auth.Role("operations")}, "capacity", "x", "", 100); err != nil {
		t.Fatalf("capacity ignores target/after: %v", err)
	}
}

func TestBytesTextScalesToTheLargestUnit(t *testing.T) {
	for n, want := range map[float64]string{0: "0 B", 999: "999 B", 1500: "1.5 KB", 16e9: "16.0 GB", 17e12: "17.0 TB", 20e12: "20.0 TB"} {
		if got := bytesText(n); got != want {
			t.Errorf("bytesText(%v)=%q want %q", n, got, want)
		}
	}
	s := capacityService()
	w := s.nodeWarnings(NodeCapacity{Kind: "edge", LastHeartbeatAt: ptr(time.Now()), CPUSustainedPercent: ptr(1.0), BytesMonth: ptr(int64(17e12))}, time.Now(), true)
	if len(w) != 1 || w[0].Message != "This month's traffic 17.0 TB is 85% of the 20.0 TB allowance." {
		t.Fatalf("warning text: %+v", w)
	}
}
