package controlplane

import (
	"context"
	"fmt"
	"github.com/google/uuid"
	"strings"
	"testing"
	"time"
)

// The edge answers "machine offline" for a preview whose owner machine sleeps, so the snapshot lists exactly the
// active foreground previews of an offline, not removed, owner, or of an online owner whose carrier dropped (no
// live route on this edge yet before the 90 s offline mark); a served preview, a background preview, an ended lease
// and a removed owner stay out (they keep their 502/404 answers).
func TestOfflineOwnerPreviewsListOnlyActiveForegroundWithAnOfflineOwner(t *testing.T) {
	store := openControlPlaneTestDB(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	owner, node := "user_off_"+suffix, "edge_off_"+suffix
	now := time.Now().UTC().Truncate(time.Microsecond)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	machines := map[string]string{"asleep": "off_asleep_" + suffix, "online": "off_online_" + suffix, "revoked": "off_revoked_" + suffix, "dropped": "off_dropped_" + suffix}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = store.SQL().ExecContext(cleanup, `DELETE FROM paperboat.preview_domains WHERE account_id=$1`, owner)
		_, _ = store.SQL().ExecContext(cleanup, `DELETE FROM paperboat.preview_leases WHERE account_id=$1`, owner)
		_, _ = store.SQL().ExecContext(cleanup, `DELETE FROM paperboat.user_machines WHERE user_id=$1`, owner)
		_, _ = store.SQL().ExecContext(cleanup, `DELETE FROM paperboat.users WHERE id=$1`, owner)
		_, _ = store.SQL().ExecContext(cleanup, `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, node)
	})
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,email_verified,status) VALUES($1,$1,'owner@test.invalid',false,'active')`, owner)
	exec(`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at,public_ingress_verified_at) VALUES($1,'default','1.0','epoch','ready',true,now(),'region','domain-'||$1,ARRAY['edge']::text[],ARRAY['http3','http2'],100,0,now(),now()+interval '1 hour',now())`, node)
	for name, id := range machines {
		state, online := "offline", false
		if name == "online" || name == "dropped" {
			state, online = "online", true
		}
		exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,online,seat_state) VALUES($1,$2,$3,$6,'linux','amd64','/workspace',$4,$5,'occupied')`, id, owner, "env_"+id, state, online, strings.ReplaceAll(id, "_", "-"))
	}
	exec(`UPDATE paperboat.user_machines SET revoked_at=$2 WHERE id=$1`, machines["revoked"], now)
	lease := func(name, machine, kind, terminal string, userDeadline *time.Time) string {
		host := name + "-" + suffix + ".preview.example.test"
		var stopped *time.Time
		if terminal != "active" {
			stopped = &now
		}
		exec(`INSERT INTO paperboat.preview_leases(owner_session_kind,id,endpoint,account_id,actor_id,owner_machine_id,owner_session_id,target_scheme,target_address,access_mode,lease_deadline,user_deadline,allocation_state,edge_state,origin_state,terminal_state,stopped_at,created_at,last_renewed_at) VALUES($1,$2,$3,$4,$4,$5,$6,'http','127.0.0.1:3000','public',$7,$8,'ready','ready','ready',$9,$10,$11,$11)`,
			kind, "prv_"+name+"_"+suffix, "https://"+host, owner, machine, "session_"+name+"_"+suffix, now.Add(-time.Minute+time.Hour), userDeadline, terminal, stopped, now.Add(-2*time.Hour))
		return host
	}
	deadline := now.Add(time.Hour)
	want := lease("asleep", machines["asleep"], "foreground", "active", nil)
	wantLocal := lease("local", machines["asleep"], "local_lease", "active", nil)
	serving := lease("online", machines["online"], "foreground", "active", nil)
	// Online, but its carrier dropped (no live route on this edge): listed before the machine is marked offline.
	wantDropped := lease("dropped", machines["dropped"], "foreground", "active", nil)
	// The online owner's preview this edge serves (a live, observed route) stays out.
	exec(`INSERT INTO paperboat.endpoints(id,kind,account_id,agent_machine_id,access_mode,lifetime,preview_lease_id) VALUES($1,'preview',$2,$3,'public','lease',$1)`, "prv_online_"+suffix, owner, machines["online"])
	exec(`INSERT INTO paperboat.endpoint_routes(id,endpoint_id,hostname,protocol) VALUES($1,$2,$3,'http')`, "route_online_"+suffix, "prv_online_"+suffix, serving)
	exec(`INSERT INTO paperboat.endpoint_edge_assignments(route_id,endpoint_id,edge_node_id,edge_process_epoch,carrier_session_id,observed_ready,observed_at) VALUES($1,$2,$3,'epoch','session_online',true,now())`, "route_online_"+suffix, "prv_online_"+suffix, node)
	lease("background", machines["asleep"], "local_lease", "active", &deadline)
	lease("stopped", machines["asleep"], "foreground", "stopped", nil)
	lease("revoked", machines["revoked"], "foreground", "active", nil)

	ws := strings.ReplaceAll(uuid.NewString(), "-", "")
	exec(`INSERT INTO paperboat.workspace_domains(id,account_id,hostname,challenge_reference,ownership_state,created_by_actor_id) VALUES($1,$2,$3,'x','verified',$2)`, "wsd_"+ws, owner, ws+".example.test")
	exec(`INSERT INTO paperboat.preview_domains(id,account_id,preview_id,preview_generation,hostname,match_type,ownership_challenge_reference,ownership_state,dns_target,certificate_strategy,certificate_reference,certificate_state,caa_state,conflict_state,workspace_domain_id) VALUES($1,$2,$3,1,$4,'exact','dns:test','verified','edge.example.test','managed','cert_ref','ready','ready','clear',$5)`, "pd_off_"+suffix, owner, "prv_asleep_"+suffix, "custom-"+suffix+".example.test", "wsd_"+ws)
	wantCustom := "custom-" + suffix + ".example.test"
	got, err := offlineOwnerPreviews(ctx, store, node)
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, o := range got {
		if strings.Contains(o.Hostname, suffix) { // the shared test database may hold other tests' previews
			listed[o.Hostname] = true
		}
	}
	if len(listed) != 4 || !listed[want] || !listed[wantLocal] || !listed[wantCustom] || !listed[wantDropped] {
		t.Fatalf("offline owner previews = %v, want exactly %s, %s, %s and %s", got, want, wantLocal, wantCustom, wantDropped)
	}
}
