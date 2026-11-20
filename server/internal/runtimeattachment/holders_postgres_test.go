package runtimeattachment

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pinksaucepasta/paperboat/shared/nodemesh"
)

var holderSessions atomic.Uint64

// manage makes the fixture's nodes managed (a live authority row in an enabled region), removed again before the fixture.
func (f *agentFixture) manage(t *testing.T, nodes ...string) {
	t.Helper()
	region := "holders_" + strings.ReplaceAll(f.prefix, "_", "-")
	f.exec(t, `INSERT INTO operator_regions(id,display_name,enabled) VALUES($1,'Holders test',true) ON CONFLICT DO NOTHING`, region)
	t.Cleanup(func() {
		for _, n := range nodes {
			_, _ = f.database.SQL().ExecContext(f.ctx, `DELETE FROM managed_node_authority WHERE node_id=$1`, n)
		}
		_, _ = f.database.SQL().ExecContext(f.ctx, `DELETE FROM operator_regions WHERE id=$1`, region)
	})
	for _, n := range nodes {
		f.exec(t, `INSERT INTO managed_node_authority(node_id,region_id,runtime_hash,runtime_ciphertext,config) VALUES($1,$2,sha256(convert_to($1::text,'UTF8')),'encrypted','{}'::jsonb)`, n, region)
	}
}

// carrier attaches the machine's runtime route on node (the second node of a route is the inactive one) and returns the carrier session;
// a non-empty hostname is the runtime route's public name in its admission.
func (f *agentFixture) carrier(t *testing.T, node, epoch string, active bool, hostname string) string {
	t.Helper()
	session := fmt.Sprintf("session_00000000-0000-4000-8000-%012d", holderSessions.Add(1)+uint64(time.Now().UnixNano()%1e9)*1000)
	name := ""
	if hostname != "" {
		name = `"hostname":"` + hostname + `",`
	}
	admission := fmt.Sprintf(`{%s"binding":{"tunnel_id":"t","connector_id":"c","config_generation":1},"config_content_hash":"sha256:%s","edge_endpoints":[]}`, name, strings.Repeat("a", 64))
	f.exec(t, `INSERT INTO runtime_carrier_attachments(route_id,machine_id,account_id,installation_generation,worker_generation,process_id,generation,edge_node_id,edge_process_epoch,public_identity_key,route_revision,session_id,preview_session_id,admission,expires_at,active,ready,agent_capabilities)
VALUES($1,$2,$3,1,1,'process_holders',1,$4,$5,$6,1,$7,$7,$8::jsonb,now()+interval '20 minutes',$9,true,ARRAY['endpoints','tunnel_endpoints']::text[])`, f.route, f.machine, f.account, node, epoch, f.key, session, admission, active)
	var valid bool
	if err := f.database.SQL().QueryRowContext(f.ctx, `SELECT paperboat.runtime_carrier_valid($1,$2,$3,false)`, f.route, node, epoch).Scan(&valid); err != nil || !valid {
		t.Fatalf("fixture carrier on %s is not valid (valid=%v err=%v)", node, valid, err)
	}
	return session
}

func dash(tag string) string { return strings.ReplaceAll(tag, "_", "-") }

func previewHost(tag string, i int) string {
	return fmt.Sprintf("%s-%d.preview.example.test", dash(tag), i)
}

// previews creates n preview endpoints named previewHost(tag, 1..n) of the fixture's machine.
func (f *agentFixture) previews(t *testing.T, tag string, n int) {
	t.Helper()
	ids := `SELECT $1::text||'_'||g AS id, g FROM generate_series(1,$2::int) g`
	f.exec(t, `INSERT INTO preview_leases(id,endpoint,account_id,actor_id,owner_machine_id,owner_session_id,owner_session_kind,target_scheme,target_address,access_mode,lease_deadline)
SELECT i.id,'https://'||replace($1::text,'_','-')||'-'||i.g||'.preview.example.test',$3,$3,$4,'owner_session_endpoint','foreground','http','127.0.0.1:3000','team',now()+interval '10 minutes' FROM (`+ids+`) i`, tag, n, f.account, f.machine)
	f.exec(t, `INSERT INTO endpoints(id,kind,account_id,agent_machine_id,access_mode,lifetime,preview_lease_id) SELECT i.id,'preview',$3,$4,'team','lease',i.id FROM (`+ids+`) i`, tag, n, f.account, f.machine)
	f.exec(t, `INSERT INTO endpoint_routes(id,endpoint_id,hostname,protocol) SELECT i.id,i.id,replace($1::text,'_','-')||'-'||i.g||'.preview.example.test','http' FROM (`+ids+`) i`, tag, n)
}

