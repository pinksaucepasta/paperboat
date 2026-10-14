package resourceusage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/workspace"
)

func TestPostgresBandwidthHistory(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL required")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Minute).Add(-time.Minute)
	suffix := strings.ReplaceAll(now.Format("150405"), ".", "") + "_" + strings.ToLower(t.Name())
	a, b := "usage_a_"+suffix, "usage_b_"+suffix
	machine, session, client, node := "machine_"+suffix, "session_"+suffix, "client_"+suffix, "node_"+suffix
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, user := range []string{a, b} {
		exec(`INSERT INTO users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@usage.invalid','active')`, user)
	}
	exec(`INSERT INTO user_machines(id,user_id,environment_id,description,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES($1,$2,$3,'Usage test','linux','amd64','/workspace','online','occupied',true,1)`, machine, a, "env_"+suffix)
	exec(`INSERT INTO cli_client_sessions(id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at) VALUES($1,$2,$1,'Usage','desktop','linux',ARRAY['account:read'],'active',$3,$3)`, client, a, now)
	exec(`INSERT INTO user_machine_access_sessions(credential_kind,id,user_machine_id,user_id,environment_id,cli_client_session_id,http_base_url,helper_terminal_session_id,expires_at,created_at,capabilities) VALUES('control_plane_signed',$1,$2,$3,$4,$5,'','token_usage',$6,$7,ARRAY['terminal','files'])`, session, machine, a, "env_"+suffix, client, now.Add(time.Hour), now.Add(-time.Minute))
	exec(`INSERT INTO control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready) VALUES($1,'test','1.0','usage-epoch','ready',true)`, node)
	exec(`INSERT INTO selfhost_installations(id,node_id,installation_public_key,runtime_hash,runtime_ciphertext,name,capability,scope_kind,scope_id,claim_id,claim_complete) VALUES($1,$2,decode(repeat('aa',32),'hex'),decode(repeat('bb',32),'hex'),decode('01','hex'),'Student relay','relay','account',$3,$1,true)`, "installation_"+suffix, node, a)
	defer func() {
		_, _ = store.SQL().ExecContext(ctx, `DELETE FROM resource_bandwidth_events WHERE workspace_id IN ($1,$2)`, "personal:"+a, "personal:"+b)
		for _, query := range []string{`DELETE FROM native_usage_authorities WHERE machine_id=$1`, `DELETE FROM native_usage_reporting_status WHERE machine_id=$1`, `DELETE FROM user_machine_access_sessions WHERE user_machine_id=$1`, `DELETE FROM user_machines WHERE id=$1`} {
			_, _ = store.SQL().ExecContext(ctx, query, machine)
		}
		_, _ = store.SQL().ExecContext(ctx, `DELETE FROM selfhost_installations WHERE node_id=$1`, node)
		_, _ = store.SQL().ExecContext(ctx, `DELETE FROM control_tunnel_nodes WHERE id=$1`, node)
		_, _ = store.SQL().ExecContext(ctx, `DELETE FROM cli_client_sessions WHERE id=$1`, client)
		_, _ = store.SQL().ExecContext(ctx, `DELETE FROM users WHERE id IN ($1,$2)`, a, b)
	}()

	service := New(store)
	service.Now = func() time.Time { return now.Add(2 * time.Minute) }
	report := NativeReport{AccessSessionID: session, StreamID: "stream1", Consumer: "terminal", Mode: "direct", IntervalStart: now, IntervalEnd: now.Add(30 * time.Second), Upload: 100, Download: 200}
	batch := NativeBatch{Reports: []NativeReport{report}}
	receipt, err := service.RecordNative(ctx, machine, 1, batch)
	if err != nil || receipt.Accepted != 1 {
		t.Fatalf("record: %+v %v", receipt, err)
	}
	// Retries and delayed lower counters cannot inflate or reduce totals.
	if _, err := service.RecordNative(ctx, machine, 1, batch); err != nil {
		t.Fatal(err)
	}
	report.Upload = 50
	report.Download = 80
	if _, err := service.RecordNative(ctx, machine, 1, NativeBatch{Reports: []NativeReport{report}}); err != nil {
		t.Fatal(err)
	}
	report.Mode = "relay"
	report.NodeID = node
	report.Upload = 30
	report.Download = 40
	if _, err := service.RecordNative(ctx, machine, 1, NativeBatch{Reports: []NativeReport{report}}); err != nil {
		t.Fatal(err)
	}
	scope := workspace.Scope{ID: "personal"}
	q := Query{Start: now, End: now.Add(time.Minute)}
	got, err := service.Get(ctx, a, scope, q)
	if err != nil {
		t.Fatal(err)
	}
	if got.Totals != (Bytes{Upload: 130, Download: 240}) || len(got.Resources) != 1 || len(got.Resources[0].Paths) != 2 || len(got.Series) != 1 {
		t.Fatalf("history=%+v", got)
	}
	q.Hosting = "self_hosted"
	got, err = service.Get(ctx, a, scope, q)
	if err != nil || got.Totals != (Bytes{Upload: 30, Download: 40}) || got.Resources[0].Paths[0].NodeID != node {
		t.Fatalf("selfhost=%+v %v", got, err)
	}
	q.Hosting = ""
	got, err = service.Get(ctx, b, scope, q)
	if err != nil || got.Totals != (Bytes{}) || len(got.Resources) != 0 {
		t.Fatalf("cross workspace=%+v %v", got, err)
	}
	q.Start = now.Add(time.Minute)
	q.End = now.Add(2 * time.Minute)
	got, err = service.Get(ctx, a, scope, q)
	if err != nil || got.Totals != (Bytes{}) {
		t.Fatalf("time filter=%+v %v", got, err)
	}
	q.Start = now
	q.End = now.Add(time.Minute)
	q.Mode = "direct"
	got, err = service.Get(ctx, a, scope, q)
	if err != nil || got.Totals != (Bytes{Upload: 100, Download: 200}) {
		t.Fatalf("direct=%+v %v", got, err)
	}
	// A wrong machine generation is rejected independently of valid reports.
	receipt, err = service.RecordNative(ctx, machine, 2, NativeBatch{Reports: []NativeReport{report}})
	if err != nil || len(receipt.RejectedIndices) != 1 {
		t.Fatalf("generation fencing=%+v %v", receipt, err)
	}
	// Revocation caps the stored reporting admission but does not erase earlier
	// authorized bytes. Deleting the access session does not erase that binding.
	exec(`UPDATE user_machine_access_sessions SET revoked_at=$2,state='revoked' WHERE id=$1`, session, now.Add(40*time.Second))
	report.StreamID = "revoked-late"
	report.IntervalEnd = now.Add(50 * time.Second)
	receipt, err = service.RecordNative(ctx, machine, 1, NativeBatch{Reports: []NativeReport{report}})
	if err != nil || len(receipt.RejectedIndices) != 1 {
		t.Fatalf("revoked=%+v %v", receipt, err)
	}
	exec(`DELETE FROM user_machine_access_sessions WHERE id=$1`, session)
	report.StreamID = "retained-earlier"
	report.IntervalEnd = now.Add(20 * time.Second)
	report.Upload = 5
	report.Download = 7
	receipt, err = service.RecordNative(ctx, machine, 1, NativeBatch{Reports: []NativeReport{report}})
	if err != nil || receipt.Accepted != 1 {
		t.Fatalf("retained=%+v %v", receipt, err)
	}
	edgeNode, edgeOperation := "edge_"+suffix, "edge_receipt_"+suffix
	exec(`INSERT INTO control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready) VALUES($1,'test','1.0','usage-epoch','ready',true)`, edgeNode)
	defer func() {
		_, _ = store.SQL().ExecContext(ctx, `DELETE FROM control_usage_receipts WHERE operation_id=$1`, edgeOperation)
		_, _ = store.SQL().ExecContext(ctx, `DELETE FROM control_tunnel_nodes WHERE id=$1`, edgeNode)
	}()
	exec(`INSERT INTO control_usage_receipts(operation_id,edge_node_id,counter_epoch,environment_id,route_id,route_revision,direction,observed_bytes,delta_bytes,interval_start,interval_end,workspace_id) VALUES($1,$2,'epoch','env','retained-route',1,'ingress',111,111,$3,$4,$5)`, edgeOperation, edgeNode, now, now.Add(30*time.Second), "personal:"+a)
	exec(`SELECT project_edge_bandwidth($1)`, edgeOperation)
	exec(`SELECT project_edge_bandwidth($1)`, edgeOperation)
	q.Mode = "edge"
	q.Hosting = "managed"
	got, err = service.Get(ctx, a, scope, q)
	if err != nil || got.Totals != (Bytes{Upload: 111}) || len(got.Resources) != 1 || got.Resources[0].Paths[0].NodeID != edgeNode {
		t.Fatalf("accepted edge projection=%+v %v", got, err)
	}
	terminal, attachment := "terminal_"+suffix, "attachment_"+suffix
	exec(`INSERT INTO user_machine_terminal_sessions(id,user_machine_id,name,launch_cwd,owner_account) VALUES($1,$2,'usage-terminal','/workspace',$3)`, terminal, machine, a)
	defer func() {
		_, _ = store.SQL().ExecContext(ctx, `DELETE FROM user_machine_terminal_sessions WHERE id=$1`, terminal)
	}()
	browserEnd := time.Now().UTC().Add(time.Second)
	browserStart := browserEnd.Truncate(time.Minute)
	if err := service.RecordBrowserAdmission(ctx, attachment, machine, terminal, edgeNode, browserEnd.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	browser := NativeReport{AccessSessionID: attachment, StreamID: attachment, Consumer: "terminal", Mode: "edge", IntervalStart: browserStart, IntervalEnd: browserEnd, Upload: 17, Download: 19}
	receipt, err = service.RecordNative(ctx, machine, 1, NativeBatch{Reports: []NativeReport{browser}})
	if err != nil || receipt.Accepted != 1 {
		t.Fatalf("browser attachment=%+v %v", receipt, err)
	}
	browserQuery := Query{Start: browserStart, End: browserStart.Add(time.Minute), ResourceID: terminal, Mode: "edge"}
	got, err = service.Get(ctx, a, scope, browserQuery)
	if err != nil || got.Totals != (Bytes{Upload: 17, Download: 19}) || got.Resources[0].Paths[0].NodeID != edgeNode {
		t.Fatalf("browser derived edge=%+v %v", got, err)
	}
	browser.NodeID = "different-node"
	receipt, err = service.RecordNative(ctx, machine, 1, NativeBatch{Reports: []NativeReport{browser}})
	if err != nil || len(receipt.RejectedIndices) != 1 {
		t.Fatalf("browser edge fence=%+v %v", receipt, err)
	}
	for _, port := range []int{3000, 4000} {
		serviceSession := session + fmt.Sprint(port)
		exec(`INSERT INTO user_machine_access_sessions(credential_kind,id,user_machine_id,user_id,environment_id,cli_client_session_id,http_base_url,expires_at,created_at,capabilities,machine_service_port,machine_service_source_kind,machine_service_source_id,machine_service_source_generation,machine_service_team_fence) VALUES('control_plane_signed',$1,$2,$3,$4,$5,'',$6,$7,ARRAY['private_access'],$8,'automatic',$2,1,paperboat.machine_service_team_fence($3,$2,'automatic',$2,NULL))`, serviceSession, machine, a, "env_"+suffix, client, now.Add(time.Hour), now.Add(-time.Minute), port)
		serviceReport := NativeReport{AccessSessionID: serviceSession, StreamID: serviceSession, Consumer: "private_tcp", Mode: "direct", IntervalStart: now, IntervalEnd: now.Add(30 * time.Second), Upload: int64(port)}
		receipt, err = service.RecordNative(ctx, machine, 1, NativeBatch{Reports: []NativeReport{serviceReport}})
		if err != nil || receipt.Accepted != 1 {
			t.Fatalf("service port=%d: %+v %v", port, receipt, err)
		}
	}
	got, err = service.Get(ctx, a, scope, Query{Start: now, End: now.Add(time.Minute), ResourceID: machine, Mode: "direct"})
	if err != nil {
		t.Fatal(err)
	}
	routes := map[string]int64{}
	for _, resource := range got.Resources {
		if resource.Kind == "machine_service" {
			routes[resource.RouteID] = resource.Upload
			if resource.ID != machine {
				t.Fatalf("noncanonical service id %+v", resource)
			}
		}
	}
	if routes["tcp:3000"] != 3000 || routes["tcp:4000"] != 4000 || len(routes) != 2 {
		t.Fatalf("distinct services=%+v", got)
	}
	_, err = service.Get(ctx, a, workspace.Scope{ID: "not-a-member", TeamID: "not-a-member"}, q)
	if !errors.Is(err, workspace.ErrForbidden) {
		t.Fatalf("membership=%v", err)
	}
	// Retained admission permits delayed bytes after the access-session row
	// was deleted, but does not expose a foreign private node named by a host.
	exec(`UPDATE selfhost_installations SET scope_id=$2,name='Private outsider relay' WHERE node_id=$1`, node, b)
	report.Mode, report.NodeID, report.StreamID = "relay", node, "foreign-node"
	report.IntervalEnd, report.Upload, report.Download = now.Add(20*time.Second), 3, 4
	receipt, err = service.RecordNative(ctx, machine, 1, NativeBatch{Reports: []NativeReport{report}})
	if err != nil || receipt.Accepted != 1 {
		t.Fatalf("foreign node observation=%+v %v", receipt, err)
	}
	got, err = service.Get(ctx, a, scope, Query{Start: now, End: now.Add(time.Minute), Mode: "relay", Hosting: "unknown"})
	if err != nil || got.Totals != (Bytes{Upload: 3, Download: 4}) || len(got.Resources) != 1 || len(got.Resources[0].Paths) != 1 || got.Resources[0].Paths[0].NodeName != node {
		t.Fatalf("foreign node metadata disclosed or valid bytes lost: %+v %v", got, err)
	}
	// Retirement is not revocation of the owner's right to see earlier usage.
	exec(`UPDATE selfhost_installations SET scope_id=$2,state='removed',name='Student relay' WHERE node_id=$1`, node, a)
	report.StreamID, report.Upload, report.Download = "retired-owned-node", 6, 8
	receipt, err = service.RecordNative(ctx, machine, 1, NativeBatch{Reports: []NativeReport{report}})
	if err != nil || receipt.Accepted != 1 {
		t.Fatalf("retired owner observation=%+v %v", receipt, err)
	}
	got, err = service.Get(ctx, a, scope, Query{Start: now, End: now.Add(time.Minute), Mode: "relay", Hosting: "self_hosted"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, resource := range got.Resources {
		for _, path := range resource.Paths {
			if path.NodeID == node && path.NodeName == "Student relay" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("retired owned relay's historical identity lost")
	}
	t.Run("resource and path continuation", func(t *testing.T) {
		insert := `INSERT INTO resource_bandwidth_events(operation_id,workspace_id,resource_kind,resource_id,resource_name,machine_id,machine_name,mode,hosting,node_id,node_name,upload_bytes,download_bytes,interval_start,interval_end) VALUES($1,$2,'paging',$3,$3,$4,'Machine','relay','managed',$5,$5,1,2,$6,$7)`
		for i := 0; i < 201; i++ {
			id := fmt.Sprintf("page_%03d", i)
			exec(insert, "paging_"+suffix+id, "personal:"+a, id, machine, "node", now, now.Add(30*time.Second))
		}
		for i := 1; i < 33; i++ {
			exec(insert, fmt.Sprintf("paths_%s_%03d", suffix, i), "personal:"+a, "page_000", machine, fmt.Sprintf("node_%03d", i), now, now.Add(30*time.Second))
		}
		exec(insert, "foreign_paging_"+suffix, "personal:"+b, "page_999", machine, "node", now, now.Add(30*time.Second))
		query := Query{Start: now, End: now.Add(time.Minute), ResourceKind: "paging", Limit: 200}
		first, err := service.Get(ctx, a, scope, query)
		if err != nil || len(first.Resources) != 200 || first.Pagination.Total != 201 || first.Pagination.NextOffset == nil || *first.Pagination.NextOffset != 200 || first.Totals != (Bytes{Upload: 233, Download: 466}) {
			t.Fatalf("first page: %+v %v", first, err)
		}
		query.Offset = 200
		last, err := service.Get(ctx, a, scope, query)
		if err != nil || len(last.Resources) != 1 || last.Resources[0].ID != "page_200" || last.Pagination.NextOffset != nil || last.Totals != first.Totals {
			t.Fatalf("last page: %+v %v", last, err)
		}
		empty := ""
		query.Offset = 0
		query.Limit = 1
		query.ResourceID = "page_000"
		query.RouteID = &empty
		query.MachineID = &machine
		paths, err := service.Get(ctx, a, scope, query)
		if err != nil || len(paths.Resources) != 1 || len(paths.Resources[0].Paths) != 32 || paths.Resources[0].PathPagination.Total != 33 || paths.Resources[0].PathPagination.NextOffset == nil || paths.Totals.Upload != 33 {
			t.Fatalf("path first: %+v %v", paths, err)
		}
		query.PathOffset = 32
		tail, err := service.Get(ctx, a, scope, query)
		if err != nil || len(tail.Resources) != 1 || len(tail.Resources[0].Paths) != 1 || tail.Resources[0].PathPagination.NextOffset != nil || tail.Resources[0].Upload != 33 || tail.Totals != paths.Totals {
			t.Fatalf("path tail: %+v %v", tail, err)
		}
		query.PathOffset = 33
		exhausted, err := service.Get(ctx, a, scope, query)
		if err != nil || len(exhausted.Resources) != 1 || len(exhausted.Resources[0].Paths) != 0 || exhausted.Resources[0].PathPagination.Total != 33 {
			t.Fatalf("exhausted: %+v %v", exhausted, err)
		}
		query.MachineID = &empty
		none, err := service.Get(ctx, a, scope, query)
		if err != nil || none.Pagination.Total != 0 || len(none.Resources) != 0 || none.Totals != (Bytes{}) {
			t.Fatalf("exact empty machine: %+v %v", none, err)
		}
	})

}

func TestUsageBounds(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	for _, q := range []Query{{Start: now, End: now}, {Start: now.Add(-MaxWindow - time.Second), End: now}, {Start: now.Add(-time.Hour), End: now, Mode: "requested-relay"}, {Start: now.Add(-time.Hour), End: now, Hosting: "selfhost"}, {Start: now.Add(-time.Hour), End: now, Limit: 201}, {Start: now.Add(-time.Hour), End: now, PathLimit: 33}, {Start: now.Add(-time.Hour), End: now, Offset: -1}} {
		if q.Validate(now) == nil {
			t.Fatalf("accepted %+v", q)
		}
	}
	r := NativeReport{AccessSessionID: "session", StreamID: "stream", Consumer: "file_transfer", Mode: "relay", IntervalStart: now, IntervalEnd: now.Add(time.Second), Upload: 1}
	if err := r.validate(now.Add(time.Minute)); err != nil {
		t.Fatalf("relay unknown node: %v", err)
	}
	r.IntervalStart = r.IntervalStart.Add(time.Second)
	if r.validate(now.Add(time.Minute)) == nil {
		t.Fatal("accepted unbounded interval key")
	}
}
