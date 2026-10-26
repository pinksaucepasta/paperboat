package domaininventory

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/previewdomain"
	"github.com/pinksaucepasta/paperboat/server/internal/tunnelv1"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
)

// The Domains page lists the workspace's tunnel domains and its live previews' custom domains in hostname order,
// keyset paged with an exact total: deleted domains, ended previews' domains and other accounts' domains are
// not in it.
func TestWorkspaceDomainListOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	s := fmt.Sprint(time.Now().UnixNano())
	owner, other := "dom_owner_"+s, "dom_other_"+s
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active'),($2,$2,$2||'@example.test','active')`, owner, other)
	for _, account := range []string{owner, other} {
		host, tunnel, route := account+"_host", account+"_tunnel", account+"_route"
		exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,configured_capabilities,observed_capabilities)
VALUES($1,$2,$1,'host','linux','amd64','/w','online','occupied',true,ARRAY['preview_launch'],ARRAY['preview_launch'])`, host, account)
		exec(`INSERT INTO paperboat.tunnels(id,account_id,name,access_mode,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id)
VALUES($1,$2,'web','private',$3,$4,$5,$2)`, tunnel, account, "endpoint_"+uuid.NewString(), "https://t-"+strings.ReplaceAll(account, "_", "-")+".example.test", host)
		exec(`INSERT INTO paperboat.tunnel_routes(id,tunnel_id,name,protocol,match_type,origin_scheme,origin_address,generation,desired_state,created_by_actor_id,updated_by_actor_id,created_at,updated_at)
VALUES($1,$2,'web','http','catch_all','http','127.0.0.1:3000',1,'active',$3,$3,now(),now())`, route, tunnel, account)
	}
	tunnelDomain := func(id, account, hostname string) {
		exec(`INSERT INTO paperboat.tunnel_domains (id,account_id,tunnel_id,route_id,hostname,match_type,ownership_challenge_reference,dns_target,dns_provider,expected_records,created_at,updated_at,ownership_state,certificate_state)
VALUES ($1,$2,$2||'_tunnel',$2||'_route',$3,'exact','dns-challenge://test','stable.example.test','generic','[]'::jsonb,now(),now(),'pending','pending')`, id, account, hostname)
	}
	tunnelDomain("dom_b_"+s, owner, "b-"+s+".example.test")
	tunnelDomain("dom_a_"+s, owner, "a-"+s+".example.test")
	tunnelDomain("dom_gone_"+s, owner, "gone-"+s+".example.test")
	exec(`UPDATE paperboat.tunnel_domains SET deleted_at=now() WHERE id=$1`, "dom_gone_"+s)
	tunnelDomain("dom_other_"+s, other, "c-other-"+s+".example.test")
	preview := func(id, hostname string) {
		exec(`INSERT INTO paperboat.preview_leases(id,endpoint,account_id,actor_id,owner_machine_id,owner_session_id,target_scheme,target_address,access_mode,
lease_deadline,owner_session_kind) VALUES($1,$2,$3,$3,$3||'_host','session','http','127.0.0.1:3000','private',now()+interval '1 hour','local_lease')`, id, "https://"+id+".example.test", owner)
		exec(`INSERT INTO paperboat.preview_domains(id,account_id,preview_id,preview_generation,hostname,match_type,ownership_challenge_reference,ownership_state,dns_target,certificate_strategy,certificate_state,caa_state,conflict_state)
VALUES($1,$2,$3,1,$4,'exact','dns:placement','verified','edge.example.test','managed','issuing','ready','clear')`, "pdom_"+id, owner, id, hostname)
	}
	preview("prv-live-"+s, "c-"+s+".example.test")
	preview("prv-ended-"+s, "d-"+s+".example.test")
	exec(`UPDATE paperboat.preview_leases SET terminal_state='stopped',stopped_at=now() WHERE id=$1`, "prv-ended-"+s)

	service := New(store)
	personal := workspace.WithContext(ctx, workspace.Scope{ID: "personal"})
	hostnames := func(items []Item) []string {
		out := []string{}
		for _, item := range items {
			switch view := item.Domain.(type) {
			case tunnelv1.DomainView:
				out = append(out, "tunnel "+view.Hostname)
			case previewdomain.DomainView:
				out = append(out, "preview "+view.Hostname)
			default:
				t.Fatalf("item %+v has an unknown view", item)
			}
		}
		return out
	}
	want := []string{"tunnel a-" + s + ".example.test", "tunnel b-" + s + ".example.test", "preview c-" + s + ".example.test"}
	page, err := service.List(personal, owner, Options{Limit: 50, Q: s})
	if err != nil || page.Total != 3 || page.More || !slices.Equal(hostnames(page.Items), want) {
		t.Fatalf("domains: %v total %d more %v, %v; want %v", hostnames(page.Items), page.Total, page.More, err, want)
	}
	var walked []string
	opts := Options{Limit: 2, Q: s}
	for {
		page, err := service.List(personal, owner, opts)
		if err != nil || page.Total != 3 {
			t.Fatalf("page: total %d, %v; want 3 on every page", page.Total, err)
		}
		walked = append(walked, hostnames(page.Items)...)
		if !page.More {
			break
		}
		next := page.Next()
		opts.After = &next
	}
	if !slices.Equal(walked, want) {
		t.Fatalf("walking the cursor listed %v, want %v", walked, want)
	}
	for _, item := range page.Items {
		if (item.Kind == "tunnel" && item.OwnerName != "web") || (item.Kind == "preview" && item.Endpoint != "https://prv-live-"+s+".example.test") {
			t.Fatalf("item %s carries owner %q / endpoint %q", item.Kind, item.OwnerName, item.Endpoint)
		}
	}
	for _, c := range []struct {
		name string
		opts Options
		want []string
	}{
		{"previews only", Options{Limit: 50, Q: s, Kind: "preview"}, want[2:]},
		{"tunnels only", Options{Limit: 50, Q: s, Kind: "tunnel"}, want[:2]},
		{"hostname", Options{Limit: 50, Q: "b-" + s}, want[1:2]},
	} {
		page, err := service.List(personal, owner, c.opts)
		if err != nil || page.Total != len(c.want) || !slices.Equal(hostnames(page.Items), c.want) {
			t.Errorf("%s: %v total %d, %v; want %v", c.name, hostnames(page.Items), page.Total, err, c.want)
		}
	}
	if page, err := service.List(personal, other, Options{Limit: 50, Q: s}); err != nil || page.Total != 1 {
		t.Fatalf("the other account sees %v (total %d), %v; want only its own domain", hostnames(page.Items), page.Total, err)
	}
}