// serve assigns the previews of tag to node, observed ready or not.
func (f *agentFixture) serve(t *testing.T, tag string, n int, node, epoch, session string, ready bool) {
	t.Helper()
	f.exec(t, `INSERT INTO endpoint_edge_assignments(route_id,endpoint_id,edge_node_id,edge_process_epoch,carrier_session_id,generation,observed_ready,observed_at)
SELECT $1::text||'_'||g,$1::text||'_'||g,$3,$4,$5,1,$6,now() FROM generate_series(1,$2::int) g`, tag, n, node, epoch, session, ready)
}

// allHolders pages the whole snapshot the way an edge does, checking every page's bound.
func allHolders(t *testing.T, f *agentFixture, caller string, limit int) (map[string][]string, int) {
	t.Helper()
	out, page, pages := map[string][]string{}, "", 0
	for {
		holders, next, err := f.service.Holders(f.ctx, caller, page, limit)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		if limit != 0 && len(holders) > limit || len(holders) > nodemesh.MaxHolderPage {
			t.Fatalf("page of %d holders exceeds the limit %d", len(holders), limit)
		}
		for _, h := range holders {
			if _, dup := out[h.Name]; dup {
				t.Fatalf("key %q served twice", h.Name)
			}
			out[h.Name] = h.NodeIDs
		}
		if next == "" {
			return out, pages
		}
		if next <= page {
			t.Fatalf("cursor %q does not advance past %q", next, page)
		}
		page = next
	}
}

func TestHoldersOnlyManagedNodesAndValidCarriers(t *testing.T) {
	f := newAgentFixture(t, "holdmgd")
	f.manage(t, f.node) // the backup node stays unmanaged
	runtimeHost := f.machine + ".runtime.example.test"
	sessionA := f.carrier(t, f.node, "epoch_endpoint", true, runtimeHost)
	sessionB := f.carrier(t, f.backupNode, "epoch_backup", false, runtimeHost)
	f.previews(t, f.prefix+"_ok", 1)
	f.serve(t, f.prefix+"_ok", 1, f.node, "epoch_endpoint", sessionA, true)
	f.serve(t, f.prefix+"_ok", 1, f.backupNode, "epoch_backup", sessionB, true)
	f.previews(t, f.prefix+"_waiting", 1)
	f.serve(t, f.prefix+"_waiting", 1, f.node, "epoch_endpoint", sessionA, false)
	okHost := previewHost(f.prefix+"_ok", 1)

	got, _ := allHolders(t, f, f.node, 0)
	want := map[string][]string{runtimeHost: {f.node}, okHost: {f.node}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("holders = %v, want %v: the unmanaged node never holds, a route its edge has not observed ready is not held", got, want)
	}
	// A caller that is not a managed node (a self-hosted edge) is told nothing, and so is an unknown one.
	for _, caller := range []string{f.backupNode, "no_such_node_" + f.prefix} {
		if got, _ := allHolders(t, f, caller, 0); len(got) != 0 {
			t.Fatalf("non-managed caller %s got %v", caller, got)
		}
	}
	// A machine that was revoked no longer has a valid carrier: its names go with it.
	f.exec(t, `UPDATE user_machines SET revoked_at=now() WHERE id=$1`, f.machine)
	if got, _ := allHolders(t, f, f.node, 0); len(got) != 0 {
		t.Fatalf("holders of a revoked machine's carrier = %v", got)
	}
	if _, _, err := f.service.Holders(f.ctx, f.node, "UPPER", 0); err == nil {
		t.Fatal("an invalid cursor was accepted")
	}
	if _, _, err := f.service.Holders(f.ctx, f.node, "", nodemesh.MaxHolderPage+1); err == nil {
		t.Fatal("a page over the bound was accepted")
	}
}

