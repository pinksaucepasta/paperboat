package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/shared/tunnelrouterules"
)

// pb tunnel route add sends the rules the flags describe, normalized, in the route create body.
func TestTunnelRouteAddSendsRewriteRules(t *testing.T) {
	withTunnelCommandClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			tunnel := validCommandTunnel()
			w.Header().Set("ETag", tunnel.ETag)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": tunnel})
			return
		}
		var input api.TunnelRouteInput
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.RewriteRules == nil {
			t.Fatalf("create body: %v %+v", err, input)
		}
		rules := input.RewriteRules
		if len(rules.RequestHeaders) != 2 || rules.RequestHeaders[0] != (tunnelrouterules.HeaderRule{Op: "set", Name: "X-Env", Value: "prod"}) || rules.RequestHeaders[1].Op != "remove" ||
			len(rules.ResponseHeaders) != 1 || rules.PathRewrite == nil || rules.PathRewrite.From != "/api" || rules.PathRewrite.To != "/v1" ||
			len(rules.Redirects) != 1 || rules.Redirects[0] != (tunnelrouterules.Redirect{From: "/old", Code: 308, To: "/new"}) {
			t.Fatalf("rules = %+v", rules)
		}
		route := validCommandRoute("route_r1", "web")
		route.RewriteRules = rules
		_ = json.NewEncoder(w).Encode(map[string]any{"data": api.TunnelRouteMutation{Route: route, Operation: validCommandOperation("route", route.ID), Changed: true}})
	})
	command := tunnelCobraCommandV1()
	command.SetArgs([]string{"route", "add", "tun_1", "--name", "web", "--to", "http://127.0.0.1:3000",
		"--request-header", "set:x-env=prod", "--request-header", "remove:X-Debug", "--response-header", "add:X-Served-By=paperboat",
		"--rewrite-path", "/api/ -> /v1", "--redirect", "/old -> 308:/new", "--json"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
}

// pb tunnel route update replaces only the lists its flags name, keeps the rest of the route's current rules, and clears
// a list with its --clear flag; the patch carries the whole rules document.
func TestTunnelRouteUpdateReplacesAndClearsRuleLists(t *testing.T) {
	current := validCommandRoute("route_r1", "web")
	current.RewriteRules = &tunnelrouterules.Rules{
		RequestHeaders: []tunnelrouterules.HeaderRule{{Op: "set", Name: "X-Keep", Value: "1"}},
		Redirects:      []tunnelrouterules.Redirect{{From: "/old", Code: 301, To: "/new"}},
	}
	var sent map[string]any
	withTunnelCommandClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": api.TunnelRoutePage{Items: []api.TunnelRoute{current}}})
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": api.TunnelRouteMutation{Route: current, Operation: validCommandOperation("route", current.ID), Changed: true}})
	})
	command := tunnelCobraCommandV1()
	command.SetArgs([]string{"route", "update", "tun_1", "route_r1", "--response-header", "set:X-New=1", "--clear-redirects", "--json"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	rules, _ := sent["rewrite_rules"].(map[string]any)
	if rules == nil || rules["request_headers"] == nil || rules["response_headers"] == nil || rules["redirects"] != nil {
		t.Fatalf("patch rules = %#v", sent["rewrite_rules"])
	}
	// clearing the last rules sends an empty document (explicitly: the field is present)
	current.RewriteRules = &tunnelrouterules.Rules{Redirects: []tunnelrouterules.Redirect{{From: "/old", Code: 301, To: "/new"}}}
	command = tunnelCobraCommandV1()
	command.SetArgs([]string{"route", "update", "tun_1", "route_r1", "--clear-redirects", "--json"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if value, present := sent["rewrite_rules"]; !present || len(value.(map[string]any)) != 0 {
		t.Fatalf("clearing sent %#v", sent["rewrite_rules"])
	}
}

// What the edge owns, malformed rules, rules on a non-http route and conflicting flags are refused before any request.
func TestTunnelRouteRuleFlagsAreRefusedBeforeTheServerSeesThem(t *testing.T) {
	for name, args := range map[string][]string{
		"an edge header":          {"route", "add", "tun_1", "--name", "web", "--to", "http://127.0.0.1:3000", "--request-header", "set:X-Forwarded-For=1.2.3.4"},
		"a visitor assertion":     {"route", "add", "tun_1", "--name", "web", "--to", "http://127.0.0.1:3000", "--request-header", "remove:Paperboat-Visitor-Assertion"},
		"not op:Name=value":       {"route", "add", "tun_1", "--name", "web", "--to", "http://127.0.0.1:3000", "--request-header", "X-Env=prod"},
		"an http redirect target": {"route", "add", "tun_1", "--name", "web", "--to", "http://127.0.0.1:3000", "--redirect", "/a -> http://example.test/b"},
		"a bad redirect status":   {"route", "add", "tun_1", "--name", "web", "--to", "http://127.0.0.1:3000", "--redirect", "/a -> 303:/b"},
		"a tcp route":             {"route", "add", "tun_1", "--name", "db", "--protocol", "tcp_private", "--to", "tcp://127.0.0.1:5432", "--rewrite-path", "/a -> /b"},
		"a flag and its clear":    {"route", "update", "tun_1", "route_r1", "--redirect", "/a -> /b", "--clear-redirects"},
		"two path rewrites":       {"route", "add", "tun_1", "--name", "web", "--to", "http://127.0.0.1:3000", "--rewrite-path", "/a -> /b", "--rewrite-path", "/c -> /d"},
		"a redirect loop":         {"route", "add", "tun_1", "--name", "web", "--to", "http://127.0.0.1:3000", "--redirect", "/a -> /a/b"},
	} {
		command := tunnelCobraCommandV1()
		command.SetArgs(args)
		command.SetOut(&strings.Builder{})
		command.SetErr(&strings.Builder{})
		if err := command.Execute(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// --clear-redirects=false is "do not clear", not a clear: a script that passes the flag off must keep the route's rules.
func TestTunnelRouteUpdateClearFlagSetToFalseClearsNothing(t *testing.T) {
	current := validCommandRoute("route_r1", "web")
	current.RewriteRules = &tunnelrouterules.Rules{Redirects: []tunnelrouterules.Redirect{{From: "/old", Code: 301, To: "/new"}}}
	var sent map[string]any
	withTunnelCommandClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": api.TunnelRoutePage{Items: []api.TunnelRoute{current}}})
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": api.TunnelRouteMutation{Route: current, Operation: validCommandOperation("route", current.ID), Changed: true}})
	})
	command := tunnelCobraCommandV1()
	command.SetArgs([]string{"route", "update", "tun_1", "route_r1", "--response-header", "set:X-New=1", "--clear-redirects=false", "--json"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	rules, _ := sent["rewrite_rules"].(map[string]any)
	if rules == nil || rules["redirects"] == nil || rules["response_headers"] == nil {
		t.Fatalf("--clear-redirects=false changed the redirects: %#v", sent["rewrite_rules"])
	}
}