func TestHoldersMovedCarrierShowsNewNode(t *testing.T) {
	f := newAgentFixture(t, "holdmove")
	f.manage(t, f.node, f.backupNode)
	sessionA := f.carrier(t, f.node, "epoch_endpoint", true, "")
	f.previews(t, f.prefix+"_mv", 1)
	f.serve(t, f.prefix+"_mv", 1, f.node, "epoch_endpoint", sessionA, true)
	host := previewHost(f.prefix+"_mv", 1)
	if got, _ := allHolders(t, f, f.backupNode, 0); fmt.Sprint(got) != fmt.Sprint(map[string][]string{host: {f.node}}) {
		t.Fatalf("before the move: %v", got)
	}
	f.exec(t, `DELETE FROM endpoint_edge_assignments WHERE edge_node_id=$1`, f.node)
	f.exec(t, `DELETE FROM runtime_carrier_attachments WHERE edge_node_id=$1`, f.node)
	sessionB := f.carrier(t, f.backupNode, "epoch_backup", true, "")
	f.serve(t, f.prefix+"_mv", 1, f.backupNode, "epoch_backup", sessionB, true)
	if got, _ := allHolders(t, f, f.node, 0); fmt.Sprint(got) != fmt.Sprint(map[string][]string{host: {f.backupNode}}) {
		t.Fatalf("after the move: %v, want the new node only", got)
	}
}

func TestHoldersCursorStableUnderInsert(t *testing.T) {
	f := newAgentFixture(t, "holdcur")
	f.manage(t, f.node)
	session := f.carrier(t, f.node, "epoch_endpoint", true, "")
	mid := f.prefix + "_b"
	f.previews(t, mid, 10)
	f.serve(t, mid, 10, f.node, "epoch_endpoint", session, true)
	var want []string
	for i := 1; i <= 10; i++ {
		want = append(want, previewHost(mid, i))
	}
	sort.Strings(want)
	first, next, err := f.service.Holders(f.ctx, f.node, "", 4)
	if err != nil || len(first) != 4 || next != first[3].Name {
		t.Fatalf("first page %v next %q err %v", first, next, err)
	}
	// Names sorting before and after the cursor appear while the walk is in progress.
	for _, tag := range []string{f.prefix + "_a", f.prefix + "_z"} {
		f.previews(t, tag, 1)
		f.serve(t, tag, 1, f.node, "epoch_endpoint", session, true)
	}
	seen := []string{first[0].Name, first[1].Name, first[2].Name, first[3].Name}
	for page := next; page != ""; {
		var holders []nodemesh.Holder
		if holders, page, err = f.service.Holders(f.ctx, f.node, page, 4); err != nil {
			t.Fatal(err)
		}
		for _, h := range holders {
			seen = append(seen, h.Name)
		}
	}
	if !sort.StringsAreSorted(seen) || len(slices.Compact(slices.Clone(seen))) != len(seen) {
		t.Fatalf("walk is not one ascending pass: %v", seen)
	}
	for _, name := range want {
		if !slices.Contains(seen, name) {
			t.Fatalf("%s missed by the walk: %v", name, seen)
		}
	}
	if !slices.Contains(seen, previewHost(f.prefix+"_z", 1)) || slices.Contains(seen, previewHost(f.prefix+"_a", 1)) {
		t.Fatalf("a name inserted after the cursor must appear, one before it must not: %v", seen)
	}
}

func TestHoldersPagesAcrossMoreThan4096Names(t *testing.T) {
	f := newAgentFixture(t, "holdbig")
	f.manage(t, f.node, f.backupNode)
	sessionA := f.carrier(t, f.node, "epoch_endpoint", true, "")
	sessionB := f.carrier(t, f.backupNode, "epoch_backup", false, "")
	tag := f.prefix + "_big"
	f.previews(t, tag, 5000)
	f.serve(t, tag, 5000, f.node, "epoch_endpoint", sessionA, true)
	f.serve(t, tag, 5000, f.backupNode, "epoch_backup", sessionB, true)
	for _, table := range []string{"preview_leases", "endpoints", "endpoint_routes", "endpoint_edge_assignments"} {
		f.exec(t, "ANALYZE "+table) // the planner needs the sizes the bulk insert just made, as production's autovacuum gives it
	}
	first, cancel := context.WithTimeout(f.ctx, 10*time.Second)
	_, _, err := f.service.Holders(first, f.node, "", nodemesh.MaxHolderPage)
	cancel()
	if err != nil {
		f.explain(t, keyPage, "", nodemesh.MaxHolderPage+1)
		f.explain(t, validHolders, "", "zzzz")
		t.Fatalf("first page: %v", err)
	}
	got, pages := allHolders(t, f, f.node, nodemesh.MaxHolderPage)
	if len(got) != 5000 || pages < 10 || pages > 11 {
		t.Fatalf("%d names in %d pages, want 5000 in 10 or 11", len(got), pages)
	}
	for i := 1; i <= 5000; i++ {
		nodes := got[previewHost(tag, i)]
		if len(nodes) != 2 || !slices.Contains(nodes, f.node) || !slices.Contains(nodes, f.backupNode) {
			t.Fatalf("%s held by %v, want both nodes", previewHost(tag, i), nodes)
		}
	}
}

// A public raw TCP route is held under tcp:<port>, a TLS-passthrough route under its hostname and custom domains like any name, a private TCP route
// under nothing; the keys of both kinds come in one ascending order.
func TestHoldersIncludeTCPKeysAndPassthroughNames(t *testing.T) {
	f := newAgentFixture(t, "holdtcp")
	f.manage(t, f.node)
	session := f.carrier(t, f.node, "epoch_endpoint", true, "")
	tunnel, label := "tunnel_"+f.prefix, uuid.NewString()
	stable := label + ".tunnels.example.test"
	custom := "custom-" + f.prefix + ".example.test"
	f.exec(t, `INSERT INTO tunnels(id,account_id,name,desired_state,access_mode,generation,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id) VALUES($1,$2,$1,'active','public',1,$3,$4,$5,$2)`,
		tunnel, f.account, "endpoint_"+label, "https://"+stable, f.machine)
	route := func(suffix, protocol, extra string) string {
		id := suffix + "_" + f.prefix
		f.exec(t, `INSERT INTO tunnel_routes(id,tunnel_id,name,priority,protocol,match_type,match_hostname,origin_scheme,origin_address,tls_verification,generation,desired_state,created_by_actor_id,updated_by_actor_id,created_at,updated_at,public_tcp_listener_id,public_tcp_port,preserve_host)
VALUES($1,$2,$3,5,$4,`+extra+`,'tcp','127.0.0.1:1','not_applicable',1,'active',$5,$5,now(),now(),`+map[string]string{"tcp": "'listener_'||$1,24022", "tls": "'listener_tls_443',443", "private_tcp": "NULL,NULL"}[protocol]+`,false)`, id, tunnel, suffix, protocol, f.account)
		return id
	}
	tcp := route("tcp", "tcp", `'managed','`+stable+`'`)
	tls := route("tls", "tls", `'managed','`+stable+`'`)
	private := route("priv", "private_tcp", `'catch_all',NULL`)
	f.exec(t, `INSERT INTO endpoints(id,kind,account_id,agent_machine_id,access_mode,lifetime,tunnel_id) VALUES($1,'tunnel',$2,$3,'public','persistent',$1)`, tunnel, f.account, f.machine)
	f.exec(t, `INSERT INTO tunnel_domains(id,account_id,tunnel_id,route_id,hostname,match_type,ownership_challenge_reference,ownership_state,dns_target,certificate_strategy,certificate_state,caa_state,conflict_state) VALUES($1,$2,$3,$4,$5,'exact',$1,'verified',$5,'none','not_applicable','ready','clear')`, "domain_"+f.prefix, f.account, tunnel, tls, custom)
	for id, host := range map[string]string{tcp: stable, tls: stable, private: stable} {
		f.exec(t, `INSERT INTO endpoint_routes(id,endpoint_id,hostname,protocol,tunnel_route_id) SELECT $1,$2,$3,protocol,id FROM tunnel_routes WHERE id=$1`, id, tunnel, host)
		f.exec(t, `INSERT INTO endpoint_edge_assignments(route_id,endpoint_id,edge_node_id,edge_process_epoch,carrier_session_id,generation,observed_ready,observed_at) VALUES($1,$2,$3,'epoch_endpoint',$4,1,true,now())`, id, tunnel, f.node, session)
	}
	holders, _, err := f.service.Holders(f.ctx, f.node, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, h := range holders {
		names = append(names, h.Name)
		if len(h.NodeIDs) != 1 || h.NodeIDs[0] != f.node || h.Validate() != nil {
			t.Fatalf("holder %+v", h)
		}
	}
	want := []string{nodemesh.TCPKey(24022), custom, stable}
	sort.Strings(want) // key order is byte order, whatever the kind
	if fmt.Sprint(names) != fmt.Sprint(want) {
		t.Fatalf("keys %v, want %v: the tcp port key, the passthrough route's custom domain and its hostname, no key for the private route", names, want)
	}
	// The walk across both kinds is one ascending pass whatever the page size.
	got, _ := allHolders(t, f, f.node, 1)
	if len(got) != 3 {
		t.Fatalf("one key per page walk: %v", got)
	}
}

// TestWildcardHolderSeeded: a one-label wildcard custom domain and a one-label wildcard route are held under their *.suffix
// keys by the carrier's node, and the exact hostname beside them is held as before.
func TestWildcardHolderSeeded(t *testing.T) {
	f := newAgentFixture(t, "holdwild")
	f.manage(t, f.node)
	session := f.carrier(t, f.node, "epoch_endpoint", true, "")
	tunnel, label := "tunnel_"+f.prefix, uuid.NewString()
	stable := label + ".tunnels.example.test"
	dns := strings.ReplaceAll(f.prefix, "_", "-") // endpoint_routes.hostname allows no underscore
	domainSuffix := "wild-" + dns + ".example.test"
	routeSuffix := "routes-" + dns + ".example.test"
	f.exec(t, `INSERT INTO tunnels(id,account_id,name,desired_state,access_mode,generation,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id) VALUES($1,$2,$1,'active','public',1,$3,$4,$5,$2)`,
		tunnel, f.account, "endpoint_"+label, "https://"+stable, f.machine)
	exact, wild := "exact_"+f.prefix, "wild_"+f.prefix
	f.exec(t, `INSERT INTO tunnel_routes(id,tunnel_id,name,priority,protocol,match_type,match_hostname,wildcard_suffix,origin_scheme,origin_address,tls_verification,generation,desired_state,created_by_actor_id,updated_by_actor_id,created_at,updated_at,public_tcp_listener_id,public_tcp_port,preserve_host)
VALUES($1,$2,$1,5,'http','managed',$3,NULL,'http','127.0.0.1:1','not_applicable',1,'active',$4,$4,now(),now(),NULL,NULL,false),
($5,$2,$5,5,'http','one_label_wildcard',NULL,$6,'http','127.0.0.1:1','not_applicable',1,'active',$4,$4,now(),now(),NULL,NULL,false)`, exact, tunnel, stable, f.account, wild, routeSuffix)
	f.exec(t, `INSERT INTO endpoints(id,kind,account_id,agent_machine_id,access_mode,lifetime,tunnel_id) VALUES($1,'tunnel',$2,$3,'public','persistent',$1)`, tunnel, f.account, f.machine)
	f.exec(t, `INSERT INTO tunnel_domains(id,account_id,tunnel_id,route_id,hostname,match_type,ownership_challenge_reference,ownership_state,dns_target,certificate_strategy,certificate_state,caa_state,conflict_state) VALUES($1,$2,$3,$4,$5,'one_label_wildcard',$1,'verified',$5,'none','not_applicable','ready','clear')`, "domain_"+f.prefix, f.account, tunnel, exact, "*."+domainSuffix)
	// endpoint_routes.hostname is always a concrete name (endpoint_routes_hostname_check), so the wildcard route's own
	// endpoint hostname is a name under the suffix; the seed announces it as before, beside the wildcard key.
	app := "app." + routeSuffix
	for id, host := range map[string]string{exact: stable, wild: app} {
		f.exec(t, `INSERT INTO endpoint_routes(id,endpoint_id,hostname,protocol,tunnel_route_id) SELECT $1,$2,$3,protocol,id FROM tunnel_routes WHERE id=$1`, id, tunnel, host)
		f.exec(t, `INSERT INTO endpoint_edge_assignments(route_id,endpoint_id,edge_node_id,edge_process_epoch,carrier_session_id,generation,observed_ready,observed_at) VALUES($1,$2,$3,'epoch_endpoint',$4,1,true,now())`, id, tunnel, f.node, session)
	}
	holders, _, err := f.service.Holders(f.ctx, f.node, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, h := range holders {
		names = append(names, h.Name)
		if len(h.NodeIDs) != 1 || h.NodeIDs[0] != f.node || h.Validate() != nil {
			t.Fatalf("holder %+v", h)
		}
	}
	want := []string{stable, app, nodemesh.WildcardKey(domainSuffix), nodemesh.WildcardKey(routeSuffix)}
	sort.Strings(want)
	if fmt.Sprint(names) != fmt.Sprint(want) {
		t.Fatalf("keys %v, want %v: the exact hostnames, the wildcard custom domain and the wildcard route as *.suffix keys", names, want)
	}
}

// alias adds a preview domain (a custom-domain alias) to the preview lease previewID, in the given certificate state.
func (f *agentFixture) alias(t *testing.T, domainID, previewID, hostname, matchType, certificateState string) {
	t.Helper()
	f.exec(t, `INSERT INTO preview_domains(id,account_id,preview_id,preview_generation,hostname,match_type,ownership_challenge_reference,ownership_state,dns_target,certificate_strategy,certificate_reference,certificate_state,caa_state,conflict_state)
SELECT $1,$2,p.id,p.generation,$3,$4,$1,'verified',$3,'managed','cert-'||$1,$5,'ready','clear' FROM preview_leases p WHERE p.id=$6`, domainID, f.account, hostname, matchType, certificateState, previewID)
}

// TestPreviewAliasHolderSeeded: the ready custom-domain aliases of a preview (exact and one-label wildcard) are held by the
// preview's carrier node exactly like its own hostname; an alias whose certificate is not ready, one of a preview whose
// route its edge has not observed ready, and one of an expired preview are not.
func TestPreviewAliasHolderSeeded(t *testing.T) {
	f := newAgentFixture(t, "holdalias")
	f.manage(t, f.node)
	t.Cleanup(func() {
		_, _ = f.database.SQL().ExecContext(f.ctx, `DELETE FROM preview_domains WHERE account_id=$1`, f.account)
	})
	session := f.carrier(t, f.node, "epoch_endpoint", true, "")
	dns := dash(f.prefix)
	ok, waiting, expired := f.prefix+"_ok", f.prefix+"_waiting", f.prefix+"_expired"
	for _, tag := range []string{ok, waiting, expired} {
		f.previews(t, tag, 1)
		f.serve(t, tag, 1, f.node, "epoch_endpoint", session, tag != waiting)
	}
	f.exec(t, `UPDATE preview_leases SET created_at=now()-interval '2 minutes', lease_deadline=now()-interval '1 minute' WHERE id=$1`, expired+"_1")
	exact, wild := "app-"+dns+".example.test", "wild-"+dns+".example.test"
	f.alias(t, "exact_"+f.prefix, ok+"_1", exact, "exact", "ready")
	f.alias(t, "wild_"+f.prefix, ok+"_1", "*."+wild, "one_label_wildcard", "ready")
	f.alias(t, "pending_"+f.prefix, ok+"_1", "pending-"+dns+".example.test", "exact", "issuing")
	f.alias(t, "waiting_"+f.prefix, waiting+"_1", "waiting-"+dns+".example.test", "exact", "ready")
	f.alias(t, "expired_"+f.prefix, expired+"_1", "expired-"+dns+".example.test", "exact", "ready")

	got, _ := allHolders(t, f, f.node, 0)
	want := map[string][]string{
		previewHost(ok, 1): {f.node}, exact: {f.node}, nodemesh.WildcardKey(wild): {f.node},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("holders = %v, want %v: ready aliases of the live preview only", got, want)
	}
}

// TestWildcardDomainOnTLSRouteNotSeeded: a TLS-passthrough route serves its exact custom domains only (the pull's
// domain_bindings), so a wildcard domain attached to one is not announced as a *.suffix key.
func TestWildcardDomainOnTLSRouteNotSeeded(t *testing.T) {
	f := newAgentFixture(t, "holdtlswild")
	f.manage(t, f.node)
	session := f.carrier(t, f.node, "epoch_endpoint", true, "")
	tunnel, label := "tunnel_"+f.prefix, uuid.NewString()
	stable := label + ".tunnels.example.test"
	exactDomain, wildSuffix := "tls-"+f.prefix+".example.test", "wild-"+f.prefix+".example.test"
	f.exec(t, `INSERT INTO tunnels(id,account_id,name,desired_state,access_mode,generation,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id) VALUES($1,$2,$1,'active','public',1,$3,$4,$5,$2)`,
		tunnel, f.account, "endpoint_"+label, "https://"+stable, f.machine)
	route := "route_" + f.prefix
	f.exec(t, `INSERT INTO tunnel_routes(id,tunnel_id,name,priority,protocol,match_type,match_hostname,wildcard_suffix,origin_scheme,origin_address,tls_verification,generation,desired_state,created_by_actor_id,updated_by_actor_id,created_at,updated_at,public_tcp_listener_id,public_tcp_port,preserve_host)
VALUES($1,$2,$1,5,'tls','managed',$3,NULL,'tcp','127.0.0.1:1','not_applicable',1,'active',$4,$4,now(),now(),'listener_tls_443',443,false)`, route, tunnel, stable, f.account)
	f.exec(t, `INSERT INTO endpoints(id,kind,account_id,agent_machine_id,access_mode,lifetime,tunnel_id) VALUES($1,'tunnel',$2,$3,'public','persistent',$1)`, tunnel, f.account, f.machine)
	for id, domain := range map[string][2]string{"domain_exact_" + f.prefix: {exactDomain, "exact"}, "domain_wild_" + f.prefix: {"*." + wildSuffix, "one_label_wildcard"}} {
		f.exec(t, `INSERT INTO tunnel_domains(id,account_id,tunnel_id,route_id,hostname,match_type,ownership_challenge_reference,ownership_state,dns_target,certificate_strategy,certificate_state,caa_state,conflict_state) VALUES($1,$2,$3,$4,$5,$6,$1,'verified',$5,'none','not_applicable','ready','clear')`, id, f.account, tunnel, route, domain[0], domain[1])
	}
	f.exec(t, `INSERT INTO endpoint_routes(id,endpoint_id,hostname,protocol,tunnel_route_id) SELECT $1,$2,$3,protocol,id FROM tunnel_routes WHERE id=$1`, route, tunnel, stable)
	f.exec(t, `INSERT INTO endpoint_edge_assignments(route_id,endpoint_id,edge_node_id,edge_process_epoch,carrier_session_id,generation,observed_ready,observed_at) VALUES($1,$2,$3,'epoch_endpoint',$4,1,true,now())`, route, tunnel, f.node, session)
	holders, _, err := f.service.Holders(f.ctx, f.node, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, h := range holders {
		names = append(names, h.Name)
	}
	want := []string{stable, exactDomain}
	sort.Strings(want)
	if fmt.Sprint(names) != fmt.Sprint(want) {
		t.Fatalf("keys %v, want %v: the exact hostname and exact domain, and no *.suffix key for a wildcard domain on a TLS route", names, want)
	}
}

// explain logs the executed plan of one holder query (a failing page names where its time went).
func (f *agentFixture) explain(t *testing.T, query string, args ...any) {
	t.Helper()
	tx, err := f.database.SQL().BeginTx(f.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(f.ctx, `SET LOCAL statement_timeout='120s'`); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.QueryContext(f.ctx, "EXPLAIN (ANALYZE, BUFFERS) "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		t.Log(line)
	}
}
